package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
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
}
