package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Fixture shaped like Tesco search results; no live network is used.
const tescoFixture = `<html><body><ul>
<li class="tile"><a href="/groceries/en-GB/products/111">Tesco Free Range Eggs 6 Pack</a>
  <p>£1.45</p><p>£0.24/each</p></li>
<li class="tile"><a href="/groceries/en-GB/products/222"><span>Happy Eggs Free Range Large Eggs 12 Pack</span></a>
  <p>£2.40</p><p>£0.20/each</p></li>
<li class="tile"><a href="/groceries/en-GB/products/333">Cadbury Creme Egg 5 Pack</a>
  <p>£1.00</p><p>£0.20/each</p></li>
<li class="tile"><a href="/groceries/en-GB/products/444">Tesco Egg Mayonnaise Sandwich</a>
  <p>£1.20</p></li>
<li class="tile"><a href="/groceries/en-GB/products/555">Tesco Large Eggs 15 Pack</a>
  <p>£2.85</p><p>£0.19/each</p></li>
</ul></body></html>`

func TestParseTescoResults(t *testing.T) {
	got := parseTescoResults(tescoFixture, "https://t.test")
	if len(got) != 5 {
		t.Fatalf("got %d tiles", len(got))
	}
	p := got[1]
	if p.Product != "Happy Eggs Free Range Large Eggs 12 Pack" || p.Price != 2.40 || p.UnitPrice != 0.20 ||
		p.UnitLabel != "each" || p.URL != "https://t.test/groceries/en-GB/products/222" {
		t.Fatalf("bad parse: %+v", p)
	}
}

func TestPickCheapestUsesUnitPriceAndRelevance(t *testing.T) {
	cands := parseTescoResults(tescoFixture, "https://t.test")
	// Both are "each"-priced, so the 12 pack wins on unit price despite the higher shelf price.
	p, err := pickCheapest("free range eggs", cands)
	if err != nil || p.Product != "Happy Eggs Free Range Large Eggs 12 Pack" {
		t.Fatalf("want 12 pack by unit price, got %+v %v", p, err)
	}
	p, err = pickCheapest("large eggs", cands)
	if err != nil || p.Product != "Tesco Large Eggs 15 Pack" {
		t.Fatalf("want 15 pack by unit price, got %+v %v", p, err)
	}
	if _, err := pickCheapest("anchovies", cands); !errors.Is(err, errNoMatch) {
		t.Fatalf("want errNoMatch, got %v", err)
	}
}

func TestPickCheapestFallsBackToShelfPrice(t *testing.T) {
	p, err := pickCheapest("milk", []Price{
		{Product: "Whole Milk", Price: 1.50, UnitPrice: 0.26, UnitLabel: "litre"},
		{Product: "Semi Milk", Price: 1.10},
	})
	if err != nil || p.Product != "Semi Milk" {
		t.Fatalf("got %+v %v", p, err)
	}
}

func TestTescoProviderLookup(t *testing.T) {
	var ua atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua.Store(r.UserAgent())
		if r.URL.Query().Get("query") == "blocked" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(tescoFixture))
	}))
	defer srv.Close()
	tp := &tescoProvider{baseURL: srv.URL, client: srv.Client()}
	p, err := tp.Lookup(context.Background(), "large eggs")
	if err != nil || p.Store != "Tesco" || p.Price != 2.85 || p.FetchedAt.IsZero() {
		t.Fatalf("got %+v %v", p, err)
	}
	if ua.Load() != tescoUserAgent {
		t.Fatalf("user agent %v", ua.Load())
	}
	if _, err := tp.Lookup(context.Background(), "blocked"); err == nil {
		t.Fatal("blocked lookup should fail")
	}
	if _, err := tp.Lookup(context.Background(), "anchovies"); !errors.Is(err, errNoMatch) {
		t.Fatalf("want errNoMatch, got %v", err)
	}
}

