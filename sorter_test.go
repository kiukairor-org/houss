package main

import (
	"net/http"
	"testing"
)

func flagged(t *testing.T, st *store, id, title, tab, due string) Item {
	t.Helper()
	res, err := st.Apply(Item{ID: id, Title: title, Tab: tab, TargetDate: due, NeedsSort: true, UpdatedAt: monday.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	return res[0]
}

func TestSorterFilesFlaggedItems(t *testing.T) {
	st, _ := openStore(t.TempDir())
	l, _ := fakeLLM(t, answer(`{"items":[{"title":"call the plumber","list":"todos","when":"friday"}]}`))
	flagged(t, st, "a", "call the plumber friday", "shopping", "")
	flagged(t, st, "b", "call the plumber friday", "shopping", "2030-01-01")
	_, _ = st.Apply(Item{ID: "c", Title: "milk", Tab: "shopping"}) // not flagged: never sent
	newSorter(st, l).pass()

	a, _ := st.Get("a")
	if a.Tab != "todos" || a.TargetDate != "2026-11-06" || a.NeedsSort || a.Title != "call the plumber friday" || a.UpdatedAt <= monday.UnixMilli() {
		t.Fatalf("a: %+v", a)
	}
	if b, _ := st.Get("b"); b.Tab != "todos" || b.TargetDate != "2030-01-01" {
		t.Fatalf("a date the person chose must stay: %+v", b)
	}
	if c, _ := st.Get("c"); c.Tab != "shopping" || c.NeedsSort {
		t.Fatalf("c: %+v", c)
	}
}

func TestSorterLeavesItemsWhenLLMFails(t *testing.T) {
	st, _ := openStore(t.TempDir())
	// Down: nothing changes, the flag stays for a later pass.
	down, _ := fakeLLM(t, answer(`{}`))
	down.url = "http://127.0.0.1:1"
	flagged(t, st, "a", "milk, call mum", "shopping", "")
	newSorter(st, down).pass()
	if a, _ := st.Get("a"); a.Tab != "shopping" || !a.NeedsSort {
		t.Fatalf("down: %+v", a)
	}
	// Disagreeing or garbage answers: retried, then given up without moving the item.
	for _, reply := range []string{
		`{"items":[{"title":"milk","list":"shopping","when":""},{"title":"call mum","list":"todos","when":""}]}`,
		`not json`,
	} {
		st, _ := openStore(t.TempDir())
		l, reqs := fakeLLM(t, answer(reply))
		flagged(t, st, "a", "milk, call mum", "shopping", "")
		s := newSorter(st, l)
		for i := 0; i < maxSortTries; i++ {
			s.pass()
		}
		a, _ := st.Get("a")
		if a.Tab != "shopping" || a.NeedsSort || a.Title != "milk, call mum" || len(*reqs) != maxSortTries {
			t.Fatalf("%s: %+v after %d calls", reply, a, len(*reqs))
		}
	}
}

func TestFinishSortKeepsNewerEdits(t *testing.T) {
	st, _ := openStore(t.TempDir())
	read := flagged(t, st, "a", "plumber", "shopping", "")
	edited := read
	edited.Title, edited.UpdatedAt = "call the plumber", read.UpdatedAt+5
	if _, err := st.Apply(edited); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.FinishSort(read, "todos", "2026-11-06"); ok || err != nil {
		t.Fatalf("stale sort applied: %v %v", ok, err)
	}
	if a, _ := st.Get("a"); a.Title != "call the plumber" || a.Tab != "shopping" || !a.NeedsSort {
		t.Fatalf("%+v", a)
	}
}

func TestCreateKicksSorter(t *testing.T) {
	s := testServer(t)
	h := newMux(s)
	c := login(t, h)
	s.sorter = newSorter(s.store, nil)
	rec := do(t, h, "POST", "/api/items", `{"title":"call the plumber","tab":"shopping","needs_sort":true}`, c)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	select {
	case <-s.sorter.kick:
	default:
		t.Fatal("saving a flagged item must wake the sorter")
	}
	if got := listItems(t, h, "/api/items", c); len(got) != 1 || !got[0].NeedsSort {
		t.Fatalf("flag not stored: %+v", got)
	}
}
