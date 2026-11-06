package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// streamLLM answers /api/chat like Ollama with stream:true: one JSON line per chunk.
func streamLLM(t *testing.T, delay time.Duration, chunks ...string) (*llmClient, *[]map[string]any) {
	t.Helper()
	return fakeLLM(t, func(w http.ResponseWriter) {
		time.Sleep(delay)
		for _, c := range chunks {
			fmt.Fprintf(w, `{"message":{"role":"assistant","content":%q},"done":false}`+"\n", c)
			w.(http.Flusher).Flush()
		}
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":""},"done":true}`)
	})
}

func chatBody(msgs ...string) string {
	var m []chatMsg
	for i, c := range msgs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		m = append(m, chatMsg{role, c})
	}
	b, _ := json.Marshal(map[string]any{"messages": m})
	return string(b)
}

func TestChatStreamsWithListContext(t *testing.T) {
	s := testServer(t)
	h := newMux(s)
	c := login(t, h)
	_, _ = s.store.Apply(Item{ID: "a", Title: "milk", Tab: "shopping", TargetDate: "2026-11-03"})
	var reqs *[]map[string]any
	s.llm, reqs = streamLLM(t, 0, "You need ", "milk tomorrow.\n+ eggs")
	rec := do(t, h, "POST", "/api/chat", chatBody("what do I need tomorrow?"), c)
	if rec.Code != 200 || rec.Body.String() != "You need milk tomorrow.\n+ eggs" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("%d %q", rec.Code, rec.Body)
	}
	r := (*reqs)[0]
	if r["think"] != false || r["stream"] != true || r["keep_alive"] != float64(-1) {
		t.Fatalf("request: %v", r)
	}
	msgs := r["messages"].([]any)
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" || !strings.Contains(sys["content"].(string), "- milk [Shopping]") {
		t.Fatalf("system prompt lacks the lists: %v", sys)
	}
	if n := len(listItems(t, h, "/api/items", c)); n != 1 {
		t.Fatalf("chat must not change data, store has %d items", n)
	}
}

func TestChatRefusals(t *testing.T) {
	old := chatFirstToken
	chatFirstToken = 200 * time.Millisecond
	t.Cleanup(func() { chatFirstToken = old })
	s := testServer(t)
	h := newMux(s)
	c := login(t, h)
	if rec := do(t, h, "POST", "/api/chat", chatBody("hi"), nil); rec.Code != 401 {
		t.Fatalf("no cookie: %d", rec.Code)
	}
	for _, bad := range []string{`{}`, chatBody("hi", "hello"), `{"messages":[{"role":"system","content":"x"}]}`, chatBody(strings.Repeat("x", chatMessageMax+1))} {
		if rec := do(t, h, "POST", "/api/chat", bad, c); rec.Code != 400 {
			t.Fatalf("%s: %d", bad, rec.Code)
		}
	}
	if rec := do(t, h, "POST", "/api/chat", chatBody("hi"), c); rec.Code != 503 || !strings.Contains(rec.Body.String(), "disabled") {
		t.Fatalf("disabled: %d %s", rec.Code, rec.Body)
	}
	down, _ := streamLLM(t, 0, "x")
	down.url = "http://127.0.0.1:1"
	s.llm = down
	if rec := do(t, h, "POST", "/api/chat", chatBody("hi"), c); rec.Code != 502 || !strings.Contains(rec.Body.String(), "down") {
		t.Fatalf("down: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/api/chat", chatBody("hi"), c); rec.Code != 503 || !strings.Contains(rec.Body.String(), "unavailable") {
		t.Fatalf("after a failure the chat says unavailable at once: %d %s", rec.Code, rec.Body)
	}
	s.llm, _ = streamLLM(t, time.Second, "late")
	start := time.Now()
	if rec := do(t, h, "POST", "/api/chat", chatBody("hi"), c); rec.Code != 504 || time.Since(start) > 900*time.Millisecond {
		t.Fatalf("no first token in time: %d after %v %s", rec.Code, time.Since(start), rec.Body)
	}
}

func TestChatRateLimit(t *testing.T) {
	s := testServer(t)
	h := newMux(s)
	c := login(t, h)
	s.llm, _ = streamLLM(t, 0, "ok")
	for i := 0; i < chatPerMinute; i++ {
		if rec := do(t, h, "POST", "/api/chat", chatBody("hi"), c); rec.Code != 200 {
			t.Fatalf("message %d: %d", i, rec.Code)
		}
	}
	if rec := do(t, h, "POST", "/api/chat", chatBody("hi"), c); rec.Code != 429 {
		t.Fatalf("over the limit: %d", rec.Code)
	}
	s.chat.now = func() time.Time { return time.Now().Add(61 * time.Second) }
	if rec := do(t, h, "POST", "/api/chat", chatBody("hi"), c); rec.Code != 200 {
		t.Fatalf("a minute later: %d", rec.Code)
	}
	// Concurrency: two answers at once, the third waits.
	l := newChatLimiter()
	r1, _ := l.acquire("a")
	r2, _ := l.acquire("b")
	if _, err := l.acquire("c"); err == nil {
		t.Fatal("third concurrent chat allowed")
	}
	r1()
	if r3, err := l.acquire("c"); err != nil {
		t.Fatal(err)
	} else {
		r3()
	}
	r2()
}

func TestTrimHistory(t *testing.T) {
	long := strings.Repeat("x", 900)
	var msgs []chatMsg
	for i := 0; i < 20; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, chatMsg{role, fmt.Sprintf("%d %s", i, long)})
	}
	msgs = append(msgs, chatMsg{"user", "last"})
	got := trimHistory(msgs)
	total := 0
	for _, m := range got {
		total += len(m.Content)
	}
	if total > chatHistoryMax || got[0].Role != "user" || got[len(got)-1].Content != "last" || len(got) < 3 {
		t.Fatalf("%d messages, %d chars, first %s", len(got), total, got[0].Role)
	}
}

