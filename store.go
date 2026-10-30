// Item storage: an in-memory map persisted atomically to a JSON file on the data volume.
// (The standard library has no SQLite driver and CLAUDE.md forbids new dependencies.)
// Deletes are kept as tombstones so last-write-wins also covers delete vs. edit.
package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var tabs = map[string]bool{"shopping": true, "todos": true, "midterm": true}

type Item struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Tab        string `json:"tab"`
	Done       bool   `json:"done"`
	TargetDate string `json:"target_date"` // YYYY-MM-DD or ""
	UpdatedAt  int64  `json:"updated_at"`  // unix milliseconds; the last-write-wins clock
	Deleted    bool   `json:"deleted,omitempty"`
}

var errInvalid = errors.New("invalid item")

func (it *Item) normalize() error {
	it.Title = strings.TrimSpace(it.Title)
	if it.ID == "" || len(it.ID) > 64 || it.Title == "" || len(it.Title) > 200 || !tabs[it.Tab] {
		return errInvalid
	}
	if it.TargetDate != "" {
		if _, err := time.Parse("2006-01-02", it.TargetDate); err != nil {
			return errInvalid
		}
	}
	return nil
}

type store struct {
	mu    sync.Mutex
	path  string // "" = memory only
	items map[string]Item
}

func openStore(dir string) (*store, error) {
	s := &store{items: map[string]Item{}}
	if dir == "" {
		return s, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s.path = filepath.Join(dir, "houss.json")
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var all []Item
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	for _, it := range all {
		s.items[it.ID] = it
	}
	return s, nil
}

// saveLocked writes via temp file + rename so a crash never leaves a torn file.
func (s *store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	all := make([]Item, 0, len(s.items))
	for _, it := range s.items {
		all = append(all, it)
	}
	b, err := json.Marshal(all)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// applyLocked applies one change if it is not older than what is stored.
func (s *store) applyLocked(it Item) (Item, error) {
	if err := it.normalize(); err != nil {
		return Item{}, err
	}
	if it.UpdatedAt <= 0 {
		it.UpdatedAt = time.Now().UnixMilli()
	}
	if cur, ok := s.items[it.ID]; ok && it.UpdatedAt < cur.UpdatedAt {
		return cur, nil
	}
	s.items[it.ID] = it
	return it, nil
}

// Apply upserts changes (including tombstones) and persists once.
func (s *store) Apply(changes ...Item) ([]Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range changes {
		c := it
		if err := c.normalize(); err != nil {
			return nil, err
		}
	}
	out := make([]Item, 0, len(changes))
	for _, it := range changes {
		res, err := s.applyLocked(it)
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, s.saveLocked()
}

// Delete tombstones an existing item (offline deletes arrive through Apply with Deleted set).
func (s *store) Delete(id string, ts int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.items[id]
	if !ok {
		return os.ErrNotExist
	}
	if ts <= 0 {
		ts = time.Now().UnixMilli()
	}
	if ts < cur.UpdatedAt {
		ts = cur.UpdatedAt
	}
	cur.Deleted, cur.UpdatedAt = true, ts
	s.items[id] = cur
	return s.saveLocked()
}

func (s *store) Get(id string) (Item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	return it, ok && !it.Deleted
}

// List returns live items, optionally for one tab, in a stable order.
func (s *store) List(tab string) []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Item{}
	for _, it := range s.items {
		if !it.Deleted && (tab == "" || it.Tab == tab) {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}
