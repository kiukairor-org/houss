// Tracing without dependencies: one OTLP server span per request, exported as OTLP/HTTP JSON to
// $OTEL_EXPORTER_OTLP_ENDPOINT (the in-cluster OpenTelemetry Collector, which forwards to New Relic).
// No endpoint set (tests, local runs) means no export. Keep this file when building on the template.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	spanKindServer  = 2 // OTLP SpanKind SERVER
	statusCodeError = 2 // OTLP StatusCode ERROR
	maxBatch        = 256
	flushEvery      = 5 * time.Second
)

type otlpValue struct {
	StringValue *string `json:"stringValue,omitempty"`
	IntValue    *string `json:"intValue,omitempty"` // OTLP/JSON encodes int64 as a string
}

type otlpAttr struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpStatus struct {
	Code int `json:"code,omitempty"`
}

type otlpSpan struct {
	TraceID     string     `json:"traceId"`
	SpanID      string     `json:"spanId"`
	Name        string     `json:"name"`
	Kind        int        `json:"kind"`
	StartUnixNs string     `json:"startTimeUnixNano"`
	EndUnixNs   string     `json:"endTimeUnixNano"`
	Attributes  []otlpAttr `json:"attributes"`
	Status      otlpStatus `json:"status"`
}

func strAttr(k, v string) otlpAttr { return otlpAttr{Key: k, Value: otlpValue{StringValue: &v}} }

func intAttr(k string, v int) otlpAttr {
	s := strconv.Itoa(v)
	return otlpAttr{Key: k, Value: otlpValue{IntValue: &s}}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// tracer buffers spans and posts them in batches; it never blocks a request (full buffer = drop).
type tracer struct {
	url     string
	service string
	client  *http.Client
	spans   chan otlpSpan
	mu      sync.Mutex // serialises flushes
}

func newTracer(endpoint, service string) *tracer {
	if endpoint == "" {
		return nil
	}
	return &tracer{
		url:     strings.TrimRight(endpoint, "/") + "/v1/traces",
		service: service,
		client:  &http.Client{Timeout: 5 * time.Second},
		spans:   make(chan otlpSpan, 4*maxBatch),
	}
}

func (t *tracer) record(s otlpSpan) {
	select {
	case t.spans <- s:
	default: // buffer full: drop rather than slow the app down
	}
}

// flush sends everything buffered so far.
func (t *tracer) flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for {
		batch := make([]otlpSpan, 0, maxBatch)
	fill:
		for len(batch) < maxBatch {
			select {
			case s := <-t.spans:
				batch = append(batch, s)
			default:
				break fill
			}
		}
		if len(batch) == 0 {
			return
		}
		t.post(batch)
	}
}

func (t *tracer) post(batch []otlpSpan) {
	body := map[string]any{"resourceSpans": []any{map[string]any{
		"resource": map[string]any{"attributes": []otlpAttr{
			strAttr("service.name", t.service),
			strAttr("service.version", version),
			strAttr("telemetry.sdk.language", "go"),
		}},
		"scopeSpans": []any{map[string]any{"scope": map[string]string{"name": "app-template"}, "spans": batch}},
	}}}
	buf, err := json.Marshal(body)
	if err != nil {
		return
	}
	resp, err := t.client.Post(t.url, "application/json", bytes.NewReader(buf))
	if err != nil {
		log.Printf("telemetry: export failed: %v", err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("telemetry: export rejected: HTTP %d", resp.StatusCode)
	}
}

func (t *tracer) run() {
	for range time.Tick(flushEvery) {
		t.flush()
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// middleware wraps the mux; with a nil tracer it returns the mux unchanged.
func (t *tracer) middleware(mux *http.ServeMux) http.Handler {
	if t == nil {
		return mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" { // probes: not worth a span
			mux.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		mux.ServeHTTP(rec, r)
		end := time.Now()
		route := r.Pattern // set by ServeMux (Go 1.23+), e.g. "GET /version"; empty on 404
		if route == "" {
			route = r.Method + " (unmatched)"
		} else if !strings.HasPrefix(route, r.Method+" ") {
			route = r.Method + " " + route
		}
		s := otlpSpan{
			TraceID:     randHex(16),
			SpanID:      randHex(8),
			Name:        route,
			Kind:        spanKindServer,
			StartUnixNs: strconv.FormatInt(start.UnixNano(), 10),
			EndUnixNs:   strconv.FormatInt(end.UnixNano(), 10),
			Attributes: []otlpAttr{
				strAttr("http.request.method", r.Method),
				strAttr("http.route", strings.TrimPrefix(route, r.Method+" ")),
				strAttr("url.path", r.URL.Path),
				intAttr("http.response.status_code", rec.status),
			},
		}
		if rec.status >= 500 {
			s.Status.Code = statusCodeError
		}
		t.record(s)
	})
}

// setupTracing reads the standard OTel env vars; returns nil (tracing off) without an endpoint.
func setupTracing() *tracer {
	service := os.Getenv("OTEL_SERVICE_NAME")
	if service == "" {
		service = appName()
	}
	t := newTracer(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"), service)
	if t != nil {
		go t.run()
	}
	return t
}
