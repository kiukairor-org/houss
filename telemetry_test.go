package main

import (
	"context"
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
	mux := newMux(testServer(t))
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
	mux := newMux(testServer(t))
	if h := tr.middleware(mux); h != http.Handler(mux) {
		t.Fatal("nil tracer must return the mux unchanged")
	}
}

func TestLLMCallsAreGenAIClientSpansInsideTheRequest(t *testing.T) {
	col, got := fakeCollector(t)
	tr := newTracer(col.URL, "demo")
	appTracer = tr
	t.Cleanup(func() { appTracer = nil })
	l, _ := fakeLLM(t, func(w http.ResponseWriter) {
		writeJSON(w, http.StatusOK, map[string]any{"model": "test-model", "prompt_eval_count": 42, "eval_count": 7,
			"message": map[string]string{"role": "assistant", "content": `{"ok":true}`}})
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ask", func(w http.ResponseWriter, r *http.Request) {
		var out map[string]any
		if err := l.chat(r.Context(), []llmMsg{{"user", "hi"}}, nil, &out); err != nil {
			t.Errorf("chat: %v", err)
		}
	})
	tr.middleware(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ask", nil))
	_ = l.chat(context.Background(), []llmMsg{{"user", "background"}}, nil, &map[string]any{}) // no request: own trace
	tr.flush()
	var server, child, bg map[string]any
	for _, s := range got() {
		switch {
		case s["kind"].(float64) == 2:
			server = s
		case s["parentSpanId"] != nil:
			child = s
		default:
			bg = s
		}
	}
	if server == nil || child == nil || bg == nil {
		t.Fatalf("want server, child and background spans: %v", got())
	}
	if child["traceId"] != server["traceId"] || child["parentSpanId"] != server["spanId"] || child["kind"].(float64) != 3 {
		t.Fatalf("LLM span is not a CLIENT child of the request span: %v / %v", child, server)
	}
	if child["name"] != "chat test-model" || attr(child, "gen_ai.provider.name")["stringValue"] != "ollama" ||
		attr(child, "gen_ai.operation.name")["stringValue"] != "chat" || attr(child, "gen_ai.request.model")["stringValue"] != "test-model" ||
		attr(child, "gen_ai.usage.input_tokens")["intValue"] != "42" || attr(child, "gen_ai.usage.output_tokens")["intValue"] != "7" {
		t.Fatalf("missing gen_ai attributes: %v", child)
	}
	if bg["traceId"] == server["traceId"] {
		t.Fatal("background call must start its own trace")
	}
}

func TestLLMSpanRecordsErrors(t *testing.T) {
	col, got := fakeCollector(t)
	appTracer = newTracer(col.URL, "demo")
	t.Cleanup(func() { appTracer = nil })
	l, _ := fakeLLM(t, func(w http.ResponseWriter) { http.Error(w, "boom", http.StatusInternalServerError) })
	if err := l.chat(context.Background(), []llmMsg{{"user", "hi"}}, nil, &map[string]any{}); err == nil {
		t.Fatal("want an error")
	}
	appTracer.flush()
	s := got()
	if len(s) != 1 || s[0]["status"].(map[string]any)["code"].(float64) != 2 || attr(s[0], "error.type")["stringValue"] != "llm_output" {
		t.Fatalf("error not recorded on the span: %v", s)
	}
}
