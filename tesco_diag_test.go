package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func tescoDiagServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("query") {
		case "forbidden":
			w.Header().Set("Server", "AkamaiGHost")
			http.SetCookie(w, &http.Cookie{Name: "_abck", Value: "x"})
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("nope"))
		case "limited":
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		case "challenge":
			_, _ = w.Write([]byte("<html><title>Pardon the interruption</title></html>"))
		case "captcha-ok": // valid tiles plus an incidental marker word
			_, _ = w.Write([]byte(tescoFixture + "<script src=\"/recaptcha.js\">captcha</script>"))
		case "empty":
			_, _ = w.Write([]byte("<html><body><ul></ul></body></html>"))
		case "ua": // blocks the default identity only
			if r.UserAgent() == tescoUserAgent {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(tescoFixture))
		default:
			_, _ = w.Write([]byte(tescoFixture))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTescoClassification(t *testing.T) {
	srv := tescoDiagServer(t)
	tp := &tescoProvider{baseURL: srv.URL, client: srv.Client()}
	cases := []struct {
		q, class string
		status   int
		tiles    int
	}{
		{"forbidden", classBlocked, 403, 0},
		{"limited", classBlocked, 429, 0},
		{"challenge", classBlocked, 200, 0},
		{"empty", classParseEmpty, 200, 0},
		{"eggs", classOK, 200, 5},
		{"captcha-ok", classOK, 200, 5},
	}
	for _, c := range cases {
		res, err := tp.Diagnose(context.Background(), c.q, false)
		if err != nil || len(res) != 1 {
			t.Fatalf("%s: %v %v", c.q, res, err)
		}
		if d := res[0]; d.Classification != c.class || d.Status != c.status || d.Tiles != c.tiles {
			t.Fatalf("%s: got %+v", c.q, d)
		}
	}
	d, _ := tp.Diagnose(context.Background(), "forbidden", false)
	if d[0].Headers["Server"] != "AkamaiGHost" || len(d[0].CookieNames) != 1 || d[0].CookieNames[0] != "_abck" ||
		d[0].Snippet != "nope" || d[0].BodySize != 4 {
		t.Fatalf("headers/cookies/snippet: %+v", d[0])
	}
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(tescoFixture + "<script src=\"/recaptcha.js\">captcha</script>"))
	}))
	defer okSrv.Close()
	tp2 := &tescoProvider{baseURL: okSrv.URL, client: okSrv.Client()}
	if _, err := tp2.Lookup(context.Background(), "large eggs"); err != nil {
		t.Fatalf("page with tiles must still yield a price: %v", err)
	}
	// Lookup fails for every non-ok class, and the failures stay distinguishable.
	if _, err := tp.Lookup(context.Background(), "empty"); err == nil || !strings.Contains(err.Error(), classParseEmpty) {
		t.Fatalf("got %v", err)
	}
	if _, err := tp.Lookup(context.Background(), "challenge"); err == nil || !strings.Contains(err.Error(), classBlocked) {
		t.Fatalf("got %v", err)
	}
}

func TestTescoClassificationSpanAttribute(t *testing.T) {
	srv := tescoDiagServer(t)
	col, got := fakeCollector(t)
	tr := newTracer(col.URL, "demo")
	tp := &tescoProvider{baseURL: srv.URL, client: srv.Client()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /lookup", func(w http.ResponseWriter, r *http.Request) {
		_, _ = tp.Lookup(r.Context(), "forbidden")
		w.WriteHeader(http.StatusNoContent)
	})
	tr.middleware(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/lookup", nil))
	tr.flush()
	spans := got()
	if len(spans) != 1 || attr(spans[0], "tesco.classification")["stringValue"] != classBlocked {
		t.Fatalf("got %v", spans)
	}
}

func TestTescoSnippetTruncated(t *testing.T) {
	if got := snippet(strings.Repeat("é", 1000), 300); len([]rune(got)) != 300 {
		t.Fatalf("got %d runes", len([]rune(got)))
	}
}

func TestDebugTescoEndpoint(t *testing.T) {
	srv := tescoDiagServer(t)
	s := testServer(t)
	s.tesco = &tescoProvider{baseURL: srv.URL, client: srv.Client()}
	h := newMux(s)
	if rec := do(t, h, "GET", "/api/debug/tesco?q=ua", "", nil); rec.Code != 401 {
		t.Fatalf("unauthenticated: %d", rec.Code)
	}
	c := login(t, h)
	if rec := do(t, h, "GET", "/api/debug/tesco", "", c); rec.Code != 400 {
		t.Fatalf("missing q: %d", rec.Code)
	}
	type resp struct {
		Query   string      `json:"query"`
		Results []tescoDiag `json:"results"`
	}
	var r resp
	rec := do(t, h, "GET", "/api/debug/tesco?q=ua", "", c)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("got %d %v", rec.Code, rec.Header())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	if len(r.Results) != 1 || r.Results[0].Profile != "default" || r.Results[0].Classification != classBlocked {
		t.Fatalf("got %+v", r)
	}
	r = resp{}
	rec = do(t, h, "GET", "/api/debug/tesco?q=ua&compare=1", "", c)
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	if len(r.Results) != 2 || r.Results[0].Profile != "default" || r.Results[0].Classification != classBlocked ||
		r.Results[1].Profile != "browser" || r.Results[1].Classification != classOK || r.Results[1].Tiles != 5 {
		t.Fatalf("got %+v", r)
	}
}
