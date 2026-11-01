package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// idemStore is an append-only file of settled idempotency records. It exists so a
// process restart does not turn a client retry into a second delivery: records are
// written before the response is sent and replayed on start.
//
// The file is plain JSON lines rather than a database because the access pattern
// is a single writer appending small records and the volume is bounded by the
// in-memory entry cap. A record torn by a crash fails to parse and is skipped,
// which costs at most one replay.
type idemStore struct {
	mu      sync.Mutex
	path    string
	file    *os.File
	records int
}

type idemRecord struct {
	Key     string          `json:"key"`
	Status  int             `json:"status"`
	Body    json.RawMessage `json:"body"`
	Expires int64           `json:"expires"`
}

func openIdemStore(dir string) (*idemStore, error) {
	if dir == "" {
		return nil, errors.New("no directory configured")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	path := filepath.Join(dir, "idempotency.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &idemStore{path: path, file: file}, nil
}

// load returns every parseable record. A partial trailing line, which is what a
// crash during append leaves behind, is ignored.
func (s *idemStore) load() (map[string]idemRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	records := make(map[string]idemRecord)

	file, err := os.Open(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return records, nil
		}
		return nil, err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			var record idemRecord
			if json.Unmarshal(line, &record) == nil && record.Key != "" {
				// A later record for the same key wins, which is what a reused key
				// after expiry leaves behind.
				records[record.Key] = record
			}
		}
		if readErr != nil {
			break
		}
	}

	s.records = len(records)
	return records, nil
}

func (s *idemStore) append(record idemRecord) error {
	line, err := json.Marshal(record)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.file == nil {
		return errors.New("store is closed")
	}
	if _, err := s.file.Write(line); err != nil {
		return err
	}
	s.records++
	return nil
}

// wantsCompaction reports whether the append-only file has drifted far enough
// past the in-memory cap to be worth rewriting.
func (s *idemStore) wantsCompaction(max int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.records > 2*max
}

func (s *idemStore) compactWith(live func() map[string]idemRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// The callback may lock the idempotency mutex, never the other way round, so
	// holding the store lock here cannot deadlock and no concurrent append is
	// dropped by the rewrite.
	records := live()

	temporary := s.path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}

	writer := bufio.NewWriter(file)
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			file.Close()
			os.Remove(temporary)
			return err
		}
		if _, err := writer.Write(append(line, '\n')); err != nil {
			file.Close()
			os.Remove(temporary)
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		file.Close()
		os.Remove(temporary)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(temporary)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, s.path); err != nil {
		os.Remove(temporary)
		return err
	}

	reopened, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("reopen compacted store: %w", err)
	}
	if s.file != nil {
		old := s.file
		s.file = reopened
		s.records = len(records)
		return old.Close()
	}
	s.file = reopened
	s.records = len(records)
	return nil
}

func (s *idemStore) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}
