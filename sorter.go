// Background sorting: a note added as typed while smart add was unavailable (at night, offline,
// LLM slow) is flagged needs_sort by the client. Once the LLM answers again, this files it under
// the right tab and, if it has none, a due date. It never renames, merges or deletes items, and it
// never runs inside a request.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

const (
	sortEvery    = 30 * time.Second
	sortTimeout  = 20 * time.Second
	sortBatch    = 5
	maxSortTries = 3
)

type sorter struct {
	store *store
	llm   *llmClient
	kick  chan struct{}
	tries map[string]int // item id -> rejected answers; used by the run goroutine only
}

func newSorter(st *store, l *llmClient) *sorter {
	return &sorter{store: st, llm: l, kick: make(chan struct{}, 1), tries: map[string]int{}}
}

// Kick asks for a pass soon; nil-safe and never blocks.
func (s *sorter) Kick() {
	if s == nil {
		return
	}
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *sorter) run() {
	t := time.NewTicker(sortEvery)
	for {
		select {
		case <-t.C:
		case <-s.kick:
		}
		s.pass()
	}
}

// pass sorts up to sortBatch flagged items and stops at the first sign the LLM is down.
func (s *sorter) pass() {
	for _, it := range s.store.NeedsSort(sortBatch) {
		if !s.llm.Available() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), sortTimeout)
		tab, due, err := s.llm.Classify(ctx, it.Title, time.UnixMilli(it.UpdatedAt))
		cancel()
		if errors.Is(err, errLLMDown) {
			log.Printf("llm: sort item=%s: %v", it.ID, err)
			return
		}
		if err != nil {
			s.tries[it.ID]++
			log.Printf("llm: sort item=%s try=%d: %v", it.ID, s.tries[it.ID], err)
			if s.tries[it.ID] < maxSortTries {
				continue
			}
			tab, due = "", "" // give up: leave the item where the person put it
		}
		delete(s.tries, it.ID)
		ok, err := s.store.FinishSort(it, tab, due)
		if err != nil {
			log.Printf("llm: sort item=%s: save: %v", it.ID, err)
		} else if ok {
			log.Printf("llm: sorted item=%s tab=%q due=%q", it.ID, tab, due)
		}
	}
}

// Classify picks a tab and a due date for one existing item with the quick-add prompt. Its answer
// is used only if every item the model splits the title into agrees on the tab.
func (l *llmClient) Classify(ctx context.Context, title string, written time.Time) (tab, due string, err error) {
	got, err := l.split(ctx, title, written)
	if err != nil {
		return "", "", err
	}
	tab, due = got[0].Tab, got[0].TargetDate
	for _, it := range got[1:] {
		if it.Tab != tab {
			return "", "", fmt.Errorf("%w: mixed tabs", errLLMOutput)
		}
		if it.TargetDate != due {
			due = ""
		}
	}
	return tab, due, nil
}
