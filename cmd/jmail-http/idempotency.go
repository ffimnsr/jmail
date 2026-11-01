package main

import (
	"sync"
	"time"
)

// idempotency remembers what a successful request returned so a client retry
// with the same Idempotency-Key replays the response instead of sending twice.
type idempotency struct {
	mu      sync.Mutex
	entries map[string]*idemEntry
	ttl     time.Duration
	max     int
}

type idemEntry struct {
	done    chan struct{}
	resp    *cachedResponse
	expires time.Time
}

type cachedResponse struct {
	status int
	body   []byte
}

func newIdempotency(ttl time.Duration, max int) *idempotency {
	if max <= 0 {
		max = 1024
	}
	return &idempotency{entries: make(map[string]*idemEntry), ttl: ttl, max: max}
}

// begin reserves the key. The first caller for a key owns the work; a concurrent
// duplicate gets the existing entry and must wait for it. An empty key is not
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

	entry := &idemEntry{done: make(chan struct{}), expires: now.Add(i.ttl)}
	i.entries[key] = entry
	return entry, true
}

// finish publishes the response and wakes any waiter.
func (i *idempotency) finish(entry *idemEntry, resp cachedResponse) {
	if entry == nil {
		return
	}
	entry.resp = &resp
	close(entry.done)
}

// abort releases a failed attempt so a retry is allowed to send again.
func (i *idempotency) abort(entry *idemEntry) {
	if entry == nil {
		return
	}
	i.mu.Lock()
	for key, candidate := range i.entries {
		if candidate == entry {
			delete(i.entries, key)
			break
		}
	}
	i.mu.Unlock()
	close(entry.done)
}

func (e *idemEntry) wait() *cachedResponse {
	<-e.done
	return e.resp
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
