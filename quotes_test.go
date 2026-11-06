package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestQuoteList(t *testing.T) {
	if len(quotes) < 30 {
		t.Fatalf("want at least 30 quotes, got %d", len(quotes))
	}
	for i, q := range quotes {
		if q.Text == "" || q.Artist == "" || q.Track == "" {
			t.Errorf("quote %d incomplete: %+v", i, q)
		}
		if len(q.Text) > 120 {
			t.Errorf("quote %d too long: %q", i, q.Text)
		}
	}
}

func TestQuoteEndpoint(t *testing.T) {
	h := newMux(testServer(t))
	if rec := do(t, h, "GET", "/api/quote", "", nil); rec.Code != 401 {
		t.Fatalf("unauthenticated: %d", rec.Code)
	}
	c := login(t, h)
	prev := ""
	for i := 0; i < 100; i++ {
		rec := do(t, h, "GET", "/api/quote", "", c)
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("quote: %d %q", rec.Code, rec.Header().Get("Content-Type"))
		}
		var q quote
		if err := json.Unmarshal(rec.Body.Bytes(), &q); err != nil || q.Text == "" || q.Artist == "" || q.Track == "" {
			t.Fatalf("bad body %s: %v", rec.Body, err)
		}
		if q.Text == prev {
			t.Fatalf("repeated quote twice in a row: %q", q.Text)
		}
		prev = q.Text
	}
}

func TestQuoteUIWired(t *testing.T) {
	html := do(t, newMux(testServer(t)), "GET", "/", "", nil).Body.String()
	for _, want := range []string{`id="quote-open"`, `aria-label="French rap quote"`, `id="quote-close"`} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %s", want)
		}
	}
	if strings.Index(html, `id="chat-open"`) > strings.Index(html, `id="quote-open"`) {
		t.Error("music button should follow the chat button")
	}
}
