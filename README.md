# otx

Carry OpenTelemetry through `context.Context`.

`otx` bundles the three OpenTelemetry signal providers — traces, metrics and logs — together with a
propagator into one value, moves that value across process boundaries with gRPC and HTTP middleware, and
hands it back to you anywhere you have a `context.Context`.

```go
ctx, span := otx.TraceStart(ctx, "getFeature")
defer span.End()

log.From(ctx).Info("hello") // already correlated with the span above
```

## Install

The repository is four Go modules. Take the root plus whatever you use.

```sh
go get github.com/lesomnus/otx          # the core: Otx, log, otxmem, otxtest, tag
go get github.com/lesomnus/otx/otxgrpc  # gRPC stats handlers
go get github.com/lesomnus/otx/otxhttp  # HTTP middleware and transport
go get github.com/lesomnus/otx/otxsdk   # build an Otx from OTEL_* environment variables
```

## Setup

### From the environment

`otxsdk` reads the standard `OTEL_*` variables and hands back a configured `*otx.Otx` that owns
everything it built.

```go
import "github.com/lesomnus/otx/otxsdk"

func main() {
	ctx := context.Background()

	x, err := otxsdk.New(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer x.Shutdown(ctx)

	ctx = otx.Into(ctx, x)

	// ...
}
```

`OTEL_SERVICE_NAME`, `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_TRACES_SAMPLER` and the rest behave as the
OpenTelemetry specification says, because the SDK reads them itself. On top of those, `otxsdk` reads:

| | |
| --- | --- |
| `OTEL_SDK_DISABLED` | `true` makes all three signals no-ops |
| `OTEL_TRACES_EXPORTER`, `OTEL_METRICS_EXPORTER`, `OTEL_LOGS_EXPORTER` | `otlp` (default), `console`, `none`; a comma-separated list sends to several |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `http/protobuf` (default) or `grpc`, with the usual per-signal overrides |

Anything that is not SDK configuration goes through `WithOtxOptions`, and the resource can be replaced
or extended:

```go
x, err := otxsdk.New(ctx,
	otxsdk.WithResourceAttributes(semconv.DeploymentEnvironmentName("prod")),
	otxsdk.WithOtxOptions(otx.WithScopeName("github.com/me/my-service")),
)
```

It is a separate module because it pulls in the SDK and every exporter. If something else in your
program already builds the providers, you do not need it.

### By hand

Build the SDK providers as you normally would, hand them to `otx.New`, and shut it down on exit. `otx`
owns the providers you give it: `Shutdown` flushes and stops them, so buffered spans and records are not
lost when the process ends.

```go
package main

import (
	"context"
	"log/slog"

	"github.com/lesomnus/otx"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func main() {
	ctx := context.Background()

	x := otx.New(
		otx.WithTracerProvider(sdktrace.NewTracerProvider( /* ... */ )),
		otx.WithMeterProvider(sdkmetric.NewMeterProvider( /* ... */ )),
		otx.WithLoggerProvider(sdklog.NewLoggerProvider( /* ... */ )),

		// Attribute telemetry to this service rather than to otx.
		otx.WithScopeName("github.com/me/my-service"),
	)
	defer func() {
		if err := x.Shutdown(ctx); err != nil {
			slog.Error("shutdown telemetry", slog.Any("error", err))
		}
	}()

	ctx = otx.Into(ctx, x)

	// ...
}
```

### Defaults

Everything is optional. `otx.New()` with no arguments is valid and inert.

