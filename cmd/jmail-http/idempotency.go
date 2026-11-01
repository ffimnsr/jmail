package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"
)

// idempotency remembers what a successful request returned so a client retry
// with the same Idempotency-Key replays the response instead of sending twice.
// A store makes the records survive a restart.
type idempotency struct {
	mu      sync.Mutex
	entries map[string]*idemEntry
	ttl     time.Duration
	max     int
	store   *idemStore
}

type idemEntry struct {
	key string

	// settled runs exactly once, so a panic unwind and a normal completion
	// cannot both close done and take the process down with a double close.
	settled sync.Once
	done    chan struct{}
	resp    *cachedResponse
	expires time.Time
}

type cachedResponse struct {
	status int
	body   []byte
}

func newIdempotency(ttl time.Duration, max int, store *idemStore) *idempotency {
	if max <= 0 {
		max = 1024
	}
	store_ := &idempotency{entries: make(map[string]*idemEntry), ttl: ttl, max: max, store: store}
	if store == nil {
		return store_
	}

	records, err := store.load()
	if err != nil {
		slog.Error("could not read idempotency records, retries will not be deduplicated across this restart", "error", err)
		return store_
	}

	now := time.Now()
	live := make(map[string]idemRecord, len(records))
	for key, record := range records {
		expires := time.Unix(record.Expires, 0)
		if !now.Before(expires) {
			continue
		}

		live[key] = record
		entry := &idemEntry{key: key, done: make(chan struct{}), expires: expires}
		response := &cachedResponse{status: record.Status, body: append([]byte(nil), record.Body...)}
		entry.settled.Do(func() {
			entry.resp = response
			close(entry.done)
		})
		store_.entries[key] = entry
	}

	// Drop expired and torn records so the file does not grow across restarts.
	if err := store.compactWith(func() map[string]idemRecord { return live }); err != nil {
		slog.Error("could not compact idempotency records", "error", err)
	}
	slog.Info("loaded idempotency records", "records", len(live), "ttl", ttl.String())
	return store_
}

// begin reserves the key. The first caller for a key owns the work; a concurrent
// duplicate gets the existing entry and waits for it. An empty key is not
// deduplicated at all.
func (i *idempotency) begin(key string, now time.Time) (*idemEntry, bool) {
	if key == "" {
		return nil, true
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	if entry, ok := i.entries[key]; ok && now.Before(entry.expires) {
		return entry, false
	}

	// Storing a result must not grow without bound if a client sends unique keys.
	if len(i.entries) >= i.max {
		i.sweepLocked(now)
		if len(i.entries) >= i.max {
			i.dropOldestLocked()
		}
	}

	entry := &idemEntry{
		key:     key,
		done:    make(chan struct{}),
		expires: now.Add(i.ttl),
	}
	i.entries[key] = entry
	return entry, true
}

// finish publishes the response and wakes any waiter. The record is written
// before the caller can see the response, so a crash right after the reply still
// leaves a replayable record behind.
func (i *idempotency) finish(entry *idemEntry, resp cachedResponse) {
	if entry == nil {
		return
	}
	i.persist(entry, resp)
	entry.settle(&resp)
}

func (i *idempotency) persist(entry *idemEntry, resp cachedResponse) {
	if i.store == nil {
		return
	}

	record := idemRecord{
		Key:     entry.key,
		Status:  resp.status,
		Body:    append(json.RawMessage(nil), resp.body...),
		Expires: entry.expires.Unix(),
	}
	if err := i.store.append(record); err != nil {
		// The message is already queued; losing the bookkeeping must not turn a
		// successful send into an error.
		slog.Error("could not persist idempotency record", "error", err)
		return
	}
	if !i.store.wantsCompaction(i.max) {
		return
	}

	err := i.store.compactWith(func() map[string]idemRecord {
		i.mu.Lock()
		defer i.mu.Unlock()

		live := make(map[string]idemRecord, len(i.entries))
		for key, e := range i.entries {
			if key == entry.key {
				// Not settled yet, so it is not visible through e.resp.
				live[key] = record
				continue
			}
			if e.resp == nil {
				continue
			}
			live[key] = idemRecord{
				Key:     key,
				Status:  e.resp.status,
				Body:    append(json.RawMessage(nil), e.resp.body...),
				Expires: e.expires.Unix(),
			}
		}
		return live
	})
	if err != nil {
		slog.Error("could not compact idempotency records", "error", err)
	}
}

// abort releases a failed attempt so a retry is allowed to send again.
func (i *idempotency) abort(entry *idemEntry) {
	if entry == nil {
		return
	}

	i.mu.Lock()
	if current, ok := i.entries[entry.key]; ok && current == entry {
		delete(i.entries, entry.key)
	}
	i.mu.Unlock()

	entry.settle(nil)
}

func (e *idemEntry) settle(resp *cachedResponse) {
	e.settled.Do(func() {
		e.resp = resp
		close(e.done)
	})
}

// wait blocks until the owner settles, or the caller gives up. Returning nil
// means the key was released by a failed attempt, or ctx ended first; the caller
// distinguishes those by checking ctx.
func (e *idemEntry) wait(ctx context.Context) *cachedResponse {
	select {
	case <-e.done:
		return e.resp
	case <-ctx.Done():
		return nil
	}
}

func (i *idempotency) sweepLocked(now time.Time) {
	for key, entry := range i.entries {
		select {
		case <-entry.done:
			if !now.Before(entry.expires) {
				delete(i.entries, key)
			}
		default:
			// Still in flight; leaving it alone keeps the duplicate in sync.
		}
	}
}

func (i *idempotency) dropOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for key, entry := range i.entries {
		select {
		case <-entry.done:
			if oldestKey == "" || entry.expires.Before(oldest) {
				oldestKey, oldest = key, entry.expires
			}
		default:
		}
	}
	if oldestKey != "" {
		delete(i.entries, oldestKey)
	}
}
