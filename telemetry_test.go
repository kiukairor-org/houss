package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// testTelemetry installs in-memory trace and metric pipelines as the global providers for one test.
func testTelemetry(t *testing.T) (*tracetest.InMemoryExporter, *sdkmetric.ManualReader) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prevT, prevM, prevP := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(prevT); otel.SetMeterProvider(prevM); otel.SetTextMapPropagator(prevP) })
	return exp, reader
}

func spanAttr(s tracetest.SpanStub, key string) attribute.Value {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value
		}
	}
	return attribute.Value{}
}

// histogramRoutes returns http.route -> request count for one histogram metric.
func histogramRoutes(t *testing.T, r *sdkmetric.ManualReader, name string) map[string]uint64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]uint64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				key, _ := dp.Attributes.Value("http.route")
				if key.AsString() == "" {
					key, _ = dp.Attributes.Value("server.address")
				}
				got[key.AsString()] += dp.Count
			}
		}
	}
	return got
}

func TestServerSpansAndSemconvDurationMetric(t *testing.T) {
	exp, reader := testTelemetry(t)
	mux := newMux(testServer(t))
	mux.HandleFunc("GET /fail", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "x"})
	})
	h := serverHandler(mux)
	for _, p := range []string{"/version", "/fail", "/nope", "/healthz"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, p, nil))
	}
	byName := map[string]tracetest.SpanStub{}
	for _, s := range exp.GetSpans() {
		byName[s.Name] = s
		if s.SpanKind != trace.SpanKindServer {
			t.Fatalf("want SERVER spans: %v", s.Name)
		}
	}
	if len(byName) != 3 { // /healthz is not traced
		t.Fatalf("got spans %v", byName)
	}
	ok, fail := byName["GET /version"], byName["GET /fail"]
	if spanAttr(ok, "http.route").AsString() != "/version" || spanAttr(fail, "http.response.status_code").AsInt64() != 500 {
		t.Fatalf("route/status not recorded: %v %v", ok.Attributes, fail.Attributes)
	}
	if fail.Status.Code != codes.Error || ok.Status.Code == codes.Error {
		t.Fatalf("5xx must be an error, 2xx not")
	}
	// New Relic's OpenTelemetry APM view and golden metrics read this metric (http.route = transaction name).
	routes := histogramRoutes(t, reader, "http.server.request.duration")
	if routes["/version"] != 1 || routes["/fail"] != 1 {
		t.Fatalf("http.server.request.duration by route: %v", routes)
	}
}

func TestOffWithoutEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	setupTelemetry(context.Background())(context.Background()) // installs nothing, shutdown is a no-op
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); ok {
		t.Fatal("no SDK provider must be installed without an endpoint")
	}
}

func TestLLMCallsAreGenAIClientSpansInsideTheRequest(t *testing.T) {
	exp, reader := testTelemetry(t)
	l, _ := fakeLLM(t, func(w http.ResponseWriter) {
		writeJSON(w, http.StatusOK, map[string]any{"model": "test-model", "prompt_eval_count": 42, "eval_count": 7,
			"message": map[string]string{"role": "assistant", "content": `{"ok":true}`}})
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ask", func(w http.ResponseWriter, r *http.Request) {
		var out map[string]any
		if err := l.chat(r.Context(), []llmMsg{{"user", "hi"}}, nil, &out); err != nil {
			t.Errorf("chat: %v", err)
		}
	})
	serverHandler(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ask", nil))
	var server, genai, httpc tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		switch {
		case s.SpanKind == trace.SpanKindServer:
			server = s
		case s.Name == "chat test-model":
			genai = s
		case s.SpanKind == trace.SpanKindClient:
			httpc = s
		}
	}
	if genai.Parent.SpanID() != server.SpanContext.SpanID() || genai.SpanKind != trace.SpanKindClient {
		t.Fatalf("gen_ai span is not a CLIENT child of the request span")
	}
	if httpc.Parent.SpanID() != genai.SpanContext.SpanID() || spanAttr(httpc, "http.request.method").AsString() != "POST" {
		t.Fatalf("otelhttp client span is not a child of the gen_ai span: %v", httpc)
	}
	if spanAttr(genai, "gen_ai.provider.name").AsString() != "ollama" || spanAttr(genai, "gen_ai.request.model").AsString() != "test-model" ||
		spanAttr(genai, "gen_ai.usage.input_tokens").AsInt64() != 42 || spanAttr(genai, "gen_ai.usage.output_tokens").AsInt64() != 7 {
		t.Fatalf("missing gen_ai attributes: %v", genai.Attributes)
	}
	if len(histogramRoutes(t, reader, "http.client.request.duration")) == 0 {
		t.Fatal("outbound call must record http.client.request.duration (external services)")
	}
}

func TestLLMSpanRecordsErrors(t *testing.T) {
	exp, _ := testTelemetry(t)
	l, _ := fakeLLM(t, func(w http.ResponseWriter) { http.Error(w, "boom", http.StatusInternalServerError) })
	if err := l.chat(context.Background(), []llmMsg{{"user", "hi"}}, nil, &map[string]any{}); err == nil {
		t.Fatal("want an error")
	}
	for _, s := range exp.GetSpans() {
		if s.Name == "chat test-model" {
			if s.Status.Code != codes.Error || spanAttr(s, "error.type").AsString() != "llm_output" {
				t.Fatalf("error not recorded on the span: %v", s)
			}
			return
		}
	}
	t.Fatal("no gen_ai span")
}

func TestThirdPartyCallsCarryNoTraceHeaders(t *testing.T) {
	testTelemetry(t)
	var hdr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hdr = r.Header.Get("traceparent") }))
	defer srv.Close()
	ctx, sp := startSpan(context.Background(), "parent")
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	if _, err := instrumentedClient(0, false).Do(req); err != nil {
		t.Fatal(err)
	}
	sp.end(nil)
	if hdr != "" {
		t.Fatalf("traceparent leaked to a third party: %q", hdr)
	}
}
