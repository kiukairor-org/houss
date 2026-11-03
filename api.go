package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	return json.NewDecoder(r.Body).Decode(v)
}

func apiErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errInvalid):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid item"})
	case errors.Is(err, os.ErrNotExist):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}
}

func (s *server) registerAPI(mux *http.ServeMux) {
	a := s.auth
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("POST /api/logout", a.logout)
	mux.HandleFunc("GET /api/session", a.require(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
	}))

	// Runs one live Tesco lookup and reports what came back; compare=1 also tries browser-like headers.
	mux.HandleFunc("GET /api/debug/tesco", a.require(func(w http.ResponseWriter, r *http.Request) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if s.tesco == nil || q == "" {
			apiErr(w, errInvalid)
			return
		}
		compare := r.URL.Query().Get("compare") == "1"
		profiles := 1
		if compare {
			profiles = 2
		}
		// Each profile may wait on the rate limiter and then do a full request.
		ctx, cancel := context.WithTimeout(r.Context(), lookupTime*2*time.Duration(profiles))
		defer cancel()
		res, err := s.tesco.Diagnose(ctx, q, compare)
		if err != nil {
			apiErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"query": q, "results": res})
	}))

	mux.HandleFunc("GET /api/items", a.require(func(w http.ResponseWriter, r *http.Request) {
		tab := r.URL.Query().Get("tab")
		if tab != "" && !tabs[tab] {
			apiErr(w, errInvalid)
			return
		}
		writeJSON(w, http.StatusOK, s.store.List(tab))
	}))

	mux.HandleFunc("POST /api/items", a.require(func(w http.ResponseWriter, r *http.Request) {
		var it Item
		if err := decodeBody(w, r, &it); err != nil {
			apiErr(w, errInvalid)
			return
		}
		if it.ID == "" {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			it.ID = hex.EncodeToString(b)
		}
		it.Deleted = false
		res, err := s.store.Apply(it)
		if err != nil {
			apiErr(w, err)
			return
		}
		s.sorter.Kick()
		writeJSON(w, http.StatusCreated, res[0])
	}))

	mux.HandleFunc("PUT /api/items/{id}", a.require(func(w http.ResponseWriter, r *http.Request) {
		var it Item
		if err := decodeBody(w, r, &it); err != nil {
			apiErr(w, errInvalid)
			return
		}
		it.ID, it.Deleted = r.PathValue("id"), false
		res, err := s.store.Apply(it)
		if err != nil {
			apiErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res[0])
	}))

	mux.HandleFunc("DELETE /api/items/{id}", a.require(func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.Delete(r.PathValue("id"), 0); err != nil {
			apiErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
	}))

	// Offline queue replay: apply changes (last write wins per item), return the full live list.
	mux.HandleFunc("POST /api/sync", a.require(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Changes []Item `json:"changes"`
		}
		if err := decodeBody(w, r, &body); err != nil {
			apiErr(w, errInvalid)
			return
		}
		if _, err := s.store.Apply(body.Changes...); err != nil {
			apiErr(w, err)
			return
		}
		s.sorter.Kick()
		writeJSON(w, http.StatusOK, map[string]any{"items": s.store.List("")})
	}))

	// Purchase history (history.go), newest first: ?since=<unix ms>&limit=<n, default 500>.
	mux.HandleFunc("GET /api/history", a.require(func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || limit <= 0 || limit > historyMax {
			limit = 500
		}
		writeJSON(w, http.StatusOK, s.store.History(since, limit))
	}))

	// Learned buying patterns (learn.go): suggestions above the Shopping list and the profile.
	if s.learn == nil {
		s.learn = newLearner("", s.store, s.llm)
	}
	mux.HandleFunc("GET /api/suggestions", a.require(func(w http.ResponseWriter, r *http.Request) {
		src, list := s.learn.Suggestions(time.Now())
		writeJSON(w, http.StatusOK, map[string]any{"source": src, "suggestions": list})
	}))
	mux.HandleFunc("POST /api/suggestions/dismiss", a.require(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := decodeBody(w, r, &body); err != nil || strings.TrimSpace(body.Name) == "" || len(body.Name) > 200 {
			apiErr(w, errInvalid)
			return
		}
		s.learn.Dismiss(strings.TrimSpace(body.Name), time.Now())
		writeJSON(w, http.StatusOK, map[string]bool{"dismissed": true})
	}))
	mux.HandleFunc("GET /api/profile", a.require(func(w http.ResponseWriter, r *http.Request) {
		p, at, edited := s.learn.Profile()
		writeJSON(w, http.StatusOK, map[string]any{"profile": p, "updated_at": at, "edited": edited})
	}))
	mux.HandleFunc("PUT /api/profile", a.require(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Profile string `json:"profile"`
		}
		if err := decodeBody(w, r, &body); err != nil {
			apiErr(w, errInvalid)
			return
		}
		if err := s.learn.SetProfile(body.Profile, time.Now()); err != nil {
			apiErr(w, err)
			return
		}
		p, at, edited := s.learn.Profile()
		writeJSON(w, http.StatusOK, map[string]any{"profile": p, "updated_at": at, "edited": edited})
	}))
	// Re-learns now (profile and suggestions) instead of waiting for the morning run.
	mux.HandleFunc("POST /api/learn/refresh", a.require(func(w http.ResponseWriter, r *http.Request) {
		if !s.llm.Available() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
			return
		}
		s.learn.Refresh()
		writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
	}))

	// Assistant chat (chat.go): one streamed answer per request, read-only.
	if s.chat == nil {
		s.chat = newChatLimiter()
	}
	mux.HandleFunc("POST /api/chat", a.require(s.handleChat))

	// Smart add: whether to try it, so clients skip the wait while the LLM is down (e.g. at night).
	mux.HandleFunc("GET /api/llm", a.require(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"enabled": s.llm != nil, "available": s.llm.Available()})
	}))

	// Smart add: split a free-text note into items. It never saves anything; the client adds the
	// returned items, or the note as typed when "items" is null (LLM off, down, slow or nonsensical).
	mux.HandleFunc("POST /api/quickadd", a.require(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if err := decodeBody(w, r, &body); err != nil || strings.TrimSpace(body.Text) == "" || len(body.Text) > 200 {
			apiErr(w, errInvalid)
			return
		}
		start := time.Now()
		got, outcome := []Item(nil), "disabled"
		if s.llm.Available() {
			var err error
			got, err = s.llm.QuickAdd(r.Context(), strings.TrimSpace(body.Text), start)
			outcome = llmOutcome(err)
		} else if s.llm != nil {
			outcome = "unavailable"
		}
		ms := int(time.Since(start).Milliseconds())
		addSpanAttr(r.Context(), strAttr("llm.outcome", outcome))
		addSpanAttr(r.Context(), intAttr("llm.duration_ms", ms))
		addSpanAttr(r.Context(), intAttr("llm.items", len(got)))
		if s.llm != nil {
			log.Printf("llm: quickadd outcome=%s duration_ms=%d items=%d", outcome, ms, len(got))
		}
		if outcome != "ok" {
			writeJSON(w, http.StatusOK, map[string]any{"items": nil, "fallback": outcome})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": got})
	}))

	// Prices come from the server-side cache only; missing ones are fetched in the background,
	// so this never waits on a store. Clients poll it to pick up results.
	mux.HandleFunc("GET /api/prices", a.require(func(w http.ResponseWriter, r *http.Request) {
		out := map[string]PriceView{}
		total, priced := 0.0, 0
		if s.prices != nil {
			for _, it := range s.store.List("shopping") {
				v := s.prices.Get(it.Title)
				out[it.ID] = v
				if v.Status == "ok" && !it.Done {
					total += v.Price.Price
					priced++
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"prices": out, "total": total, "priced": priced})
	}))
}

// llmOutcome names a QuickAdd result for logs and spans.
func llmOutcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, errLLMDown):
		return "down"
	default:
		return "rejected"
	}
}
