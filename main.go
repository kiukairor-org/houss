// Minimal HTTP service. Every homie-pipeline app starts from this.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

// version is set at build time: -ldflags "-X main.version=<git sha>".
var version = "dev"

func appName() string {
	if n := os.Getenv("APP_NAME"); n != "" {
		return n
	}
	return "app"
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"app": appName(), "message": "hello"})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"app": appName(), "version": version})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	addr := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}
	tr := setupTracing() // OTLP traces to $OTEL_EXPORTER_OTLP_ENDPOINT (see telemetry.go)
	srv := &http.Server{Addr: addr, Handler: tr.middleware(newMux()), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("%s %s listening on %s", appName(), version, addr)
	log.Fatal(srv.ListenAndServe())
}
