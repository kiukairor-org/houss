package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// learnLLM is an Ollama stand-in that answers by step (group / profile / suggest), told apart by
// the system prompt, and records every request.
type learnLLM struct {
	mu      sync.Mutex
	answers map[string]string // step -> message content; missing = HTTP 500
	reqs    []map[string]any
	delay   time.Duration
}

func newLearnLLM(t *testing.T, answers map[string]string) (*learnLLM, *llmClient) {
	t.Helper()
	f := &learnLLM{answers: answers}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" {
			writeJSON(w, http.StatusOK, map[string]string{"version": "test"})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sys := body["messages"].([]any)[0].(map[string]any)["content"].(string)
		step := "suggest"
		switch {
		case strings.HasPrefix(sys, "You group"):
			step = "group"
		case strings.HasPrefix(sys, "You keep"):
			step = "profile"
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, body)
		a, ok := f.answers[step]
		d := f.delay
		f.mu.Unlock()
		time.Sleep(d)
		if !ok {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"message": map[string]string{"role": "assistant", "content": a}})
	}))
	t.Cleanup(srv.Close)
	return f, &llmClient{url: srv.URL, model: "test-model", client: &http.Client{}, up: true}
}

func (f *learnLLM) count(step string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.reqs {
		sys := r["messages"].([]any)[0].(map[string]any)["content"].(string)
		if (step == "group") == strings.HasPrefix(sys, "You group") && (step == "profile") == strings.HasPrefix(sys, "You keep") {
			n++
		}
	}
	return n
}

// now is a Tuesday 10:00 London; bought(title, days ago...) ticks a Shopping item on those days.
var learnNow = time.Date(2026, 11, 10, 10, 0, 0, 0, london)

func bought(t *testing.T, s *store, title string, daysAgo ...int) {
	t.Helper()
	id := "id-" + normTitle(title)
	for _, d := range daysAgo {
		at := learnNow.AddDate(0, 0, -d)
		apply(t, s, Item{ID: id, Title: title, Tab: "shopping", UpdatedAt: at.UnixMilli() - 1000})
		apply(t, s, Item{ID: id, Title: title, Tab: "shopping", Done: true, UpdatedAt: at.UnixMilli()})
	}
}

func TestStatsSuggestionsWithoutLLM(t *testing.T) {
	s, _ := openStore(t.TempDir())
	bought(t, s, "Milk", 15, 10, 5) // every 5 days, 5 days ago: due
	bought(t, s, "Lessive", 40, 10) // every 30 days, 10 days ago: not yet
	bought(t, s, "Saffron", 3)      // once: cold start, never suggested
	bought(t, s, "Bread", 9, 6, 3)  // every 3 days, 3 days ago: due, but on the list (below)
	apply(t, s, Item{ID: "open", Title: "bread", Tab: "shopping", UpdatedAt: learnNow.UnixMilli()})
	ln := newLearner("", s, nil)
	src, got := ln.Suggestions(learnNow)
	if src != "stats" || len(got) != 1 || got[0].Name != "Milk" || got[0].Reason != "usually every 5 days, last bought 5 days ago" {
		t.Fatalf("%s %+v", src, got)
	}
	ln.Dismiss("milk", learnNow)
	if _, got := ln.Suggestions(learnNow.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("dismissed still shown: %+v", got)
	}
	if _, got := ln.Suggestions(learnNow.AddDate(0, 0, 6)); len(got) != 1 { // its usual gap has passed
		t.Fatalf("dismissal never expires: %+v", got)
	}
}

func TestGroupingMergesOnlyExistingNames(t *testing.T) {
	s, _ := openStore(t.TempDir())
	bought(t, s, "Milk", 15)
	bought(t, s, "lait", 10)
	bought(t, s, "semi-skimmed milk", 5)
	bought(t, s, "Skyr", 4)
	f, l := newLearnLLM(t, map[string]string{
		"group":   `{"groups":[{"name":"milk","titles":["Milk","lait","semi-skimmed milk"]}]}`,
		"profile": `{"profile":["Buys milk every 5 days"]}`,
		"suggest": `{"suggestions":[]}`,
	})
	ln := newLearner(t.TempDir(), s, l)
	ln.step(learnNow, false)
	_, got := ln.Suggestions(learnNow) // picks are empty: the LLM chose nothing
	if len(got) != 0 {
		t.Fatalf("%+v", got)
	}
	ln.mu.Lock()
	stats := ln.statsLocked(s.History(0, 100), learnNow)
	ln.mu.Unlock()
	if g := stats["milk"]; g == nil || g.Count != 3 || g.Median != 5 || stats["skyr"] == nil || len(stats) != 2 {
		t.Fatalf("groups: %+v", stats)
	}
	if f.count("group") != 1 {
		t.Fatal("expected one grouping call")
	}
	ln.step(learnNow.Add(time.Minute), false) // nothing new: no regrouping
	if f.count("group") != 1 {
		t.Fatal("regrouped without new names")
	}

	// An invented name rejects the whole answer.
	s2, _ := openStore(t.TempDir())
	bought(t, s2, "Milk", 10)
	bought(t, s2, "lait", 5)
	_, l2 := newLearnLLM(t, map[string]string{"group": `{"groups":[{"name":"milk","titles":["Milk","lait","oat milk"]}]}`})
	if _, err := newLearner("", s2, l2).regroup(t.Context(), []string{"Milk", "lait"}); err == nil {
		t.Fatal("invented name accepted")
	}
}

