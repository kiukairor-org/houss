package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
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
		ctx, cancel := context.WithTimeout(r.Context(), lookupTime*2)
		defer cancel()
		res, err := s.tesco.Diagnose(ctx, q, r.URL.Query().Get("compare") == "1")
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
		writeJSON(w, http.StatusOK, map[string]any{"items": s.store.List("")})
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
