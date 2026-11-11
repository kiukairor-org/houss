// OpenTelemetry with the official Go SDK (D81, supersedes the hand-written exporter of D40): traces and metrics as
// OTLP/HTTP to $OTEL_EXPORTER_OTLP_ENDPOINT (the in-cluster OpenTelemetry Collector, which forwards to New Relic).
//   - otelhttp on the server: one SERVER span per request + the semconv metric http.server.request.duration, which
//     New Relic's OpenTelemetry APM view and golden metrics are computed from.
//   - otelhttp on every outbound http.Client (instrumentedClient): CLIENT spans + http.client.request.duration, so
//     Ollama and tesco.com show as external services.
//   - Go runtime metrics (GC, goroutines, memory) for New Relic's Go runtime page.
//   - logs: the official log/slog bridge (otelslog). Every log line -- including the standard library's log.Printf,
//     which Go routes through the default slog handler -- is exported over OTLP; lines logged with a request
//     context (slog.InfoContext) carry that request's trace and span ids, which is what "logs in context" needs.
//     The same lines still go to stderr for kubectl logs.
//
// No endpoint set (tests, local runs) means no export. Keep this file when building on the template.
package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

const scopeName = "app-template"

type otlpAttr = attribute.KeyValue

func strAttr(k, v string) otlpAttr { return attribute.String(k, v) }

func intAttr(k string, v int) otlpAttr { return attribute.Int(k, v) }

// telemetryResource builds the OpenTelemetry resource. resource.Default() is the BASE (telemetry.sdk.*, and a
// service.name of "unknown_service:<exe>" when nothing else sets one). In resource.Merge(a, b) the SECOND argument
// wins, so we always set service.name explicitly on top of it and never fall back to unknown_service. The name is
// the one the environment supplies (OTEL_SERVICE_NAME, else OTEL_RESOURCE_ATTRIBUTES), else appName() -- which reads
// APP_NAME, set by Argo CD to the repo name, e.g. "houss" or "pr-5-houss" for a preview.
func telemetryResource() (*resource.Resource, error) {
	name := envServiceName()
	if name == "" {
		name = appName()
	}
	return resource.Merge(resource.Default(), resource.NewSchemaless(semconv.ServiceName(name), semconv.ServiceVersion(version)))
}

// envServiceName returns the service.name the OTEL_* environment supplies (OTEL_SERVICE_NAME, else a service.name
// entry in OTEL_RESOURCE_ATTRIBUTES), or "" when neither is set.
func envServiceName() string {
	if v := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME")); v != "" {
		return v
	}
	for _, kv := range strings.Split(os.Getenv("OTEL_RESOURCE_ATTRIBUTES"), ",") {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.TrimSpace(k) == string(semconv.ServiceNameKey) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// setupTelemetry installs the global tracer and meter providers from the standard OTEL_* env vars. It returns a
// shutdown func that flushes both; without OTEL_EXPORTER_OTLP_ENDPOINT it installs nothing (no-op providers).
func setupTelemetry(ctx context.Context) func(context.Context) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return func(context.Context) {}
	}
	res, err := telemetryResource()
	if err != nil {
		log.Printf("telemetry: resource: %v", err)
	}
	te, err := otlptracehttp.New(ctx)
	if err != nil {
		log.Printf("telemetry: off, trace exporter: %v", err)
		return func(context.Context) {}
	}
	me, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithTemporalitySelector(deltaSelector))
	if err != nil {
		log.Printf("telemetry: off, metric exporter: %v", err)
		return func(context.Context) {}
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(te), sdktrace.WithResource(res))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(me)), sdkmetric.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		log.Printf("telemetry: runtime metrics: %v", err)
	}
	shutdownLogs := setupLogs(ctx, res)
	return func(ctx context.Context) {
		_ = tp.Shutdown(ctx)
		_ = mp.Shutdown(ctx)
		shutdownLogs(ctx)
	}
}

