// Assistant chat through the shared LLM. The server proxies one streamed answer per request (the
// browser never talks to Ollama), adds a summary of the household's lists as context, and never
// changes any data: suggested items come back as text lines the person can tap to add.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// chatFirstToken is the most a person waits for the first word (a var for tests).
var chatFirstToken = 5 * time.Second

const (
	chatMaxTime     = 90 * time.Second
	chatMaxPredict  = 600  // a phone screen's worth; keeps prompt + answer inside the 4096 context
	chatHistoryMax  = 6000 // characters of past turns sent (~1.5k tokens); oldest turns go first
	chatMessageMax  = 1000 // characters per message
	chatListMax     = 80   // items in the list summary
	chatPerMinute   = 6    // per signed-in session
	chatConcurrency = 2    // the LLM serves two requests at a time
)

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// trimHistory keeps the newest turns that fit chatHistoryMax and drops leading assistant turns,
// so the model always sees a conversation that starts with the person.
func trimHistory(msgs []chatMsg) []chatMsg {
	total, start := 0, len(msgs)
	for start > 0 && total+len(msgs[start-1].Content) <= chatHistoryMax {
		start--
		total += len(msgs[start].Content)
	}
	for start < len(msgs) && msgs[start].Role != "user" {
		start++
	}
	return msgs[start:]
}

