package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func apply(t *testing.T, s *store, it Item) {
	t.Helper()
	if _, err := s.Apply(it); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryRecordsShoppingTicks(t *testing.T) {
	s, _ := openStore(t.TempDir())
	t0 := time.Now().Add(-time.Hour).UnixMilli()
	apply(t, s, Item{ID: "m", Title: "Milk", Tab: "shopping", UpdatedAt: t0})
	apply(t, s, Item{ID: "p", Title: "call plumber", Tab: "todos", UpdatedAt: t0})
	apply(t, s, Item{ID: "m", Title: "Milk", Tab: "shopping", Done: true, UpdatedAt: t0 + 1000})
	apply(t, s, Item{ID: "p", Title: "call plumber", Tab: "todos", Done: true, UpdatedAt: t0 + 1000})
	// Editing a ticked item again is not another purchase.
	apply(t, s, Item{ID: "m", Title: "Milk 2L", Tab: "shopping", Done: true, UpdatedAt: t0 + 2000})
	h := s.History(0, 100)
	if len(h) != 1 || h[0].ItemID != "m" || h[0].Title != "Milk" || h[0].BoughtAt != t0+1000 || h[0].Backfilled {
		t.Fatalf("history = %+v", h)
	}
}

func TestHistoryUndoWindow(t *testing.T) {
	s, _ := openStore(t.TempDir())
	t0 := time.Now().Add(-48 * time.Hour).UnixMilli()
	tick := func(done bool, at int64) {
		apply(t, s, Item{ID: "e", Title: "eggs", Tab: "shopping", Done: done, UpdatedAt: at})
	}
	tick(false, t0)
	tick(true, t0+1000)
	tick(false, t0+1000+int64(5*time.Minute/time.Millisecond)) // mis-tap: taken back
	if h := s.History(0, 100); len(h) != 0 {
		t.Fatalf("undo kept %+v", h)
	}
	tick(true, t0+int64(time.Hour/time.Millisecond))
	tick(false, t0+int64(2*time.Hour/time.Millisecond)) // re-listed an hour later: a real purchase
	tick(true, t0+int64(26*time.Hour/time.Millisecond))
	if h := s.History(0, 100); len(h) != 2 || h[0].BoughtAt <= h[1].BoughtAt {
		t.Fatalf("history = %+v", h)
	}
}

func TestHistoryIgnoresStaleChanges(t *testing.T) {
	s, _ := openStore(t.TempDir())
	apply(t, s, Item{ID: "b", Title: "bread", Tab: "shopping", UpdatedAt: 2000})
	apply(t, s, Item{ID: "b", Title: "bread", Tab: "shopping", Done: true, UpdatedAt: 1000}) // older than stored
	if h := s.History(0, 100); len(h) != 0 {
		t.Fatalf("stale tick recorded: %+v", h)
	}
}

func TestHistoryPersistsAndBackfillsOnce(t *testing.T) {
	dir := t.TempDir()
	// An items file from before history existed: one ticked, one ticked then cleared, one open.
	t0 := time.Now().Add(-72 * time.Hour).UnixMilli()
	old := []Item{
		{ID: "a", Title: "Skyr", Tab: "shopping", Done: true, UpdatedAt: t0 + 1000},
		{ID: "b", Title: "Lessive", Tab: "shopping", Done: true, Deleted: true, UpdatedAt: t0 + 2000},
		{ID: "c", Title: "Milk", Tab: "shopping", UpdatedAt: t0 + 3000},
		{ID: "d", Title: "dentist", Tab: "todos", Done: true, UpdatedAt: t0 + 4000},
	}
	b, _ := json.Marshal(old)
	if err := os.WriteFile(filepath.Join(dir, "houss.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := s.History(0, 100)
	if len(h) != 2 || !h[0].Backfilled || h[0].Title != "Lessive" || h[1].Title != "Skyr" {
		t.Fatalf("backfill = %+v", h)
	}
	apply(t, s, Item{ID: "c", Title: "Milk", Tab: "shopping", Done: true, UpdatedAt: time.Now().UnixMilli()})

	s2, err := openStore(dir) // reload: no second backfill, the new tick is kept
	if err != nil {
		t.Fatal(err)
	}
	if h := s2.History(0, 100); len(h) != 3 || h[0].Title != "Milk" || h[0].Backfilled {
		t.Fatalf("after reload = %+v", h)
	}
}

func TestHistoryPrunesOldEvents(t *testing.T) {
	s, _ := openStore("")
	now := time.Now()
	s.history = []Purchase{{ID: "x", ItemID: "x", Title: "old", Tab: "shopping", BoughtAt: now.Add(-3 * 365 * 24 * time.Hour).UnixMilli()}}
	apply(t, s, Item{ID: "n", Title: "new", Tab: "shopping", Done: true, UpdatedAt: now.UnixMilli()})
	if h := s.History(0, 100); len(h) != 1 || h[0].Title != "new" {
		t.Fatalf("history = %+v", h)
	}
}

func TestHistoryEndpoint(t *testing.T) {
	s := testServer(t)
	h := newMux(s)
	if rec := do(t, h, "GET", "/api/history", "", nil); rec.Code != 401 {
		t.Fatalf("no cookie: %d", rec.Code)
	}
	c := login(t, h)
	now := time.Now().UnixMilli()
	do(t, h, "POST", "/api/sync", `{"changes":[{"id":"m","title":"Milk","tab":"shopping","updated_at":`+itoa(now-2000)+`}]}`, c)
	do(t, h, "POST", "/api/sync", `{"changes":[{"id":"m","title":"Milk","tab":"shopping","done":true,"updated_at":`+itoa(now-1000)+`}]}`, c)
	rec := do(t, h, "GET", "/api/history?since="+itoa(now-5000), "", c)
	var got []Purchase
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 || len(got) != 1 || got[0].Title != "Milk" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/api/history?since="+itoa(now), "", c); rec.Body.String() != "[]\n" {
		t.Fatalf("since filter: %s", rec.Body)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