// setupLogs makes log/slog (and with it the standard log package) write to stderr AND to OTLP through the official
// otelslog bridge. A failure here leaves logging as it was (stderr only).
func setupLogs(ctx context.Context, res *resource.Resource) func(context.Context) {
	le, err := otlploghttp.New(ctx)
	if err != nil {
		log.Printf("telemetry: logs stay on stderr only, log exporter: %v", err)
		return func(context.Context) {}
	}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(le)), sdklog.WithResource(res))
	global.SetLoggerProvider(lp)
	slog.SetDefault(slog.New(slog.NewMultiHandler(
		slog.NewTextHandler(os.Stderr, nil),
		otelslog.NewHandler(scopeName, otelslog.WithLoggerProvider(lp)),
	)))
	return func(ctx context.Context) { _ = lp.Shutdown(ctx) }
}

// deltaSelector: New Relic's recommended temporality (delta for counters and histograms, cumulative otherwise).
func deltaSelector(k sdkmetric.InstrumentKind) metricdata.Temporality {
	switch k {
	case sdkmetric.InstrumentKindCounter, sdkmetric.InstrumentKindHistogram, sdkmetric.InstrumentKindObservableCounter:
		return metricdata.DeltaTemporality
	}
	return metricdata.CumulativeTemporality
}

// serverHandler wraps the mux with otelhttp. otelhttp reads the ServeMux pattern (Go 1.22+) after the handler ran:
// it becomes http.route on the span and on http.server.request.duration, and names the span "GET /api/items".
// /healthz (probes) is not traced.
func serverHandler(mux *http.ServeMux) http.Handler {
	routed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
		if r.Pattern != "" { // otelhttp puts the route on the metric only; the span gets it here
			route := r.Pattern[strings.IndexByte(r.Pattern, ' ')+1:] // "GET /version" -> "/version"
			trace.SpanFromContext(r.Context()).SetAttributes(semconv.HTTPRoute(route))
		}
	})
	return otelhttp.NewHandler(routed, "http.server",
		otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/healthz" }),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			if r.Pattern == "" {
				return r.Method
			}
			if strings.HasPrefix(r.Pattern, r.Method+" ") {
				return r.Pattern
			}
			return r.Method + " " + r.Pattern
		}),
	)
}

// instrumentedClient is an http.Client whose calls become CLIENT spans + http.client.request.duration.
// propagate=false keeps trace headers away from third parties (tesco.com).
func instrumentedClient(timeout time.Duration, propagate bool) *http.Client {
	opts := []otelhttp.Option{}
	if !propagate {
		opts = append(opts, otelhttp.WithPropagators(propagation.NewCompositeTextMapPropagator()))
	}
	return &http.Client{Timeout: timeout, Transport: otelhttp.NewTransport(http.DefaultTransport, opts...)}
}

// clientSpan is a logical outbound operation (e.g. one LLM chat) around the HTTP call(s) it makes.
type clientSpan struct{ s trace.Span }

// startSpan opens a CLIENT span as a child of the span in ctx; pass the returned ctx to the HTTP request so the
// otelhttp span nests under it.
func startSpan(ctx context.Context, name string, attrs ...otlpAttr) (context.Context, *clientSpan) {
	ctx, s := otel.Tracer(scopeName).Start(ctx, name, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	return ctx, &clientSpan{s}
}

// end records the span; a non-nil err marks it as an error with error.type.
func (c *clientSpan) end(err error, attrs ...otlpAttr) {
	c.s.SetAttributes(attrs...)
	if err != nil {
		c.s.SetStatus(codes.Error, err.Error())
		c.s.SetAttributes(strAttr("error.type", errType(err)))
	}
	c.s.End()
}

// addSpanAttr attaches an attribute to the current request's span; a no-op without tracing.
func addSpanAttr(ctx context.Context, a otlpAttr) {
	trace.SpanFromContext(ctx).SetAttributes(a)
}