func TestChatSystemSummary(t *testing.T) {
	items := []Item{
		{Title: "milk", Tab: "shopping", TargetDate: "2026-11-03"}, {Title: "bins", Tab: "todos", TargetDate: "2026-11-02"},
		{Title: "bank", Tab: "todos", TargetDate: "2026-10-30"}, {Title: "dentist", Tab: "todos", TargetDate: "2026-11-06"},
		{Title: "passport", Tab: "midterm", TargetDate: "2026-12-01"}, {Title: "shed", Tab: "midterm"},
		{Title: "plumber", Tab: "todos", Done: true},
	}
	got := chatSystem(items, monday)
	for _, want := range []string{"Today is Monday 2026-11-02", "Overdue:\n- bank [Todos] due 2026-10-30\n",
		"Due today (2026-11-02):\n- bins [Todos]\n", "Due tomorrow (2026-11-03):\n- milk [Shopping]\n",
		"Due later this week (until 2026-11-08):\n- dentist [Todos] due 2026-11-06\n", "Due later:\n- passport [Mid-term] due 2026-12-01\n",
		"No due date:\n- shed [Mid-term]\n", "Done:\n- plumber [Todos]\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	var many []Item
	for i := 0; i < chatListMax+20; i++ {
		many = append(many, Item{Title: fmt.Sprint("item", i), Tab: "shopping"})
	}
	if n := strings.Count(chatSystem(many, monday), "\n- "); n != chatListMax {
		t.Errorf("summary has %d items, want the cap %d", n, chatListMax)
	}
	// On a Sunday "this week" ends today.
	if got := chatSystem(nil, monday.AddDate(0, 0, 6)); !strings.Contains(got, "until 2026-11-08") {
		t.Errorf("sunday: %s", got)
	}
}

// The tracing wrapper must pass Flush through, or answers arrive in one block instead of streaming.
func TestTracingWrapperKeepsFlusher(t *testing.T) {
	testTelemetry(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("ResponseWriter lost http.Flusher behind serverHandler")
		}
	})
	serverHandler(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/stream", nil))
}