// chatSystem tells the model who it is for and what is on the lists right now. Items are grouped
// by when they are due, worked out here, because a 1.7B model is poor at date arithmetic.
func chatSystem(items []Item, now time.Time) string {
	t := now.In(london)
	day := func(d int) string { return t.AddDate(0, 0, d).Format("2006-01-02") }
	today, tomorrow := day(0), day(1)
	weekEnd := day((7 - int(t.Weekday())) % 7) // the coming Sunday
	groups := []struct {
		name  string
		lines []string
	}{
		{"Overdue", nil}, {"Due today (" + today + ")", nil}, {"Due tomorrow (" + tomorrow + ")", nil},
		{"Due later this week (until " + weekEnd + ")", nil}, {"Due later", nil}, {"No due date", nil}, {"Done", nil},
	}
	names := map[string]string{"shopping": "Shopping", "todos": "Todos", "midterm": "Mid-term"}
	for i, it := range items {
		if i >= chatListMax {
			break
		}
		g := 5
		switch d := it.TargetDate; {
		case it.Done:
			g = 6
		case d == "":
		case d < today:
			g = 0
		case d == today:
			g = 1
		case d == tomorrow:
			g = 2
		case d <= weekEnd:
			g = 3
		default:
			g = 4
		}
		line := "- " + it.Title + " [" + names[it.Tab] + "]"
		if g == 0 || g == 3 || g == 4 {
			line += " due " + it.TargetDate
		}
		groups[g].lines = append(groups[g].lines, line)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You are the assistant of houss, a shared household list app. Today is %s %s (London). "+
		"Answer briefly and helpfully, in the language of the person's last message, using only the lists below; "+
		"never invent items or dates. When you mention items already on the lists, write them as lines starting "+
		"with \"- \". Only for NEW things worth adding that are not on the lists yet, write one per line starting "+
		"with \"+ \" (for example \"+ flour\"); the person can tap those to add them. You cannot change the lists "+
		"yourself.\n\nThe household's lists, by due date ([Shopping] = to buy):\n", t.Weekday(), today)
	for _, g := range groups {
		fmt.Fprintf(&b, "%s:", g.name)
		if len(g.lines) == 0 {
			b.WriteString(" none")
		}
		for _, l := range g.lines {
			b.WriteString("\n" + l)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// chatLimiter allows chatPerMinute requests per session and chatConcurrency at once.
type chatLimiter struct {
	mu     sync.Mutex
	recent map[string][]time.Time
	busy   int
	now    func() time.Time
}

func newChatLimiter() *chatLimiter {
	return &chatLimiter{recent: map[string][]time.Time{}, now: time.Now}
}

// acquire returns a release func, or an error saying why the request must wait.
func (l *chatLimiter) acquire(session string) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := l.now().Add(-time.Minute)
	for k, ts := range l.recent { // forget idle sessions so the map stays small
		kept := ts[:0]
		for _, t := range ts {
			if t.After(cut) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(l.recent, k)
		} else {
			l.recent[k] = kept
		}
	}
	if len(l.recent[session]) >= chatPerMinute {
		return nil, errors.New("too many messages, wait a minute")
	}
	if l.busy >= chatConcurrency {
		return nil, errors.New("the assistant is busy, try again in a moment")
	}
	l.recent[session] = append(l.recent[session], l.now())
	l.busy++
	return func() {
		l.mu.Lock()
		l.busy--
		l.mu.Unlock()
	}, nil
}

// chatStream asks the LLM for one streamed answer and copies the text to w as it arrives. Nothing
// is written to w before the first token, so a slow or absent LLM still gets a clean JSON error.
func (l *llmClient) chatStream(ctx context.Context, w http.ResponseWriter, msgs []llmMsg) (firstMs, chars int, err error) {
	ctx, cancel := context.WithTimeout(ctx, chatMaxTime)
	defer cancel()
	first := time.AfterFunc(chatFirstToken, cancel)
	defer first.Stop()
	body, _ := json.Marshal(map[string]any{
		"model":      l.model,
		"messages":   msgs,
		"stream":     true,
		"think":      false,
		"keep_alive": -1,
		"options":    map[string]any{"temperature": 0.4, "num_predict": chatMaxPredict},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", l.url+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := l.client.Do(req)
	if err != nil {
		l.setUp(false)
		return 0, 0, fmt.Errorf("%w: %w", errLLMDown, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("%w: HTTP %d", errLLMOutput, resp.StatusCode)
	}
	flusher, _ := w.(http.Flusher)
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var part struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Done bool `json:"done"`
		}
		if json.Unmarshal(sc.Bytes(), &part) != nil {
			continue
		}
		if part.Message.Content != "" {
			if chars == 0 {
				first.Stop()
				firstMs = int(time.Since(start).Milliseconds())
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusOK)
			}
			chars += len(part.Message.Content)
			_, _ = w.Write([]byte(part.Message.Content))
			if flusher != nil {
				flusher.Flush()
			}
		}
		if part.Done {
			break
		}
	}
	l.setUp(true)
	if err := sc.Err(); err != nil || chars == 0 {
		if err == nil {
			err = errors.New("empty answer")
		}
		if ctx.Err() != nil {
			err = fmt.Errorf("%w: %w", errLLMDown, ctx.Err())
		}
		return firstMs, chars, err
	}
	return firstMs, chars, nil
}

func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []chatMsg `json:"messages"`
	}
	if err := decodeBody(w, r, &body); err != nil || len(body.Messages) == 0 || len(body.Messages) > 50 {
		apiErr(w, errInvalid)
		return
	}
	for _, m := range body.Messages {
		if (m.Role != "user" && m.Role != "assistant") || strings.TrimSpace(m.Content) == "" || len(m.Content) > chatMessageMax {
			apiErr(w, errInvalid)
			return
		}
	}
	if body.Messages[len(body.Messages)-1].Role != "user" {
		apiErr(w, errInvalid)
		return
	}
	if s.llm == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "disabled"})
		return
	}
	if !s.llm.Available() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
		return
	}
	session := ""
	if c, err := r.Cookie(cookieName); err == nil {
		session = c.Value
	}
	release, err := s.chat.acquire(session)
	if err != nil {
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
		return
	}
	defer release()

	msgs := []llmMsg{{"system", chatSystem(s.store.List(""), time.Now())}}
	for _, m := range trimHistory(body.Messages) {
		msgs = append(msgs, llmMsg{m.Role, m.Content})
	}
	start := time.Now()
	firstMs, chars, err := s.llm.chatStream(r.Context(), w, msgs)
	outcome := "ok"
	switch {
	case err == nil:
	case r.Context().Err() != nil:
		outcome = "client_gone"
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		outcome = "timeout"
	case errors.Is(err, errLLMDown):
		outcome = "down"
	default:
		outcome = "error"
	}
	addSpanAttr(r.Context(), strAttr("llm.outcome", outcome))
	addSpanAttr(r.Context(), intAttr("llm.first_token_ms", firstMs))
	addSpanAttr(r.Context(), intAttr("llm.duration_ms", int(time.Since(start).Milliseconds())))
	log.Printf("llm: chat outcome=%s first_token_ms=%d duration_ms=%d chars=%d", outcome, firstMs, time.Since(start).Milliseconds(), chars)
	if err != nil && chars > 0 {
		_, _ = w.Write([]byte("\n\n[interrupted]"))
	}
	if err != nil && chars == 0 { // nothing streamed yet: a clean error the client can show
		status := http.StatusBadGateway
		if outcome == "timeout" {
			status = http.StatusGatewayTimeout
		}
		writeJSON(w, status, map[string]string{"error": outcome})
	}
}
