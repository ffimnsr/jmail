package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T, dir string) *idemStore {
	t.Helper()
	store, err := openIdemStore(dir)
	if err != nil {
		t.Fatalf("openIdemStore: %v", err)
	}
	t.Cleanup(func() { _ = store.close() })
	return store
}

func TestIdempotencySurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	first := newIdempotency(time.Hour, 16, openTestStore(t, dir))
	entry, _ := first.begin("restart-key", time.Now())
	first.finish(entry, cachedResponse{status: 202, body: []byte(`{"id":"ABC123"}`)})

	// A second instance over the same directory stands in for a restarted process.
	second := newIdempotency(time.Hour, 16, openTestStore(t, dir))

	duplicate, owner := second.begin("restart-key", time.Now())
	if owner {
		t.Fatal("the key should have been loaded from disk, not re-owned")
	}
	cached := duplicate.wait(context.Background())
	if cached == nil {
		t.Fatal("expected a replayed response")
	}
	if cached.status != 202 || string(cached.body) != `{"id":"ABC123"}` {
		t.Fatalf("replayed %d %s", cached.status, cached.body)
	}
}

func TestIdempotencyAbortIsNotPersisted(t *testing.T) {
	dir := t.TempDir()

	first := newIdempotency(time.Hour, 16, openTestStore(t, dir))
	entry, _ := first.begin("failed-key", time.Now())
	first.abort(entry)

	second := newIdempotency(time.Hour, 16, openTestStore(t, dir))
	if _, owner := second.begin("failed-key", time.Now()); !owner {
		t.Fatal("a failed attempt must not be replayable after a restart")
	}
}

func TestIdempotencyDropsExpiredRecordsOnLoad(t *testing.T) {
	dir := t.TempDir()
	store := openTestStore(t, dir)

	expired := idemRecord{Key: "old", Status: 202, Body: []byte(`{}`), Expires: time.Now().Add(-time.Minute).Unix()}
	if err := store.append(expired); err != nil {
		t.Fatalf("append: %v", err)
	}

	loaded := newIdempotency(time.Hour, 16, store)
	loaded.mu.Lock()
	_, present := loaded.entries["old"]
	loaded.mu.Unlock()
	if present {
		t.Fatal("an expired record should not be loaded")
	}

	// Compaction on load should leave the file without the expired record.
	data, err := os.ReadFile(filepath.Join(dir, "idempotency.jsonl"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("expected an empty file after compaction, got %q", data)
	}
}

func TestIdempotencySkipsTornRecord(t *testing.T) {
	dir := t.TempDir()

	first := newIdempotency(time.Hour, 16, openTestStore(t, dir))
	entry, _ := first.begin("good", time.Now())
	first.finish(entry, cachedResponse{status: 202, body: []byte(`{"id":"G"}`)})

	// Simulate a crash in the middle of an append.
	file, err := os.OpenFile(filepath.Join(dir, "idempotency.jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := file.WriteString(`{"key":"trunc`); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second := newIdempotency(time.Hour, 16, openTestStore(t, dir))
	if _, owner := second.begin("good", time.Now()); owner {
		t.Fatal("the intact record should still replay")
	}
	if _, owner := second.begin("trunc", time.Now()); !owner {
		t.Fatal("the torn record should be ignored")
	}
}

func TestIdempotencyCompactsWhenTheFileGrows(t *testing.T) {
	dir := t.TempDir()
	store := openTestStore(t, dir)
	limiter := newIdempotency(time.Hour, 4, store)

	for i := 0; i < 40; i++ {
		key := string(rune('a' + i%26))
		entry, owner := limiter.begin(key, time.Now())
		if !owner {
			continue
		}
		limiter.finish(entry, cachedResponse{status: 202, body: []byte(`{}`)})
	}

	store.mu.Lock()
	records := store.records
	store.mu.Unlock()
	if records > 2*4 {
		t.Fatalf("file still holds %d records, expected compaction below %d", records, 2*4)
	}
}
