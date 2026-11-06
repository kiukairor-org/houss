package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeLLM is an Ollama stand-in: it records each /api/chat request and replies with reply().
func fakeLLM(t *testing.T, reply func(w http.ResponseWriter)) (*llmClient, *[]map[string]any) {
	t.Helper()
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" {
			writeJSON(w, http.StatusOK, map[string]string{"version": "test"})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		reply(w)
	}))
	t.Cleanup(srv.Close)
	l := &llmClient{url: srv.URL, model: "test-model", client: instrumentedClient(0, true), up: true}
	return l, &got
}

func answer(content string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		writeJSON(w, http.StatusOK, map[string]any{"message": map[string]string{"role": "assistant", "content": content}})
	}
}

func TestQuickAddValid(t *testing.T) {
	l, reqs := fakeLLM(t, answer(`{"items":[{"title":"milk","list":"shopping","when":"tomorrow"},`+
		`{"title":"eggs","list":"shopping","when":"tomorrow"},{"title":"call the plumber","list":"todos","when":"friday"}]}`))
	got, err := l.QuickAdd(t.Context(), "milk and eggs for tomorrow, call the plumber friday", monday)
	if err != nil {
		t.Fatal(err)
	}
	want := []Item{{Title: "milk", Tab: "shopping", TargetDate: "2026-11-03"},
		{Title: "eggs", Tab: "shopping", TargetDate: "2026-11-03"}, {Title: "call the plumber", Tab: "todos", TargetDate: "2026-11-06"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item %d: got %+v want %+v", i, got[i], want[i])
		}
	}
	r := (*reqs)[0]
	if r["think"] != false || r["stream"] != false || r["keep_alive"] != float64(-1) || r["model"] != "test-model" || r["format"] == nil {
		t.Fatalf("request must disable thinking/streaming, keep the model loaded and send a schema: %v", r)
	}
	if opts, _ := r["options"].(map[string]any); opts["temperature"] != float64(0) || opts["num_predict"] != nil {
		t.Fatalf("options: %v", opts)
	}
}

func TestQuickAddFailures(t *testing.T) {
	old := quickAddTimeout
	quickAddTimeout = 200 * time.Millisecond
	t.Cleanup(func() { quickAddTimeout = old })

	down, _ := fakeLLM(t, answer(`{}`))
	down.url = "http://127.0.0.1:1" // connection refused

	slow, _ := fakeLLM(t, func(w http.ResponseWriter) {
		time.Sleep(time.Second)
		answer(`{"items":[{"title":"milk","list":"shopping","when":""}]}`)(w)
	})
	garbage, _ := fakeLLM(t, answer(`{"items":[{"title":"milk",`))
	httpErr, _ := fakeLLM(t, func(w http.ResponseWriter) { http.Error(w, "model not found", http.StatusNotFound) })
	invented, _ := fakeLLM(t, answer(`{"items":[{"title":"bread","list":"shopping","when":""}]}`))
	badTab, _ := fakeLLM(t, answer(`{"items":[{"title":"milk","list":"fridge","when":""}]}`))

	cases := []struct {
		name    string
		l       *llmClient
		want    error
		outcome string
	}{
		{"down", down, errLLMDown, "down"},
		{"slow", slow, errLLMDown, "timeout"},
		{"garbage JSON", garbage, errLLMOutput, "rejected"},
		{"HTTP error", httpErr, errLLMOutput, "rejected"},
		{"invented item", invented, errLLMOutput, "rejected"},
		{"unknown tab", badTab, errLLMOutput, "rejected"},
	}
	for _, c := range cases {
		start := time.Now()
		got, err := c.l.QuickAdd(t.Context(), "milk", monday)
		if !errors.Is(err, c.want) || got != nil || llmOutcome(err) != c.outcome {
			t.Errorf("%s: got %v, %v (outcome %s)", c.name, got, err, llmOutcome(err))
		}
		if time.Since(start) > 900*time.Millisecond {
			t.Errorf("%s: waited %v, more than the timeout", c.name, time.Since(start))
		}
	}
	if down.Available() || slow.Available() {
		t.Error("a failed call must mark the LLM unavailable until the next health check")
	}
}

func TestQuickAddAPI(t *testing.T) {
	s := testServer(t)
	h := newMux(s)
	c := login(t, h)
	if rec := do(t, h, "POST", "/api/quickadd", `{"text":"milk"}`, nil); rec.Code != 401 {
		t.Fatalf("no cookie: %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/api/quickadd", `{"text":"  "}`, c); rec.Code != 400 {
		t.Fatalf("empty text: %d", rec.Code)
	}
	// LLM disabled: the client is told to add the note as typed, and nothing is saved.
	rec := do(t, h, "POST", "/api/quickadd", `{"text":"milk"}`, c)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"fallback":"disabled"`) || !strings.Contains(rec.Body.String(), `"items":null`) {
		t.Fatalf("disabled: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/api/llm", "", c); !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("status: %s", rec.Body)
	}

	s.llm, _ = fakeLLM(t, answer(`{"items":[{"title":"milk","list":"shopping","when":""},{"title":"call mum","list":"todos","when":""}]}`))
	rec = do(t, h, "POST", "/api/quickadd", `{"text":"milk, call mum"}`, c)
	var out struct{ Items []Item }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Items) != 2 || out.Items[1].Tab != "todos" {
		t.Fatalf("smart add: %s", rec.Body)
	}
	if n := len(listItems(t, h, "/api/items", c)); n != 0 {
		t.Fatalf("quickadd must not save anything, store has %d items", n)
	}

	s.llm.setUp(false) // e.g. at night: answer at once instead of waiting for a timeout
	rec = do(t, h, "POST", "/api/quickadd", `{"text":"milk"}`, c)
	if !strings.Contains(rec.Body.String(), `"fallback":"unavailable"`) {
		t.Fatalf("unavailable: %s", rec.Body)
	}
	if rec := do(t, h, "GET", "/api/llm", "", c); !strings.Contains(rec.Body.String(), `{"available":false,"enabled":true}`) {
		t.Fatalf("status: %s", rec.Body)
	}
}

func TestLLMHealthCheck(t *testing.T) {
	l, _ := fakeLLM(t, answer(`{}`))
	l.setUp(false)
	l.check()
	if !l.Available() {
		t.Fatal("healthy server reported down")
	}
	l.url = "http://127.0.0.1:1"
	l.check()
	if l.Available() {
		t.Fatal("unreachable server reported up")
	}
}
