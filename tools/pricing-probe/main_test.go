package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const tescoPage = `<ul><li><a href="/groceries/en-GB/products/123">Milk 2L</a> £1.50 £0.75/litre</li></ul>`

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		prices int
		want   string
	}{
		{200, "x", 2, classOK},
		{200, "captcha script", 1, classOK},
		{403, "Access Denied", 0, classBlocked},
		{429, "", 0, classBlocked},
		{200, "Pardon the interruption", 0, classBlocked},
		{200, "<html></html>", 0, classEmpty},
		{500, "oops", 0, classError},
		{404, "", 0, classError},
	}
	for _, c := range cases {
		if got := classify(c.status, c.body, c.prices); got != c.want {
			t.Errorf("classify(%d,%q,%d)=%s want %s", c.status, c.body, c.prices, got, c.want)
		}
	}
}

type fakes struct {
	cfg config
	mu  sync.Mutex
	uas []string
}

func newFakes(t *testing.T) *fakes {
	t.Helper()
	f := &fakes{}
	tesco := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		f.mu.Lock()
		f.uas = append(f.uas, ua)
		f.mu.Unlock()
		switch {
		case r.URL.Path == "/robots.txt":
			w.Write([]byte("User-agent: *\nDisallow: /search"))
		case strings.Contains(ua, "Mozilla"):
			w.Write([]byte(tescoPage))
		default:
			http.Error(w, "Access Denied", http.StatusForbidden)
		}
	}))
	off := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"products":[{"product_name":"Milk"},{"product_name":"Oat"}]}`))
	}))
	prices := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[{"price":1.2}]}`))
	}))
	t.Cleanup(func() { tesco.Close(); off.Close(); prices.Close() })
	f.cfg = config{items: []string{"milk", "eggs"}, maxRequests: 100, tescoURL: tesco.URL, offURL: off.URL, pricesURL: prices.URL}
	return f
}

func TestRunClassifiesEachSource(t *testing.T) {
	f := newFakes(t)
	rep := run(context.Background(), f.cfg, http.DefaultClient)

	got := map[string]string{}
	for _, r := range rep.Results {
		got[r.Source+"/"+r.Option+"/"+r.Item] = r.Classification
	}
	want := map[string]string{
		"tesco/honest-ua/milk":            classBlocked,
		"tesco/browser-headers/eggs":      classOK,
		"tesco/robots.txt/":               classInfo,
		"open-food-facts/search/milk":     classEmpty,
		"open-prices/prices-by-name/eggs": classOK,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if rep.Requests != 9 || len(rep.Results) != 9 || rep.Truncated {
		t.Errorf("requests=%d results=%d truncated=%v", rep.Requests, len(rep.Results), rep.Truncated)
	}
	for _, ua := range f.uas { // Tesco only ever sees the two existing identities
		if ua != tescoHonestUA && ua != tescoBrowserUA {
			t.Errorf("unexpected Tesco UA %q", ua)
		}
	}
}

func TestRunHonoursRequestCapAndDelay(t *testing.T) {
	f := newFakes(t)
	f.cfg.maxRequests = 3
	f.cfg.delay = 20 * time.Millisecond
	start := time.Now()
	rep := run(context.Background(), f.cfg, http.DefaultClient)
	if rep.Requests != 3 || !rep.Truncated || len(rep.Results) != 3 {
		t.Fatalf("requests=%d truncated=%v results=%d", rep.Requests, rep.Truncated, len(rep.Results))
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Errorf("expected two delays of 20ms, took %v", time.Since(start))
	}
}

func TestTransportErrorIsError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	rep := run(context.Background(), config{items: []string{"milk"}, maxRequests: 1, tescoURL: base}, http.DefaultClient)
	if r := rep.Results[0]; r.Classification != classError || r.Error == "" {
		t.Errorf("got %+v", r)
	}
}

func TestMatrixAndReport(t *testing.T) {
	f := newFakes(t)
	rep := run(context.Background(), f.cfg, http.DefaultClient)

	var buf bytes.Buffer
	writeMatrix(&buf, rep)
	out := buf.String()
	for _, s := range []string{"SOURCE/OPTION", "milk", "eggs", "tesco/honest-ua", "403 blocked", "tesco/browser-headers", "200 ok", "tesco/robots.txt", "200 info"} {
		if !strings.Contains(out, s) {
			t.Errorf("matrix missing %q:\n%s", s, out)
		}
	}

	path := filepath.Join(t.TempDir(), "report.json")
	if err := writeReport(path, rep); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var back Report
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Results) != len(rep.Results) || back.MaxRequests != 100 || back.Items[0] != "milk" {
		t.Errorf("round trip mismatch: %+v", back)
	}
}

func TestParsers(t *testing.T) {
	if n, _ := parseTesco([]byte(tescoPage)); n != 1 {
		t.Errorf("tesco tiles = %d", n)
	}
	if n, _ := parseTesco([]byte("<html>nothing</html>")); n != 0 {
		t.Errorf("tesco empty = %d", n)
	}
	if n, _ := parseOFF([]byte(`{"products":[{"price":1.5},{}]}`)); n != 1 {
		t.Errorf("off priced = %d", n)
	}
	if n, _ := parseOpenPrices([]byte(`{"items":[{"price":0},{"price":2}]}`)); n != 1 {
		t.Errorf("open prices = %d", n)
	}
	if n, note := parseOpenPrices([]byte(`nope`)); n != 0 || note == "" {
		t.Errorf("bad json: %d %q", n, note)
	}
}