type fakeProvider struct {
	name  string
	price Price
	err   error
	calls atomic.Int32
	panic bool
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Lookup(ctx context.Context, item string) (Price, error) {
	f.calls.Add(1)
	if f.panic {
		panic("boom")
	}
	return f.price, f.err
}

func syncService(ps ...PriceProvider) *priceService {
	s := newPriceService(ps...)
	s.background = func(f func()) { f() }
	return s
}

func TestPriceServiceCachesWithinWindow(t *testing.T) {
	fp := &fakeProvider{name: "A", price: Price{Store: "A", Product: "Eggs", Price: 2}}
	s := syncService(fp)
	now := time.Now()
	s.now = func() time.Time { return now }
	// First view: nothing cached yet, so pending; the lookup runs in the background.
	if v := s.Get("Eggs"); v.Status != "pending" {
		t.Fatalf("first view: %+v", v)
	}
	for i := 0; i < 3; i++ {
		if v := s.Get(" eggs "); v.Status != "ok" || v.Price.Price != 2 {
			t.Fatalf("view %d: %+v", i, v)
		}
	}
	if n := fp.calls.Load(); n != 1 {
		t.Fatalf("provider called %d times within cache window", n)
	}
	now = now.Add(13 * time.Hour)
	if v := s.Get("eggs"); v.Status != "ok" { // stale data is still shown while refreshing
		t.Fatalf("stale view: %+v", v)
	}
	if n := fp.calls.Load(); n != 2 {
		t.Fatalf("want refresh after expiry, calls=%d", n)
	}
}

func TestPriceServiceNeverBlocksAndDedupes(t *testing.T) {
	var started int
	s := newPriceService(&fakeProvider{name: "A", price: Price{Price: 1}})
	s.background = func(f func()) { started++ } // lookup never completes
	for i := 0; i < 3; i++ {
		if v := s.Get("eggs"); v.Status != "pending" {
			t.Fatalf("got %+v", v)
		}
	}
	if started != 1 {
		t.Fatalf("started %d refreshes for one key", started)
	}
}

func TestPriceServiceIsolatesProviderFailures(t *testing.T) {
	bad := &fakeProvider{name: "bad", err: errors.New("blocked")}
	boom := &fakeProvider{name: "boom", panic: true}
	cheap := &fakeProvider{name: "cheap", price: Price{Store: "cheap", Product: "Eggs", Price: 1.5}}
	dear := &fakeProvider{name: "dear", price: Price{Store: "dear", Product: "Eggs", Price: 2.5}}
	s := syncService(bad, boom, dear, cheap)
	s.Get("eggs")
	if v := s.Get("eggs"); v.Status != "ok" || v.Store != "cheap" {
		t.Fatalf("want cheapest healthy provider, got %+v", v)
	}

	s = syncService(bad, &fakeProvider{name: "none", err: errNoMatch})
	s.Get("eggs")
	if v := s.Get("eggs"); v.Status != "none" {
		t.Fatalf("want none, got %+v", v)
	}
}

func TestPriceServiceKeepsLastKnownOnFailedRefresh(t *testing.T) {
	fp := &fakeProvider{name: "A", price: Price{Store: "A", Product: "Eggs", Price: 2}}
	s := syncService(fp)
	now := time.Now()
	s.now = func() time.Time { return now }
	s.Get("eggs")
	now = now.Add(13 * time.Hour)
	fp.err = errors.New("blocked")
	s.Get("eggs") // triggers failing refresh
	if v := s.Get("eggs"); v.Status != "ok" || v.Price.Price != 2 {
		t.Fatalf("last known price lost: %+v", v)
	}
	if n := fp.calls.Load(); n != 2 {
		t.Fatalf("failed refresh retried immediately, calls=%d", n)
	}
}

func TestPricesEndpoint(t *testing.T) {
	s := testServer(t)
	fp := &fakeProvider{name: "A", price: Price{Store: "A", Product: "Free Range Eggs", Price: 2, URL: "https://x/e"}}
	s.prices = syncService(fp)
	h := newMux(s)
	c := login(t, h)
	do(t, h, "POST", "/api/items", `{"title":"eggs","tab":"shopping"}`, c)
	do(t, h, "POST", "/api/items", `{"title":"bread","tab":"shopping","done":true}`, c)
	do(t, h, "POST", "/api/items", `{"title":"call mum","tab":"todos"}`, c)

	if rec := do(t, h, "GET", "/api/prices", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", rec.Code)
	}
	type resp struct {
		Prices map[string]PriceView `json:"prices"`
		Total  float64              `json:"total"`
		Priced int                  `json:"priced"`
	}
	get := func() resp {
		rec := do(t, h, "GET", "/api/prices", "", c)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("status %d", rec.Code)
		}
		var r resp
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	get() // starts lookups
	r := get()
	if len(r.Prices) != 2 {
		t.Fatalf("only shopping items get prices: %+v", r.Prices)
	}
	// "bread" is done, so it's excluded from the total even though it has a price.
	if r.Total != 2 || r.Priced != 1 {
		t.Fatalf("total=%v priced=%d", r.Total, r.Priced)
	}
	if n := fp.calls.Load(); n != 2 {
		t.Fatalf("repeated views re-fetched: %d calls", n)
	}
	for _, v := range r.Prices {
		if v.Status != "ok" || v.Product != "Free Range Eggs" || v.URL != "https://x/e" {
			t.Fatalf("bad view %+v", v)
		}
	}
}
