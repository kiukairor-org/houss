package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeCollector records OTLP/HTTP JSON trace exports.
func fakeCollector(t *testing.T) (*httptest.Server, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var spans []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected export %s %q", r.URL.Path, r.Header.Get("Content-Type"))
		}
		b, _ := io.ReadAll(r.Body)
		var body struct {
			ResourceSpans []struct {
				Resource   map[string]any `json:"resource"`
				ScopeSpans []struct {
					Spans []map[string]any `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := json.Unmarshal(b, &body); err != nil {
			t.Errorf("invalid OTLP JSON: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, rs := range body.ResourceSpans {
			if !strings.Contains(string(b), `"service.name"`) {
				t.Errorf("missing service.name resource attribute")
			}
			for _, ss := range rs.ScopeSpans {
				spans = append(spans, ss.Spans...)
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []map[string]any { mu.Lock(); defer mu.Unlock(); return spans }
}

func attr(span map[string]any, key string) map[string]any {
	for _, a := range span["attributes"].([]any) {
		m := a.(map[string]any)
		if m["key"] == key {
			return m["value"].(map[string]any)
		}
	}
	return nil
}

func TestTracingExportsServerSpans(t *testing.T) {
	col, got := fakeCollector(t)
	tr := newTracer(col.URL, "demo")
	mux := newMux()
	mux.HandleFunc("GET /fail", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "x"})
	})
	h := tr.middleware(mux)
	for _, p := range []string{"/version", "/fail", "/nope", "/healthz"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, p, nil))
	}
	tr.flush()
	spans := got()
	if len(spans) != 3 { // /healthz is not traced
		t.Fatalf("got %d spans, want 3: %v", len(spans), spans)
	}
	byName := map[string]map[string]any{}
	for _, s := range spans {
		byName[s["name"].(string)] = s
		if len(s["traceId"].(string)) != 32 || len(s["spanId"].(string)) != 16 || s["kind"].(float64) != 2 {
			t.Fatalf("bad ids/kind: %v", s)
		}
	}
	ok, fail, miss := byName["GET /version"], byName["GET /fail"], byName["GET (unmatched)"]
	if ok == nil || fail == nil || miss == nil {
		t.Fatalf("unexpected span names: %v", byName)
	}
	if attr(ok, "http.response.status_code")["intValue"] != "200" || attr(fail, "http.response.status_code")["intValue"] != "500" {
		t.Fatalf("status codes not recorded")
	}
	if st := fail["status"].(map[string]any); st["code"].(float64) != 2 {
		t.Fatalf("5xx span not marked as error: %v", st)
	}
	if st := ok["status"].(map[string]any); len(st) != 0 {
		t.Fatalf("2xx span should have unset status: %v", st)
	}
}

func TestTracingOffWithoutEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	tr := setupTracing()
	if tr != nil {
		t.Fatal("tracer should be nil without an endpoint")
	}
	mux := newMux()
	if h := tr.middleware(mux); h != http.Handler(mux) {
		t.Fatal("nil tracer must return the mux unchanged")
	}
}