| | default | note |
| --- | --- | --- |
| tracer provider | `otel.GetTracerProvider()` | delegates, so a later `otel.SetTracerProvider` is picked up |
| meter provider | `otel.GetMeterProvider()` | same |
| logger provider | `log/global.GetLoggerProvider()` | same |
| propagator | `TraceContext` + `Baggage` | **not** `otel.GetTextMapPropagator()`, whose default propagates nothing |
| scope | `github.com/lesomnus/otx` at this module's version | override with `WithScopeName` / `WithScopeVersion` |
| controller | none | see [Lifecycle](#lifecycle) |

A provider taken from a global is *not* owned: `Shutdown` leaves it alone, because `otx` did not create
it. Only the providers you pass through `With*Provider` are shut down and flushed.

Passing `nil` to any option is ignored rather than fatal, including a typed nil such as a
`*sdktrace.TracerProvider` that a constructor returned along with an error.

## Server

Mount the middleware at the edge. It starts a span for the request and puts the `*otx.Otx` into the
request context, which is what makes `otx.Tracer(ctx)`, `log.From(ctx)` and friends work in the handler.

### gRPC

```go
import (
	"github.com/lesomnus/otx/otxgrpc"
	"google.golang.org/grpc"
)

server := grpc.NewServer(
	grpc.StatsHandler(otxgrpc.NewServerHandler(x)),
	grpc.StatsHandler(otxgrpc.NewServerLogger(x)),
)
```

`NewServerHandler` traces and injects; `NewServerLogger` writes one record per RPC boundary. gRPC calls
stats handlers in registration order, so register the tracing handler **first** — the log records then
carry the ids of its span.

Both loggers take `WithFilter` for the RPCs that are polled rather than called — a health check every
few seconds, from every replica, arrives often enough to be most of what is kept and says nothing that
reading it will repay:

```go
grpc.StatsHandler(otxgrpc.NewServerLogger(x, otxgrpc.WithFilter(func(i *stats.RPCTagInfo) bool {
	return !strings.HasPrefix(i.FullMethodName, "/grpc.health.v1.Health/")
}))),
```

What it filters is the records that logger writes. The RPC still carries the `*otx.Otx` and the
`rpc.service` and `rpc.method` attributes, and the tracing handler decides for itself.

### HTTP

```go
import "github.com/lesomnus/otx/otxhttp"

var h http.Handler = mux
h = otxhttp.BoundaryLogger(x)(h)
h = otxhttp.NewMiddleware(x, "api")(h)

http.ListenAndServe(":8080", h)
```

Order matters for the same reason: `BoundaryLogger` mounted **inside** `NewMiddleware` sees the server
span, so its records carry `trace_id` and `span_id`. It works at any mount point — it injects the
`*otx.Otx` itself — but mounted outside it has no span to correlate with.

## Client

The client side propagates the trace context outward, so the service you call joins your trace.

```go
// gRPC
conn, err := grpc.NewClient(target,
	grpc.WithStatsHandler(otxgrpc.NewClientHandler(x)),
	grpc.WithStatsHandler(otxgrpc.NewClientLogger(x)),
)

// HTTP
client := &http.Client{
	Transport: otxhttp.NewTransport(x, nil), // nil base means http.DefaultTransport
}
```

Both take the `*otx.Otx` and inject it into the outgoing request context, so calls made from a bare
`context.Background()` are still traced and logged.

## Extract

Anywhere downstream, read the signals back out of the context.

```go
import (
	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/log"
)

func (s *Server) GetFeature(ctx context.Context, p *Point) (*Feature, error) {
	ctx, span := otx.TraceStart(ctx, "getFeature")
	defer span.End()

	l := log.From(ctx)
	l.Info("looking up", slog.Float64("lat", p.Latitude))

	// ...
}
```

| | |
| --- | --- |
| `otx.From(ctx)` | the `*Otx` itself; never nil |
| `otx.FromOK(ctx)` | the `*Otx`, and whether there really was one |
| `otx.Tracer(ctx)` / `otx.TraceStart(ctx, name)` | `trace.Tracer` |
| `otx.Meter(ctx)` | `metric.Meter` |
| `otx.Int64Counter(ctx, name)` and friends | a metric instrument, cached — see [Metrics](#metrics) |
| `otx.Logger(ctx)` | the OpenTelemetry `log.Logger` |
| `otx.Propagator(ctx)` | `propagation.TextMapPropagator` |
| `log.From(ctx)` | an `*slog.Logger` |

On a context that carries no `*Otx`, every accessor falls back to a shared instance built from the
OpenTelemetry globals rather than panicking. Only `otx.Start`, `otx.Shutdown` and `otx.ForceFlush` report
the miss, as `otx.ErrNoOtx` — silently reporting a successful shutdown of nothing would hide the wiring
mistake.

## Metrics

`otx.Meter(ctx).Int64Counter(name)` returns an error that a call site can rarely act on, and takes the
meter's lock every time. These return the instrument directly, hand any error to the OpenTelemetry error
handler, and cache the result on the `*Otx`:

```go
otx.Int64Counter(ctx, "orders.placed",
	metric.WithDescription("orders accepted"),
).Add(ctx, 1, metric.WithAttributes(attribute.String("channel", ch)))
```

There is one for each of the eight synchronous instruments — `Int64` and `Float64` × `Counter`,
`UpDownCounter`, `Histogram`, `Gauge` — as both a package function taking a context and a method on
`*Otx`.

The cache is keyed by name and kind only, so two instruments sharing a name and kind must be declared
identically. OpenTelemetry requires that anyway: differing declarations of one name are a duplicate
registration. For asynchronous instruments and anything else, use `otx.Meter(ctx)` directly.

## Errors

`log.RecordError` reports an error as the outcome of the operation the context is in. It marks the
active span as failed, records the error on it as a span event, writes a correlated `Error` record, and
returns the error so it reads as part of a return statement:

```go
if err := store.Save(ctx, order); err != nil {
	return log.RecordError(ctx, "save order", err, slog.String("order.id", order.ID))
}
```

The message stays a fixed string and the error text goes into the `error.message` attribute, alongside
`error.type`. A message built from an error carries hostnames, addresses and timings, and a log backend
cannot group what it cannot repeat.

A nil error does nothing and returns nil, so the call can stay on a path that usually succeeds.

## Logging

`log.From(ctx)` returns a standard `*slog.Logger` bridged onto the OpenTelemetry logger provider with
[otelslog]. The logger is **bound to its context**, so the short methods carry it anyway:

```go
log.From(ctx).Info("hello")             // reaches the handler with ctx
log.From(ctx).InfoContext(ctx, "hello") // identical
```

Every record picks up the span that is active at the moment of the call, as the record's native trace
fields and as the `trace_id` / `span_id` attributes (`log.TraceIDKey`, `log.SpanIDKey`) so that a plain
text or JSON handler shows the correlation too.

To attach attributes to everything logged downstream, stash a logger:

```go
ctx = log.Into(ctx, log.From(ctx).With(slog.String("tenant", id)))
```

Stashing is safe to repeat: the span ids are resolved per record, not baked into the handler, so a logger
that passes through several spans does not accumulate stale ids.

`log.Into` also lets you swap the sink entirely, which is convenient in local development:

```go
ctx = log.Into(ctx, slog.New(slog.NewTextHandler(os.Stderr, nil)))
```

## Lifecycle

`Shutdown` stops the providers `otx` owns. To have something of your own stopped alongside them — an
exporter connection, a collection loop — give a `Controller`:

```go
x := otx.New(
	otx.WithTracerProvider(tp),
	otx.WithController(otx.JoinControllers(
		otx.NewController(pool.Connect, pool.Close),
		otx.NewController(nil, cache.Flush),
	)),
)

if err := x.Start(ctx); err != nil {
	return err
}
defer x.Shutdown(ctx)
```

`Start` runs the controllers in order and unwinds the ones that already started if a later one fails.
`Shutdown` runs them in reverse, then shuts down the owned providers, joining every error. It is
idempotent. `ForceFlush` flushes the owned providers without stopping them.

## Testing

`otxtest.New(t)` wires an `Otx` onto in-memory recorders for all three signals and shuts it down when
the test ends. Everything is synchronous — spans and records land as they are produced, metrics are
pulled on demand — so there is nothing to wait for and no sleeps to write.

```go
func TestPlaceOrder(t *testing.T) {
	h := otxtest.New(t)
	ctx := h.Into(t.Context())

	err := placeOrder(ctx, order)

	require.NoError(t, err)
	require.Len(t, h.Ended(), 1)
	require.Equal(t, "order placed", h.Records()[0].Body().AsString())
	require.NotEmpty(t, h.Collect(ctx).ScopeMetrics)
}
```

`h.Otx` is the `*otx.Otx`; `h.Spans`, `h.Logs` and `h.Reader` are the recorders behind it for anything
the methods do not cover. Options are applied after the recorders, so `otxtest.New(t, opts...)` can
replace any of them.

For a single signal, `otxmem.LogExporter` is the log exporter on its own, and upstream provides
`tracetest.NewInMemoryExporter` and `sdkmetric.NewManualReader`.

## Working on this repository

Four modules, no bootstrap: each submodule carries `replace github.com/lesomnus/otx => ../`, so a fresh
clone builds and a change to the root is exercised by the submodule tests immediately.

```sh
for m in . otxgrpc otxhttp otxsdk; do
	(cd "$m" && go build ./... && go vet ./... && go test ./... -race)
done
```

A `replace` directive is ignored by anyone who depends on the module, so it only affects work in this
repository. What consumers see is the `require` line, which means releases have to go in order:

1. tag the root — `git tag v1.2.3`
2. in each submodule, `go get github.com/lesomnus/otx@v1.2.3 && go mod tidy`
3. commit, then tag the submodules — `git tag otxgrpc/v1.2.3`, and the same for `otxhttp` and `otxsdk`

Skipping step 2 publishes a submodule that compiles here and not for anyone else. The `released` job in
CI is what catches it: it drops the replace directives and builds each submodule against the root
version it actually requires. It does not block a run, because it is expected to be red between a change
to the root and the release that follows.

[otelslog]: https://pkg.go.dev/go.opentelemetry.io/contrib/bridges/otelslog
