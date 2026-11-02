// Smart quick-add through the shared household LLM (Ollama, reached in-cluster at
// http://llm.ai.svc.cluster.local:11434). The LLM is optional: it is switched off every night and
// can vanish at any time, so every caller falls back to adding the text exactly as typed.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// quickAddTimeout is the most a user waits on the LLM before the note is added as typed (a var for tests).
var quickAddTimeout = 5 * time.Second

const (
	healthEvery   = 30 * time.Second
	healthTimeout = 2 * time.Second
)

var errLLMDown = errors.New("llm unavailable")

type llmClient struct {
	url, model string
	client     *http.Client

	mu sync.Mutex
	up bool // result of the last health check or call
}

// newLLM reads LLM_ENABLED / LLM_URL / LLM_MODEL; nil means smart features are off.
func newLLM() *llmClient {
	if os.Getenv("LLM_ENABLED") != "true" {
		return nil
	}
	url := os.Getenv("LLM_URL")
	if url == "" {
		url = "http://llm.ai.svc.cluster.local:11434"
	}
	model := os.Getenv("LLM_MODEL")
	if model == "" {
		model = "qwen3:1.7b"
	}
	return &llmClient{url: strings.TrimRight(url, "/"), model: model, client: &http.Client{}}
}

// Available reports whether smart features should be tried now (nil-safe).
func (l *llmClient) Available() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.up
}

func (l *llmClient) setUp(up bool) {
	l.mu.Lock()
	l.up = up
	l.mu.Unlock()
}

// check pings the server so the app stops waiting on the LLM while it is down (e.g. every night).
func (l *llmClient) check() {
	ctx, cancel := context.WithTimeout(context.Background(), healthTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", l.url+"/api/version", nil)
	resp, err := l.client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	l.setUp(err == nil && resp.StatusCode == http.StatusOK)
}

func (l *llmClient) run() {
	l.check()
	for range time.Tick(healthEvery) {
		l.check()
	}
}

type llmMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chat sends one non-streaming request and decodes the model's JSON answer into out.
// Connection problems and timeouts return errLLMDown; anything malformed returns errLLMOutput.
func (l *llmClient) chat(ctx context.Context, msgs []llmMsg, schema any, out any) error {
	body, _ := json.Marshal(map[string]any{
		"model":      l.model,
		"messages":   msgs,
		"stream":     false,
		"think":      false, // qwen3 and lfm3 reason by default: minutes instead of seconds
		"keep_alive": -1,
		"format":     schema,
		"options":    map[string]any{"temperature": 0},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", l.url+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.client.Do(req)
	if err != nil {
		l.setUp(false)
		return fmt.Errorf("%w: %w", errLLMDown, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: HTTP %d", errLLMOutput, resp.StatusCode)
	}
	var r struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: %w", errLLMDown, ctx.Err())
		}
		return fmt.Errorf("%w: %v", errLLMOutput, err)
	}
	l.setUp(true)
	if err := json.Unmarshal([]byte(r.Message.Content), out); err != nil {
		return fmt.Errorf("%w: %v", errLLMOutput, err)
	}
	return nil
}

var quickSchema = map[string]any{"type": "object", "required": []string{"items"},
	"properties": map[string]any{"items": map[string]any{"type": "array", "items": map[string]any{
		"type": "object", "required": []string{"title", "list", "when", "when_text"},
		"properties": map[string]any{
			"title":     map[string]any{"type": "string"},
			"list":      map[string]any{"type": "string", "enum": []string{"shopping", "todos", "midterm"}},
			"when":      map[string]any{"type": "string", "enum": whenWords},
			"when_text": map[string]any{"type": "string"},
		}}}}}

// The system prompt and examples never change, so the server can reuse its cache for them.
const quickSystem = "You sort household quick-add notes for a shared list app. Split a note into separate items, " +
	"one thing to buy or one task each. list is shopping (things to buy), todos (tasks, calls, appointments) or " +
	"midterm (projects, repairs, anything with no hurry). when is the day the note gives for that item (today, " +
	"tomorrow, a weekday, next_week, next_month), else empty; when_text is the note's own words for that day, " +
	"else empty. title is short, in the note's language, without the day words and without adding words like Buy. " +
	"Use only things written in the note. Answer with compact one-line JSON."

var quickExamples = []llmMsg{
	{"user", "eggs and butter for tomorrow, call the plumber"},
	{"assistant", `{"items":[{"title":"eggs","list":"shopping","when":"tomorrow","when_text":"tomorrow"},{"title":"butter","list":"shopping","when":"tomorrow","when_text":"tomorrow"},{"title":"call the plumber","list":"todos","when":"","when_text":""}]}`},
	{"user", "du pain et du beurre, sortir les poubelles ce soir, rappeler la banque vendredi, repeindre le salon"},
	{"assistant", `{"items":[{"title":"pain","list":"shopping","when":"","when_text":""},{"title":"beurre","list":"shopping","when":"","when_text":""},{"title":"sortir les poubelles","list":"todos","when":"today","when_text":"ce soir"},{"title":"rappeler la banque","list":"todos","when":"friday","when_text":"vendredi"},{"title":"repeindre le salon","list":"midterm","when":"","when_text":""}]}`},
}

// QuickAdd turns a free-text note into items (title, tab, target date). Any error means the
// caller should add the note exactly as typed.
func (l *llmClient) QuickAdd(ctx context.Context, note string, now time.Time) ([]Item, error) {
	ctx, cancel := context.WithTimeout(ctx, quickAddTimeout)
	defer cancel()
	return l.split(ctx, note, now)
}

func (l *llmClient) split(ctx context.Context, note string, now time.Time) ([]Item, error) {
	msgs := append([]llmMsg{{"system", quickSystem}}, quickExamples...)
	msgs = append(msgs, llmMsg{"user", note})
	var out struct {
		Items []quickItem `json:"items"`
	}
	if err := l.chat(ctx, msgs, quickSchema, &out); err != nil {
		return nil, err
	}
	return checkQuick(note, out.Items, now)
}
