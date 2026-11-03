// Purchase history: one event each time a Shopping item is ticked, kept so houss can later learn
// buying patterns. It lives next to houss.json in history.json, append-only apart from undo and
// pruning, and is written by the same atomic save as the items.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"time"
)

const (
	// undoWindow: unticking within this long after a tick takes the purchase back (a mis-tap).
	undoWindow = 10 * time.Minute
	// historyKeep bounds growth: events older than this are dropped.
	historyKeep = 2 * 365 * 24 * time.Hour
	historyMax  = 20000
)

type Purchase struct {
	ID       string `json:"id"`
	ItemID   string `json:"item_id"`
	Title    string `json:"title"`
	Tab      string `json:"tab"`
	BoughtAt int64  `json:"bought_at"` // unix milliseconds: the tick's updated_at (client clock)
	// Backfilled marks events made once from items already ticked when history started.
	Backfilled bool `json:"backfilled,omitempty"`
}

func newEventID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// loadHistory reads history.json; when it does not exist yet it backfills one event per ticked
// Shopping item (deleted ones too: ticked then cleared), dated by the item's last change.
func (s *store) loadHistory() error {
	b, err := os.ReadFile(s.hpath)
	if err == nil {
		return json.Unmarshal(b, &s.history)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, it := range s.items {
		if it.Tab == "shopping" && it.Done {
			s.history = append(s.history, Purchase{ID: newEventID(), ItemID: it.ID, Title: it.Title, Tab: it.Tab, BoughtAt: it.UpdatedAt, Backfilled: true})
		}
	}
	sort.Slice(s.history, func(i, j int) bool { return s.history[i].BoughtAt < s.history[j].BoughtAt })
	s.hdirty = true
	return nil
}

// recordLocked notes a purchase when a Shopping item goes from not done to done, and takes the
// latest one back when it is unticked within undoWindow. prev is the stored version, if any.
func (s *store) recordLocked(prev Item, existed bool, it Item) {
	wasDone := existed && prev.Done && !prev.Deleted
	switch {
	case it.Done && !wasDone && !it.Deleted && it.Tab == "shopping":
		// Offline replays can arrive out of order: keep the slice sorted by BoughtAt.
		e := Purchase{ID: newEventID(), ItemID: it.ID, Title: it.Title, Tab: it.Tab, BoughtAt: it.UpdatedAt}
		i := sort.Search(len(s.history), func(i int) bool { return s.history[i].BoughtAt > e.BoughtAt })
		s.history = append(s.history, Purchase{})
		copy(s.history[i+1:], s.history[i:])
		s.history[i] = e
		s.pruneLocked(time.Now().UnixMilli())
		s.hdirty = true
	case wasDone && !it.Done && !it.Deleted:
		for i := len(s.history) - 1; i >= 0; i-- {
			e := s.history[i]
			if e.ItemID != it.ID || e.Backfilled {
				continue
			}
			if d := it.UpdatedAt - e.BoughtAt; d >= 0 && d <= undoWindow.Milliseconds() {
				s.history = append(s.history[:i], s.history[i+1:]...)
				s.hdirty = true
			}
			break
		}
	}
}

func (s *store) pruneLocked(now int64) {
	cut := now - historyKeep.Milliseconds()
	i := 0
	for i < len(s.history) && s.history[i].BoughtAt < cut {
		i++
	}
	if n := len(s.history) - i; n > historyMax {
		i += n - historyMax
	}
	if i > 0 {
		s.history = append([]Purchase(nil), s.history[i:]...)
	}
}

func (s *store) saveHistoryLocked() error {
	if s.hpath == "" || !s.hdirty {
		return nil
	}
	b, err := json.Marshal(s.history)
	if err != nil {
		return err
	}
	tmp := s.hpath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.hpath); err != nil {
		return err
	}
	s.hdirty = false
	return nil
}

// History returns purchases bought at or after since (ms), newest first, at most limit.
func (s *store) History(since int64, limit int) []Purchase {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Purchase{}
	for i := len(s.history) - 1; i >= 0 && len(out) < limit; i-- {
		if s.history[i].BoughtAt >= since {
			out = append(out, s.history[i])
		}
	}
	return out
}
