package main

import (
	"context"
	"testing"
	"time"
)

func TestIdempotencyReleasesKeyAfterAbort(t *testing.T) {
	store := newIdempotency(time.Minute, 8, nil)
	now := time.Now()

	entry, owner := store.begin("key-1", now)
	if !owner {
		t.Fatal("first begin should own the key")
	}

	store.abort(entry)

	second, owner := store.begin("key-1", now)
	if !owner {
		t.Fatal("the key should be free again after a failed attempt")
	}
	if second == entry {
		t.Fatal("a released key should get a fresh entry")
	}
}

func TestIdempotencyReplaysFinishedResponse(t *testing.T) {
	store := newIdempotency(time.Minute, 8, nil)
	now := time.Now()

	entry, _ := store.begin("key-2", now)
	store.finish(entry, cachedResponse{status: 202, body: []byte(`{"id":"A"}`)})

	duplicate, owner := store.begin("key-2", now)
	if owner {
		t.Fatal("a finished key should be a replay, not an owner")
	}
	cached := duplicate.wait(context.Background())
	if cached == nil {
		t.Fatal("expected the cached response")
	}
	if cached.status != 202 || string(cached.body) != `{"id":"A"}` {
		t.Fatalf("cached = %d %s", cached.status, cached.body)
	}
}

// A panic unwind aborts, and the deferred abort can also run after finish. That
// used to close the done channel twice, which panics in a goroutine and kills
// the process.
func TestIdempotencySettleIsIdempotent(t *testing.T) {
	store := newIdempotency(time.Minute, 8, nil)

	entry, _ := store.begin("key-3", time.Now())
	store.finish(entry, cachedResponse{status: 202})
	store.abort(entry)
	store.abort(entry)
	store.finish(entry, cachedResponse{status: 500})
}

func TestIdempotencyWaitHonorsContext(t *testing.T) {
	store := newIdempotency(time.Minute, 8, nil)
	entry, _ := store.begin("key-4", time.Now())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	if cached := entry.wait(ctx); cached != nil {
		t.Fatal("expected a nil result when the caller gives up")
	}
}

func TestIdempotencyEmptyKeyIsNotDeduplicated(t *testing.T) {
	store := newIdempotency(time.Minute, 8, nil)

	for i := 0; i < 3; i++ {
		entry, owner := store.begin("", time.Now())
		if !owner {
			t.Fatal("an empty key must never deduplicate")
		}
		if entry != nil {
			t.Fatal("an empty key should not allocate an entry")
		}
	}
}

func TestIdempotencyExpiresEntries(t *testing.T) {
	store := newIdempotency(time.Minute, 8, nil)
	start := time.Now()

	entry, _ := store.begin("key-5", start)
	store.finish(entry, cachedResponse{status: 202})

	if _, owner := store.begin("key-5", start.Add(2*time.Minute)); !owner {
		t.Fatal("an expired entry should not be replayed")
	}
}
