// Tracing without dependencies: one OTLP server span per request, exported as OTLP/HTTP JSON to
// $OTEL_EXPORTER_OTLP_ENDPOINT (the in-cluster OpenTelemetry Collector, which forwards to New Relic).
// No endpoint set (tests, local runs) means no export. Keep this file when building on the template.
package main

import (
	"bytes"
	"context"
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
	spanKindClient  = 3 // OTLP SpanKind CLIENT (outbound calls, e.g. the LLM)
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
	ParentID    string     `json:"parentSpanId,omitempty"`
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

// Flush lets streamed responses (the assistant chat) through the tracing wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

type spanAttrsKey struct{}

type spanAttrs struct {
	mu      sync.Mutex
	attrs   []otlpAttr
	traceID string // ids of the request span, so outbound calls become its children
	spanID  string
}

// appTracer is the process tracer (set by setupTracing); nil means tracing is off.
var appTracer *tracer

// clientSpan is an outbound call in progress (nil-safe: every method is a no-op without tracing).
type clientSpan struct {
	t     *tracer
	s     otlpSpan
	start time.Time
}

// startSpan opens a CLIENT span: a child of the request span in ctx, or a new trace for background work.
func startSpan(ctx context.Context, name string, attrs ...otlpAttr) *clientSpan {
	if appTracer == nil {
		return nil
	}
	s := otlpSpan{TraceID: randHex(16), SpanID: randHex(8), Name: name, Kind: spanKindClient, Attributes: attrs}
	if sa, ok := ctx.Value(spanAttrsKey{}).(*spanAttrs); ok && sa.traceID != "" {
		s.TraceID, s.ParentID = sa.traceID, sa.spanID
	}
	return &clientSpan{t: appTracer, s: s, start: time.Now()}
}

// end records the span; a non-nil err marks it as an error with error.type.
func (c *clientSpan) end(err error, attrs ...otlpAttr) {
	if c == nil {
		return
	}
	c.s.StartUnixNs = strconv.FormatInt(c.start.UnixNano(), 10)
	c.s.EndUnixNs = strconv.FormatInt(time.Now().UnixNano(), 10)
	c.s.Attributes = append(c.s.Attributes, attrs...)
	if err != nil {
		c.s.Status.Code = statusCodeError
		c.s.Attributes = append(c.s.Attributes, strAttr("error.type", errType(err)))
	}
	c.t.record(c.s)
}

// addSpanAttr attaches an attribute to the current request's span; a no-op without tracing.
func addSpanAttr(ctx context.Context, a otlpAttr) {
	if sa, ok := ctx.Value(spanAttrsKey{}).(*spanAttrs); ok {
		sa.mu.Lock()
		sa.attrs = append(sa.attrs, a)
		sa.mu.Unlock()
	}
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
		sa := &spanAttrs{traceID: randHex(16), spanID: randHex(8)}
		r = r.WithContext(context.WithValue(r.Context(), spanAttrsKey{}, sa)) // r.Pattern is set on this copy
		mux.ServeHTTP(rec, r)
		end := time.Now()
		route := r.Pattern // set by ServeMux (Go 1.23+), e.g. "GET /version"; empty on 404
		if route == "" {
			route = r.Method + " (unmatched)"
		} else if !strings.HasPrefix(route, r.Method+" ") {
			route = r.Method + " " + route
		}
		s := otlpSpan{
			TraceID:     sa.traceID,
			SpanID:      sa.spanID,
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
		sa.mu.Lock()
		s.Attributes = append(s.Attributes, sa.attrs...)
		sa.mu.Unlock()
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
	appTracer = t
	if t != nil {
		go t.run()
	}
	return t
}
