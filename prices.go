// Price lookups for shopping items. Stores sit behind PriceProvider so more can be added later;
// the cache and API never mention a specific store.
package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// Price is one store's best match for an item.
type Price struct {
	Store     string    `json:"store"`
	Product   string    `json:"product"`
	Price     float64   `json:"price"`                // pounds
	UnitPrice float64   `json:"unit_price,omitempty"` // pounds per UnitLabel
	UnitLabel string    `json:"unit_label,omitempty"` // e.g. "100g", "kg", "each"
	URL       string    `json:"url"`
	FetchedAt time.Time `json:"fetched_at"`
}

var errNoMatch = errors.New("no matching product")

type PriceProvider interface {
	Name() string
	Lookup(ctx context.Context, item string) (Price, error)
}

const (
	priceTTL   = 12 * time.Hour // fresh window for a successful lookup
	missTTL    = 1 * time.Hour  // failures and non-matches are retried sooner, but not hammered
	lookupTime = 20 * time.Second
)

type priceEntry struct {
	price     *Price // nil = last lookup found nothing
	checkedAt time.Time
}

type priceService struct {
	providers []PriceProvider
	now       func() time.Time
	// background runs a refresh; tests replace it to run synchronously.
	background func(func())

	mu       sync.Mutex
	cache    map[string]priceEntry
	inflight map[string]bool
}

func newPriceService(providers ...PriceProvider) *priceService {
	return &priceService{
		providers:  providers,
		now:        time.Now,
		background: func(f func()) { go f() },
		cache:      map[string]priceEntry{},
		inflight:   map[string]bool{},
	}
}

func priceKey(title string) string { return strings.Join(strings.Fields(strings.ToLower(title)), " ") }

// PriceView is what the API returns per item. Status is "ok", "none" or "pending".
type PriceView struct {
	Status string `json:"status"`
	*Price
}

// Get returns the last known price for title without ever waiting on a provider.
// A missing or expired entry schedules a background refresh; expired data is still returned.
func (s *priceService) Get(title string) PriceView {
	key := priceKey(title)
	s.mu.Lock()
	e, ok := s.cache[key]
	ttl := priceTTL
	if ok && e.price == nil {
		ttl = missTTL
	}
	stale := !ok || s.now().Sub(e.checkedAt) >= ttl
	start := stale && !s.inflight[key]
	if start {
		s.inflight[key] = true
	}
	s.mu.Unlock()
	if start {
		s.background(func() { s.refresh(key) })
	}
	switch {
	case !ok:
		return PriceView{Status: "pending"}
	case e.price == nil:
		return PriceView{Status: "none"}
	}
	return PriceView{Status: "ok", Price: e.price}
}

// refresh queries every provider (a failing one is skipped) and caches the cheapest result.
func (s *priceService) refresh(key string) {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTime)
	defer cancel()
	var best *Price
	for _, p := range s.providers {
		got, err := safeLookup(ctx, p, key)
		if err != nil {
			continue
		}
		if best == nil || got.Price < best.Price {
			g := got
			best = &g
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, key)
	if old, ok := s.cache[key]; best == nil && ok && old.price != nil {
		// Keep the last known price (and its age) when a refresh fails; retry after missTTL.
		old.checkedAt = s.now().Add(missTTL - priceTTL)
		s.cache[key] = old
		return
	}
	s.cache[key] = priceEntry{price: best, checkedAt: s.now()}
}

func safeLookup(ctx context.Context, p PriceProvider, item string) (pr Price, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.New("provider panic")
		}
	}()
	return p.Lookup(ctx, item)
}

// pickCheapest keeps candidates whose name contains every word of the query, then returns the
// cheapest: by unit price when all relevant candidates share a unit, otherwise by shelf price.
func pickCheapest(query string, cands []Price) (Price, error) {
	words := queryWords(query)
	if len(words) == 0 {
		return Price{}, errNoMatch
	}
	var rel []Price
	for _, c := range cands {
		name := " " + strings.Join(queryWords(c.Product), " ") + " "
		ok := c.Price > 0
		for _, w := range words {
			if !strings.Contains(name, " "+w+" ") {
				ok = false
				break
			}
		}
		if ok {
			rel = append(rel, c)
		}
	}
	if len(rel) == 0 {
		return Price{}, errNoMatch
	}
	byUnit := rel[0].UnitPrice > 0 && rel[0].UnitLabel != ""
	for _, c := range rel {
		if c.UnitPrice <= 0 || c.UnitLabel != rel[0].UnitLabel {
			byUnit = false
		}
	}
	best := rel[0]
	for _, c := range rel[1:] {
		if (byUnit && c.UnitPrice < best.UnitPrice) || (!byUnit && c.Price < best.Price) {
			best = c
		}
	}
	return best, nil
}

// queryWords lower-cases, strips punctuation and singularises a trailing "s" so "eggs" matches "egg".
func queryWords(s string) []string {
	f := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	for i, w := range f {
		if len(w) > 3 && strings.HasSuffix(w, "s") {
			f[i] = w[:len(w)-1]
		}
	}
	return f
}