func TestLLMSuggestionsAreValidated(t *testing.T) {
	s, _ := openStore(t.TempDir())
	bought(t, s, "Milk", 15, 10, 5)
	bought(t, s, "Eggs", 14, 7)
	bought(t, s, "Flour", 21, 14, 7, 0) // just bought: not a candidate
	f, l := newLearnLLM(t, map[string]string{
		"profile": `{"profile":["Pancakes on Sundays"]}`,
		"suggest": `{"suggestions":[{"name":"Milk"},{"name":"Eggs"},{"name":"Milk"}]}`,
	})
	ln := newLearner("", s, l)
	if err := ln.pickSuggestions(t.Context(), learnNow); err != nil {
		t.Fatal(err)
	}
	src, got := ln.Suggestions(learnNow)
	if src != "llm" || len(got) != 2 || got[0].Name != "Milk" || got[1].Name != "Eggs" || // the LLM's order, once each
		got[0].Reason != "usually every 5 days, last bought 5 days ago" {
		t.Fatalf("%s %+v", src, got)
	}
	req := f.reqs[len(f.reqs)-1]
	if req["think"] != false || req["stream"] != false || req["format"] == nil {
		t.Fatalf("request: %v", req)
	}
	if opts, _ := req["options"].(map[string]any); opts["temperature"] != float64(0) || opts["num_predict"] != nil {
		t.Fatalf("options: %v", opts)
	}
	enum := req["format"].(map[string]any)["properties"].(map[string]any)["suggestions"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["name"].(map[string]any)["enum"]
	if b, _ := json.Marshal(enum); string(b) != `["Eggs","Milk"]` {
		t.Fatalf("candidates offered: %s", b)
	}
	// Adding Eggs to the list drops it from the shown picks at once.
	apply(t, s, Item{ID: "e", Title: "eggs", Tab: "shopping", UpdatedAt: learnNow.UnixMilli()})
	if _, got := ln.Suggestions(learnNow); len(got) != 1 || got[0].Name != "Milk" {
		t.Fatalf("%+v", got)
	}

	// A name outside the candidates rejects the answer; the stats picks stay in use.
	_, l2 := newLearnLLM(t, map[string]string{"suggest": `{"suggestions":[{"name":"Caviar"}]}`})
	ln2 := newLearner("", s, l2)
	if err := ln2.pickSuggestions(t.Context(), learnNow); err == nil {
		t.Fatal("non-candidate accepted")
	}
	if src, _ := ln2.Suggestions(learnNow); src != "stats" {
		t.Fatal(src)
	}
}

func TestSuggestionsFallBackWhenLLMDownSlowOrGarbage(t *testing.T) {
	s, _ := openStore(t.TempDir())
	bought(t, s, "Milk", 15, 10, 5)
	f, l := newLearnLLM(t, map[string]string{"profile": `not json`, "suggest": `{"suggestions":[{"name":"Milk"}]}`})
	ln := newLearner("", s, l)
	ln.step(learnNow, false)
	if p, _, _ := ln.Profile(); p != "" {
		t.Fatalf("garbage profile stored: %q", p)
	}
	if src, got := ln.Suggestions(learnNow); src != "llm" || len(got) != 1 {
		t.Fatalf("%s %+v", src, got)
	}
	l.setUp(false) // asleep: plain statistics
	if src, got := ln.Suggestions(learnNow); src != "stats" || len(got) != 1 || got[0].Name != "Milk" {
		t.Fatalf("%s %+v", src, got)
	}
	n := len(f.reqs)
	ln.step(learnNow.Add(time.Minute), true) // no calls while down
	if len(f.reqs) != n {
		t.Fatal("called the LLM while it is down")
	}

	l.setUp(true)
	old := learnTimeout
	learnTimeout = 50 * time.Millisecond
	t.Cleanup(func() { learnTimeout = old })
	f.mu.Lock()
	f.delay = 300 * time.Millisecond
	f.mu.Unlock()
	ln2 := newLearner("", s, l)
	ln2.step(learnNow, true)
	if src, _ := ln2.Suggestions(learnNow); src != "stats" {
		t.Fatalf("slow LLM: %s", src)
	}
}

func TestProfileMorningRunAndNumberGuard(t *testing.T) {
	dir := t.TempDir()
	s, _ := openStore(t.TempDir())
	bought(t, s, "Milk", 15, 10, 5)
	f, l := newLearnLLM(t, map[string]string{
		"profile": `{"profile":["Achète du lait tous les 5 jours","Achète du café tous les 9 jours","No pork","  "]}`,
		"suggest": `{"suggestions":[]}`,
	})
	ln := newLearner(dir, s, l)
	ln.step(learnNow, false) // never made one: runs now
	p, at, edited := ln.Profile()
	if p != "Achète du lait tous les 5 jours" || at == 0 || edited { // 9 is in no fact, "No pork" is the example's: dropped
		t.Fatalf("%q %d %v", p, at, edited)
	}
	if !strings.Contains(ln.chatContext(), p) {
		t.Fatal("chat does not see the profile")
	}
	ln.step(learnNow.Add(time.Hour), false) // later the same day: no rerun
	morning := time.Date(2026, 11, 11, 7, 5, 0, 0, london)
	ln.step(morning.Add(-10*time.Minute), false) // 06:55: too early
	if f.count("profile") != 1 {
		t.Fatalf("profile calls = %d", f.count("profile"))
	}
	ln.step(morning, false)
	ln.step(morning.Add(time.Minute), false)
	if f.count("profile") != 2 {
		t.Fatalf("profile calls = %d", f.count("profile"))
	}
	// The person's edit is kept and becomes the base of the next run; clearing forgets it.
	if err := ln.SetProfile("No meat, ever\nBio milk only", morning); err != nil {
		t.Fatal(err)
	}
	if p, _, edited := newLearner(dir, s, l).Profile(); p != "No meat, ever\nBio milk only" || !edited {
		t.Fatalf("not persisted: %q", p)
	}
	ln.step(morning.Add(2*time.Minute), true)
	msgs := f.reqs[len(f.reqs)-2]["messages"].([]any)
	if last := msgs[len(msgs)-1].(map[string]any)["content"].(string); !strings.Contains(last, "No meat, ever") {
		t.Fatalf("previous profile not sent: %s", last)
	}
	// The LLM's answer left the person's lines out; they are pinned and kept, theirs first.
	if p, _, edited := ln.Profile(); p != "No meat, ever\nBio milk only\nAchète du lait tous les 5 jours" || edited {
		t.Fatalf("pinned lines lost: %q %v", p, edited)
	}
	if err := ln.SetProfile(strings.Repeat("x\n", 13), morning); err == nil {
		t.Fatal("13 lines accepted")
	}
	if err := ln.SetProfile("", morning); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := ln.Profile(); p != "" || ln.chatContext() != "" || len(ln.st.Pinned) != 0 {
		t.Fatal("clear failed")
	}
}

func TestLearnEndpoints(t *testing.T) {
	s := testServer(t)
	h := newMux(s)
	for _, p := range []string{"/api/suggestions", "/api/profile"} {
		if rec := do(t, h, "GET", p, "", nil); rec.Code != 401 {
			t.Fatalf("%s without cookie: %d", p, rec.Code)
		}
	}
	c := login(t, h)
	now := time.Now()
	for _, d := range []int{15, 10, 5} {
		at := now.AddDate(0, 0, -d).UnixMilli()
		apply(t, s.store, Item{ID: "m", Title: "Milk", Tab: "shopping", UpdatedAt: at - 1000})
		apply(t, s.store, Item{ID: "m", Title: "Milk", Tab: "shopping", Done: true, UpdatedAt: at})
	}
	var sug struct {
		Source      string       `json:"source"`
		Suggestions []Suggestion `json:"suggestions"`
	}
	rec := do(t, h, "GET", "/api/suggestions", "", c)
	if err := json.Unmarshal(rec.Body.Bytes(), &sug); err != nil || sug.Source != "stats" || len(sug.Suggestions) != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/api/suggestions/dismiss", `{"name":"Milk"}`, c); rec.Code != 200 {
		t.Fatalf("dismiss: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/suggestions", "", c); !strings.Contains(rec.Body.String(), `"suggestions":[]`) {
		t.Fatalf("after dismiss: %s", rec.Body)
	}
	if rec := do(t, h, "PUT", "/api/profile", `{"profile":"Veggie household"}`, c); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"edited":true`) {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/api/profile", "", c); !strings.Contains(rec.Body.String(), "Veggie household") {
		t.Fatalf("get: %s", rec.Body)
	}
	if rec := do(t, h, "POST", "/api/learn/refresh", "", c); rec.Code != 503 { // no LLM in this server
		t.Fatalf("refresh: %d", rec.Code)
	}
}
