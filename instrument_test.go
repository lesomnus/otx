package otx_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxtest"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// instrumentCase drives one of the eight instrument kinds through the same
// assertions. The eight constructors have nothing in common in their
// signatures, so each case adapts one of them: new_otx and new_ctx return the
// instrument as an any, record makes a single measurement of 3 through it, and
// shape names the aggregation the SDK must produce for that kind, which is what
// tells Int64Counter apart from Float64Counter or from Int64Gauge in the
// collected data.
type instrumentCase struct {
	kind  string
	shape string

	new_otx func(x *otx.Otx, name string, opts ...metric.InstrumentOption) any
	new_ctx func(ctx context.Context, name string, opts ...metric.InstrumentOption) any

	record func(ctx context.Context, v any)
}

// accumulates reports whether repeated measurements add up, which every kind
// but a gauge does; a gauge keeps only the last one.
func (c instrumentCase) accumulates() bool {
	return !strings.HasPrefix(c.shape, "gauge")
}

var instrument_cases = []instrumentCase{
	{
		kind:  "Int64Counter",
		shape: "monotonic sum[int64]",
		new_otx: func(x *otx.Otx, name string, opts ...metric.InstrumentOption) any {
			return x.Int64Counter(name, instOpts[metric.Int64CounterOption](opts)...)
		},
		new_ctx: func(ctx context.Context, name string, opts ...metric.InstrumentOption) any {
			return otx.Int64Counter(ctx, name, instOpts[metric.Int64CounterOption](opts)...)
		},
		record: func(ctx context.Context, v any) { v.(metric.Int64Counter).Add(ctx, 3) },
	},
	{
		kind:  "Int64UpDownCounter",
		shape: "sum[int64]",
		new_otx: func(x *otx.Otx, name string, opts ...metric.InstrumentOption) any {
			return x.Int64UpDownCounter(name, instOpts[metric.Int64UpDownCounterOption](opts)...)
		},
		new_ctx: func(ctx context.Context, name string, opts ...metric.InstrumentOption) any {
			return otx.Int64UpDownCounter(ctx, name, instOpts[metric.Int64UpDownCounterOption](opts)...)
		},
		record: func(ctx context.Context, v any) { v.(metric.Int64UpDownCounter).Add(ctx, 3) },
	},
	{
		kind:  "Int64Histogram",
		shape: "histogram[int64]",
		new_otx: func(x *otx.Otx, name string, opts ...metric.InstrumentOption) any {
			return x.Int64Histogram(name, instOpts[metric.Int64HistogramOption](opts)...)
		},
		new_ctx: func(ctx context.Context, name string, opts ...metric.InstrumentOption) any {
			return otx.Int64Histogram(ctx, name, instOpts[metric.Int64HistogramOption](opts)...)
		},
		record: func(ctx context.Context, v any) { v.(metric.Int64Histogram).Record(ctx, 3) },
	},
	{
		kind:  "Int64Gauge",
		shape: "gauge[int64]",
		new_otx: func(x *otx.Otx, name string, opts ...metric.InstrumentOption) any {
			return x.Int64Gauge(name, instOpts[metric.Int64GaugeOption](opts)...)
		},
		new_ctx: func(ctx context.Context, name string, opts ...metric.InstrumentOption) any {
			return otx.Int64Gauge(ctx, name, instOpts[metric.Int64GaugeOption](opts)...)
		},
		record: func(ctx context.Context, v any) { v.(metric.Int64Gauge).Record(ctx, 3) },
	},
	{
		kind:  "Float64Counter",
		shape: "monotonic sum[float64]",
		new_otx: func(x *otx.Otx, name string, opts ...metric.InstrumentOption) any {
			return x.Float64Counter(name, instOpts[metric.Float64CounterOption](opts)...)
		},
		new_ctx: func(ctx context.Context, name string, opts ...metric.InstrumentOption) any {
			return otx.Float64Counter(ctx, name, instOpts[metric.Float64CounterOption](opts)...)
		},
		record: func(ctx context.Context, v any) { v.(metric.Float64Counter).Add(ctx, 3) },
	},
	{
		kind:  "Float64UpDownCounter",
		shape: "sum[float64]",
		new_otx: func(x *otx.Otx, name string, opts ...metric.InstrumentOption) any {
			return x.Float64UpDownCounter(name, instOpts[metric.Float64UpDownCounterOption](opts)...)
		},
		new_ctx: func(ctx context.Context, name string, opts ...metric.InstrumentOption) any {
			return otx.Float64UpDownCounter(ctx, name, instOpts[metric.Float64UpDownCounterOption](opts)...)
		},
		record: func(ctx context.Context, v any) { v.(metric.Float64UpDownCounter).Add(ctx, 3) },
	},
	{
		kind:  "Float64Histogram",
		shape: "histogram[float64]",
		new_otx: func(x *otx.Otx, name string, opts ...metric.InstrumentOption) any {
			return x.Float64Histogram(name, instOpts[metric.Float64HistogramOption](opts)...)
		},
		new_ctx: func(ctx context.Context, name string, opts ...metric.InstrumentOption) any {
			return otx.Float64Histogram(ctx, name, instOpts[metric.Float64HistogramOption](opts)...)
		},
		record: func(ctx context.Context, v any) { v.(metric.Float64Histogram).Record(ctx, 3) },
	},
	{
		kind:  "Float64Gauge",
		shape: "gauge[float64]",
		new_otx: func(x *otx.Otx, name string, opts ...metric.InstrumentOption) any {
			return x.Float64Gauge(name, instOpts[metric.Float64GaugeOption](opts)...)
		},
		new_ctx: func(ctx context.Context, name string, opts ...metric.InstrumentOption) any {
			return otx.Float64Gauge(ctx, name, instOpts[metric.Float64GaugeOption](opts)...)
		},
		record: func(ctx context.Context, v any) { v.(metric.Float64Gauge).Record(ctx, 3) },
	},
}

// instOpts converts the options a case carries into the option type one of the
// eight constructors takes. Both options used here, [metric.WithDescription]
// and [metric.WithUnit], are declared as a [metric.InstrumentOption], which is
// the interface satisfying all eight.
func instOpts[T any](opts []metric.InstrumentOption) []T {
	out := make([]T, len(opts))
	for i, o := range opts {
		out[i] = any(o).(T)
	}

	return out
}

// instName gives each kind a name of its own, so that one meter can serve the
// whole table without the eight instruments of a subtest colliding.
func instName(prefix string, kind string) string {
	return prefix + "." + kind
}

// addrOf is the address the given instrument lives at.
//
// [require.NotSame] holds for free on two pointers of different types - it
// reports "not the same object" without looking at the addresses at all - and
// most of the pairs compared here are of different types, one per kind. Where
// the point is that two instruments are distinct, compare the addresses.
func addrOf(t *testing.T, v any) uintptr {
	t.Helper()

	rv := reflect.ValueOf(v)
	require.Equal(t, reflect.Pointer, rv.Kind(), "%T is not a pointer, so its identity cannot be compared", v)

	return rv.Pointer()
}

// metricsNamed returns every collected metric of the given name. There is more
// than one only where a name was declared in two kinds at once.
func metricsNamed(t *testing.T, h *otxtest.Harness, ctx context.Context, name string) []metricdata.Metrics {
	t.Helper()

	var got []metricdata.Metrics
	for _, sm := range h.Collect(ctx).ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				got = append(got, m)
			}
		}
	}

	return got
}

// metricNamed returns the single collected metric of the given name.
func metricNamed(t *testing.T, h *otxtest.Harness, ctx context.Context, name string) metricdata.Metrics {
	t.Helper()

	got := metricsNamed(t, h, ctx, name)
	require.Len(t, got, 1, "metrics named %q", name)

	return got[0]
}

// datumOf reports the shape of an aggregation and the value of its single data
// point, so that a case can state which of the eight shapes its kind produces
// without a type switch of its own.
func datumOf(t *testing.T, a metricdata.Aggregation) (string, float64) {
	t.Helper()

	switch v := a.(type) {
	case metricdata.Sum[int64]:
		require.Len(t, v.DataPoints, 1)
		if v.IsMonotonic {
			return "monotonic sum[int64]", float64(v.DataPoints[0].Value)
		}

		return "sum[int64]", float64(v.DataPoints[0].Value)

	case metricdata.Sum[float64]:
		require.Len(t, v.DataPoints, 1)
		if v.IsMonotonic {
			return "monotonic sum[float64]", v.DataPoints[0].Value
		}

		return "sum[float64]", v.DataPoints[0].Value

	case metricdata.Gauge[int64]:
		require.Len(t, v.DataPoints, 1)
		return "gauge[int64]", float64(v.DataPoints[0].Value)

	case metricdata.Gauge[float64]:
		require.Len(t, v.DataPoints, 1)
		return "gauge[float64]", v.DataPoints[0].Value

	case metricdata.Histogram[int64]:
		require.Len(t, v.DataPoints, 1)
		return "histogram[int64]", float64(v.DataPoints[0].Sum)

	case metricdata.Histogram[float64]:
		require.Len(t, v.DataPoints, 1)
		return "histogram[float64]", v.DataPoints[0].Sum

	default:
		t.Fatalf("unexpected aggregation %T", a)
		return "", 0
	}
}

func TestInstrument(t *testing.T) {
	t.Run("every kind records a measurement that reaches the reader", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		for _, tc := range instrument_cases {
			t.Run(tc.kind, func(t *testing.T) {
				name := instName("record", tc.kind)

				v := tc.new_otx(h.Otx, name)
				require.NotNil(t, v)

				tc.record(ctx, v)

				m := metricNamed(t, h, ctx, name)
				shape, datum := datumOf(t, m.Data)
				require.Equal(t, tc.shape, shape)
				require.Equal(t, 3.0, datum)
			})
		}
	})
	t.Run("the description and unit reach the sdk", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		for _, tc := range instrument_cases {
			t.Run(tc.kind, func(t *testing.T) {
				name := instName("described", tc.kind)

				v := tc.new_otx(h.Otx, name,
					metric.WithDescription("how many "+tc.kind),
					metric.WithUnit("{thing}"),
				)
				tc.record(ctx, v)

				m := metricNamed(t, h, ctx, name)
				require.Equal(t, "how many "+tc.kind, m.Description)
				require.Equal(t, "{thing}", m.Unit)
			})
		}
	})
	t.Run("a second call with the same name returns the identical instrument", func(t *testing.T) {
		// End to end, through a real meter. It holds here even for a meter that
		// caches nothing, which is what the next subtest pins down.
		h := otxtest.New(t)

		for _, tc := range instrument_cases {
			t.Run(tc.kind, func(t *testing.T) {
				name := instName("cached", tc.kind)

				a := tc.new_otx(h.Otx, name)
				b := tc.new_otx(h.Otx, name)
				require.NotNil(t, a)
				require.Same(t, a, b)
			})
		}
	})
	t.Run("the meter is asked once per name and kind", func(t *testing.T) {
		// The point of the cache: the meter's lock is taken on the first call
		// and never again. A meter that hands out a fresh instrument every time
		// is what tells this apart from the SDK meter's own cache, which would
		// make the assertion above pass with no cache here at all.
		mp := &countingMeterProvider{}
		x := otx.New(otx.WithMeterProvider(mp))

		for _, tc := range instrument_cases {
			t.Run(tc.kind, func(t *testing.T) {
				name := instName("once", tc.kind)

				a := tc.new_otx(x, name)
				b := tc.new_otx(x, name)
				c := tc.new_otx(x, name)

				require.NotNil(t, a)
				require.Same(t, a, b)
				require.Same(t, a, c)
				require.Equal(t, 1, mp.meter.count(tc.kind, name))
			})
		}
	})
	t.Run("the cache is keyed by kind as well as name", func(t *testing.T) {
		mp := &countingMeterProvider{}
		x := otx.New(otx.WithMeterProvider(mp))

		// One name declared in all eight kinds at once.
		got := make([]any, len(instrument_cases))
		for i, tc := range instrument_cases {
			got[i] = tc.new_otx(x, "keyed")
			require.Equal(t, 1, mp.meter.count(tc.kind, "keyed"))
		}

		// By address, not by NotSame: the eight are of eight different types,
		// which testify calls "not the same object" whatever they hold.
		at := map[uintptr]string{}
		for i, v := range got {
			p := addrOf(t, v)

			prev, ok := at[p]
			require.False(t, ok, "%s and %s are the one instrument", prev, instrument_cases[i].kind)
			at[p] = instrument_cases[i].kind
		}

		// And each key still holds what it stored: a second round hits the
		// cache of its own kind, and no kind's entry was overwritten by a
		// later one.
		for i, tc := range instrument_cases {
			require.Same(t, got[i], tc.new_otx(x, "keyed"), tc.kind)
			require.Equal(t, 1, mp.meter.count(tc.kind, "keyed"), tc.kind)
		}
	})
	t.Run("the cache is keyed by name as well as kind", func(t *testing.T) {
		mp := &countingMeterProvider{}
		x := otx.New(otx.WithMeterProvider(mp))

		for _, tc := range instrument_cases {
			t.Run(tc.kind, func(t *testing.T) {
				a := tc.new_otx(x, instName("x", tc.kind))
				b := tc.new_otx(x, instName("y", tc.kind))
				require.NotSame(t, a, b)
			})
		}
	})
	t.Run("two kinds of one name are two working instruments", func(t *testing.T) {
		// Declaring one name in two kinds is a duplicate registration that the
		// SDK warns about and then serves anyway, as two streams. What matters
		// here is that the cache does not collapse them into one.
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		counter := h.Otx.Int64Counter("two.kinds")
		histogram := h.Otx.Int64Histogram("two.kinds")
		require.NotEqual(t, addrOf(t, counter), addrOf(t, histogram))

		counter.Add(ctx, 3)
		histogram.Record(ctx, 3)

		got := metricsNamed(t, h, ctx, "two.kinds")
		require.Len(t, got, 2)

		shapes := make([]string, len(got))
		for i, m := range got {
			shape, datum := datumOf(t, m.Data)
			shapes[i] = shape
			require.Equal(t, 3.0, datum)
		}
		require.ElementsMatch(t, []string{"monotonic sum[int64]", "histogram[int64]"}, shapes)
	})
	t.Run("two names of one kind are two working instruments", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		x := h.Otx.Int64Counter("name.x")
		y := h.Otx.Int64Counter("name.y")
		require.NotSame(t, x, y)

		x.Add(ctx, 3)
		y.Add(ctx, 7)

		_, datum_x := datumOf(t, metricNamed(t, h, ctx, "name.x").Data)
		_, datum_y := datumOf(t, metricNamed(t, h, ctx, "name.y").Data)
		require.Equal(t, 3.0, datum_x)
		require.Equal(t, 7.0, datum_y)
	})
	t.Run("instruments are not shared between two Otx", func(t *testing.T) {
		// The cache hangs off the Otx, not off the meter, so two Otx over one
		// meter each ask for their own.
		mp := &countingMeterProvider{}
		a := otx.New(otx.WithMeterProvider(mp))
		b := otx.New(otx.WithMeterProvider(mp))
		require.Same(t, a.Meter(), b.Meter())

		for _, tc := range instrument_cases {
			t.Run(tc.kind, func(t *testing.T) {
				name := instName("per-otx", tc.kind)

				require.NotSame(t, tc.new_otx(a, name), tc.new_otx(b, name))
				require.Equal(t, 2, mp.meter.count(tc.kind, name))
			})
		}
	})
	t.Run("a second call with a different description keeps the first", func(t *testing.T) {
		// The documented trade-off, not an accident: the key is the name and
		// the kind alone, so the options of every call after the first are
		// dropped. OpenTelemetry requires one name to be declared identically
		// everywhere anyway - differing declarations are a duplicate
		// registration - so the cost of the cheaper key is that a caller that
		// breaks that rule is not told, and gets the first declaration.
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		for _, tc := range instrument_cases {
			t.Run(tc.kind, func(t *testing.T) {
				name := instName("caveat", tc.kind)

				first := tc.new_otx(h.Otx, name,
					metric.WithDescription("first"),
					metric.WithUnit("{first}"),
				)
				second := tc.new_otx(h.Otx, name,
					metric.WithDescription("second"),
					metric.WithUnit("{second}"),
				)
				require.Same(t, first, second)

				tc.record(ctx, second)

				// One stream, not two: the meter was never asked for the second
				// declaration, so the SDK never saw a conflict to report.
				m := metricNamed(t, h, ctx, name)
				require.Equal(t, "first", m.Description)
				require.Equal(t, "{first}", m.Unit)
			})
		}
	})
	t.Run("the package level form delegates to the Otx in the context", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		for _, tc := range instrument_cases {
			t.Run(tc.kind, func(t *testing.T) {
				name := instName("package", tc.kind)

				v := tc.new_ctx(ctx, name)
				require.NotNil(t, v)

				// The method form now hits the cache the package level form
				// filled, which is only true if both went to the same Otx.
				require.Same(t, v, tc.new_otx(h.Otx, name))

				tc.record(ctx, v)

				m := metricNamed(t, h, ctx, name)
				shape, datum := datumOf(t, m.Data)
				require.Equal(t, tc.shape, shape)
				require.Equal(t, 3.0, datum)
			})
		}
	})
	t.Run("the package level form passes its options through", func(t *testing.T) {
		// Delegation alone does not say the options survived the trip: a
		// package level form that forwarded the name and dropped opts would
		// still hand back the instrument the subtest above asks for.
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		for _, tc := range instrument_cases {
			t.Run(tc.kind, func(t *testing.T) {
				name := instName("package-described", tc.kind)

				v := tc.new_ctx(ctx, name,
					metric.WithDescription("how many "+tc.kind),
					metric.WithUnit("{thing}"),
				)
				tc.record(ctx, v)

				m := metricNamed(t, h, ctx, name)
				require.Equal(t, "how many "+tc.kind, m.Description)
				require.Equal(t, "{thing}", m.Unit)
			})
		}
	})
	t.Run("the bucket boundaries reach the sdk", func(t *testing.T) {
		// [metric.WithExplicitBucketBoundaries] is not a
		// [metric.InstrumentOption], so it cannot go through the table above:
		// only the two histogram kinds accept it. It is the one option whose
		// effect is visible in the collected data rather than in the metadata.
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		bounds := []float64{1, 10, 100}

		// A measurement of 3 falls in the second of the four buckets those
		// three boundaries cut. The SDK's default boundaries are a different
		// list of eleven, so counting the buckets is enough to tell whether
		// the option landed.
		want_counts := []uint64{0, 1, 0, 0}

		t.Run("Int64Histogram", func(t *testing.T) {
			name := "bounds.Int64Histogram"

			v := h.Otx.Int64Histogram(name, metric.WithExplicitBucketBoundaries(bounds...))
			v.Record(ctx, 3)

			d, ok := metricNamed(t, h, ctx, name).Data.(metricdata.Histogram[int64])
			require.True(t, ok)
			require.Len(t, d.DataPoints, 1)
			require.Equal(t, bounds, d.DataPoints[0].Bounds)
			require.Equal(t, want_counts, d.DataPoints[0].BucketCounts)
		})
		t.Run("Float64Histogram", func(t *testing.T) {
			name := "bounds.Float64Histogram"

			v := h.Otx.Float64Histogram(name, metric.WithExplicitBucketBoundaries(bounds...))
			v.Record(ctx, 3)

			d, ok := metricNamed(t, h, ctx, name).Data.(metricdata.Histogram[float64])
			require.True(t, ok)
			require.Len(t, d.DataPoints, 1)
			require.Equal(t, bounds, d.DataPoints[0].Bounds)
			require.Equal(t, want_counts, d.DataPoints[0].BucketCounts)
		})
		t.Run("a second call with different boundaries keeps the first", func(t *testing.T) {
			// The same caveat as the description and the unit, for the one
			// option that is not shared by the eight kinds.
			name := "bounds.caveat"

			first := h.Otx.Float64Histogram(name, metric.WithExplicitBucketBoundaries(bounds...))
			second := h.Otx.Float64Histogram(name, metric.WithExplicitBucketBoundaries(2, 4, 8, 16))
			require.Same(t, first, second)

			second.Record(ctx, 3)

			d, ok := metricNamed(t, h, ctx, name).Data.(metricdata.Histogram[float64])
			require.True(t, ok)
			require.Len(t, d.DataPoints, 1)
			require.Equal(t, bounds, d.DataPoints[0].Bounds, "the boundaries of the first call")
			require.Equal(t, want_counts, d.DataPoints[0].BucketCounts)

			// The collected data alone does not say who dropped the second
			// declaration: the boundaries are not part of the identity the SDK
			// meter caches by, so the SDK would answer the second call with
			// the first instrument too. This says it was the cache here - the
			// meter never saw the second set of boundaries at all.
			mp := &countingMeterProvider{}
			x := otx.New(otx.WithMeterProvider(mp))

			a := x.Float64Histogram(name, metric.WithExplicitBucketBoundaries(bounds...))
			b := x.Float64Histogram(name, metric.WithExplicitBucketBoundaries(2, 4, 8, 16))
			require.Same(t, a, b)
			require.Equal(t, 1, mp.meter.count("Float64Histogram", name))
		})
	})
	t.Run("an instrument taken before the shutdown is still served after it", func(t *testing.T) {
		// The cache lives as long as the Otx and is never cleared, so a
		// shut-down Otx keeps handing out the instruments it made while it was
		// running, still pointing at providers that have stopped. What otx
		// promises even then is that the meter is not asked a second time.
		mp := &countingMeterProvider{}
		x := otx.New(otx.WithMeterProvider(mp))

		before := x.Int64Counter("shutdown.cached")
		require.NoError(t, x.Shutdown(t.Context()))

		require.Same(t, before, x.Int64Counter("shutdown.cached"))
		require.Equal(t, 1, mp.meter.count("Int64Counter", "shutdown.cached"))

		// A name first asked for after the shutdown is created like any other:
		// nothing here refuses to serve one.
		require.NotNil(t, x.Int64Counter("shutdown.fresh"))
		require.Equal(t, 1, mp.meter.count("Int64Counter", "shutdown.fresh"))
	})
	t.Run("measuring through an instrument after the shutdown does not panic", func(t *testing.T) {
		// Through a real SDK meter, since it is the SDK that decides what a
		// measurement into a stopped provider does. It drops it.
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		c := h.Otx.Int64Counter("shutdown.record")
		c.Add(ctx, 3)

		m := metricNamed(t, h, ctx, "shutdown.record")
		_, datum := datumOf(t, m.Data)
		require.Equal(t, 3.0, datum)

		require.NoError(t, h.Otx.Shutdown(ctx))
		require.NotPanics(t, func() { c.Add(ctx, 3) })
	})
	t.Run("the package level form falls back on a bare context", func(t *testing.T) {
		bare := t.Context()

		for _, tc := range instrument_cases {
			t.Run(tc.kind, func(t *testing.T) {
				// The fallback Otx is shared by the whole process, so this name
				// stays in its cache for the rest of the run.
				name := instName("fallback", tc.kind)

				v := tc.new_ctx(bare, name)
				require.NotNil(t, v)
				require.NotPanics(t, func() { tc.record(bare, v) })

				// It came from the shared fallback, which caches like any
				// other Otx, so every bare context sees the one instrument.
				require.Same(t, v, tc.new_otx(otx.From(bare), name))
				require.Same(t, v, tc.new_ctx(context.Background(), name))
			})
		}
	})
}

// TestInstrumentInvalidName drives the error path of all eight constructors: a
// name the SDK rejects must cost one report and nothing else.
//
// otel.SetErrorHandler is process global, so the recorder is installed for the
// duration of this test only and this test must never run in parallel with
// anything - nothing in this package does.
func TestInstrumentInvalidName(t *testing.T) {
	errs := captureErrors(t)

	h := otxtest.New(t)
	ctx := h.Into(t.Context())

	for i, tc := range instrument_cases {
		t.Run(tc.kind, func(t *testing.T) {
			// The SDK requires a name to start with a letter and to hold only
			// [A-Za-z0-9_.-/]; this one starts with a digit. It deliberately
			// does not mention the kind, which the report has to supply itself.
			name := fmt.Sprintf("1bad.%d", i)

			errs.Reset()

			v := tc.new_otx(h.Otx, name)
			require.NotNil(t, v, "a rejected name still yields an instrument, never nil")

			got := errs.Errors()
			require.Len(t, got, 1, "reported exactly once")
			require.ErrorIs(t, got[0], sdkmetric.ErrInstrumentName, "wrapping the reason the SDK gave")
			require.ErrorContains(t, got[0], fmt.Sprintf("otx: %s %q:", tc.kind, name), "naming the kind and the instrument")

			// The instrument that could not be created is cached like any
			// other, so a bad name costs one report rather than one per call.
			errs.Reset()

			again := tc.new_otx(h.Otx, name)
			require.Same(t, v, again)
			require.Empty(t, errs.Errors(), "the second call reports nothing")

			// And it is usable, so a bad name is not a nil dereference at the
			// first measurement either. The SDK builds the instrument before it
			// validates the name, so the measurement is even collected.
			require.NotPanics(t, func() { tc.record(ctx, v) })

			m := metricNamed(t, h, ctx, name)
			shape, datum := datumOf(t, m.Data)
			require.Equal(t, tc.shape, shape)
			require.Equal(t, 3.0, datum)
		})
	}

	t.Run("two callers racing on one bad name each pay for a report", func(t *testing.T) {
		// The bound on the reports is the cache, and the cache is only filled
		// once a constructor has returned, so callers that overlap inside the
		// meter each report. "One report rather than one per call" holds for
		// the uncontended case, which is every call after the first two here.
		//
		// Both callers are held inside the meter until the other has arrived,
		// so this is the race itself rather than a chance of it, and no sleep.
		meter := &countingMeter{
			err:     errors.New("no instrument for you"),
			enter:   make(chan struct{}, 2),
			release: make(chan struct{}),
		}
		x := otx.New(otx.WithMeterProvider(&countingMeterProvider{meter: meter}))

		errs.Reset()

		got := make([]metric.Int64Counter, 2)

		var wg sync.WaitGroup
		for i := range got {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got[i] = x.Int64Counter("raced.bad")
			}()
		}

		<-meter.enter
		<-meter.enter
		close(meter.release)
		wg.Wait()

		// One instrument still, whatever the reports say.
		require.NotNil(t, got[0])
		require.Same(t, got[0], got[1])

		reported := errs.Errors()
		require.Len(t, reported, 2, "one report per racing caller")
		for i, err := range reported {
			require.ErrorContains(t, err, `otx: Int64Counter "raced.bad": no instrument for you`, i)
		}

		// And the race is over: every later caller hits the cache, so the
		// racers are all it ever costs.
		errs.Reset()

		require.Same(t, got[0], x.Int64Counter("raced.bad"))
		require.Empty(t, errs.Errors(), "a call after the race reports nothing")
		require.Equal(t, 2, meter.count("Int64Counter", "raced.bad"))
	})
	t.Run("a valid name reports nothing at all", func(t *testing.T) {
		// The control: without it every assertion above would hold for a
		// recorder that was never wired up to anything.
		for _, tc := range instrument_cases {
			errs.Reset()

			require.NotNil(t, tc.new_otx(h.Otx, instName("valid", tc.kind)))
			require.Empty(t, errs.Errors(), tc.kind)
		}
	})
}

// TestInstrumentNilFromMeter documents what a constructor does when a meter
// breaks the assumption the cache rests on: that a meter reporting an error
// still hands back an instrument. The SDK always does - it builds the
// instrument before it validates the name - but nothing in the metric API
// requires it of a mock, of a decorator, or of another implementation.
//
// What happens is a panic, from the type assertion that unwraps the cached
// value, after the error has been reported. This pins the behaviour as it is
// today; it is reported as a finding rather than fixed here.
func TestInstrumentNilFromMeter(t *testing.T) {
	// The SDK builds the instrument before validating the name, so it never
	// returns nil. A Meter that follows the ordinary "nil, err" convention -
	// a mock, a decorator, another implementation - must not turn one bad name
	// into a panic on every use.
	errs := captureErrors(t)

	x := otx.New(otx.WithMeterProvider(nilMeterProvider{}))

	t.Run("the error is reported and a no-op instrument stands in", func(t *testing.T) {
		var c metric.Int64Counter
		require.NotPanics(t, func() { c = x.Int64Counter("nil.instrument") })
		require.NotNil(t, c)
		require.NotPanics(t, func() { c.Add(t.Context(), 1) })

		got := errs.Errors()
		require.Len(t, got, 1)
		require.ErrorContains(t, got[0], `otx: Int64Counter "nil.instrument"`)
	})
	t.Run("the stand-in is cached, so no later call reports or panics", func(t *testing.T) {
		errs.Reset()

		var c metric.Int64Counter
		require.NotPanics(t, func() { c = x.Int64Counter("nil.instrument") })
		require.NotNil(t, c)
		require.Empty(t, errs.Errors(), "the second call reports nothing at all")
	})
	t.Run("every kind stands in rather than handing back nil", func(t *testing.T) {
		errs.Reset()

		require.NotNil(t, x.Int64UpDownCounter("nil.a"))
		require.NotNil(t, x.Int64Histogram("nil.b"))
		require.NotNil(t, x.Int64Gauge("nil.c"))
		require.NotNil(t, x.Float64Counter("nil.d"))
		require.NotNil(t, x.Float64UpDownCounter("nil.e"))
		require.NotNil(t, x.Float64Histogram("nil.f"))
		require.NotNil(t, x.Float64Gauge("nil.g"))
		require.Len(t, errs.Errors(), 7)
	})
}

// TestInstrumentConcurrentUse is worth little without -race and a great deal
// with it: the cache is the only mutable state on an Otx, and every other test
// here drives it from a single goroutine.
func TestInstrumentConcurrentUse(t *testing.T) {
	const n = 16

	h := otxtest.New(t)
	ctx := h.Into(t.Context())

	t.Run("every goroutine gets the one instrument", func(t *testing.T) {
		for _, tc := range instrument_cases {
			t.Run(tc.kind, func(t *testing.T) {
				name := instName("concurrent", tc.kind)

				// The goroutines only collect what they observed; the
				// assertions are made here afterwards, since require stops the
				// goroutine it runs on rather than the test.
				got := make([]any, n)

				var wg sync.WaitGroup
				for i := range n {
					wg.Add(1)
					go func() {
						defer wg.Done()

						// Both entry points at once, since they share a cache.
						var v any
						if i%2 == 0 {
							v = tc.new_otx(h.Otx, name)
						} else {
							v = tc.new_ctx(ctx, name)
						}

						tc.record(ctx, v)
						got[i] = v
					}()
				}
				wg.Wait()

				for i, v := range got {
					require.NotNil(t, v, i)
					require.Same(t, got[0], v, i)
				}

				m := metricNamed(t, h, ctx, name)
				shape, datum := datumOf(t, m.Data)
				require.Equal(t, tc.shape, shape)
				if tc.accumulates() {
					require.Equal(t, 3.0*n, datum, "every goroutine's measurement landed")
				} else {
					require.Equal(t, 3.0, datum, "a gauge keeps the last measurement")
				}
			})
		}
	})
	t.Run("two callers that race to create agree on one instrument", func(t *testing.T) {
		// Both callers are held inside the meter until the other has arrived,
		// so each of them really does miss the cache and build an instrument of
		// its own - the race itself, not a chance of it, and no sleep.
		//
		// What they get back has to be the winner of the store. Handing each
		// caller the instrument it happened to build would leave the loser
		// recording into a stream nothing collects, and would make the
		// identity every other test here asserts hold only when the first two
		// callers did not overlap.
		meter := &countingMeter{
			enter:   make(chan struct{}, 2),
			release: make(chan struct{}),
		}
		x := otx.New(otx.WithMeterProvider(&countingMeterProvider{meter: meter}))

		got := make([]metric.Int64Counter, 2)

		var wg sync.WaitGroup
		for i := range got {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got[i] = x.Int64Counter("raced")
			}()
		}

		<-meter.enter
		<-meter.enter
		close(meter.release)
		wg.Wait()

		require.NotNil(t, got[0])
		require.Same(t, got[0], got[1])

		// Both did build one: the cache keeps the meter from being asked twice
		// for a name it has already served, not from being asked twice at once.
		require.Equal(t, 2, meter.count("Int64Counter", "raced"))
	})
}

// errorRecorder collects what otx hands to [otel.Handle].
type errorRecorder struct {
	mu   sync.Mutex
	errs []error
	off  bool
}

func (h *errorRecorder) Handle(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.off {
		return
	}

	h.errs = append(h.errs, err)
}

func (h *errorRecorder) Errors() []error {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]error(nil), h.errs...)
}

func (h *errorRecorder) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.errs = nil
}

// captureErrors installs a recorder as the OpenTelemetry error handler for the
// duration of the test and restores the previous one afterwards.
//
// The handler is process global, and the first one a process installs stays
// wired to the default delegator for good, so the recorder is switched off as
// well as uninstalled: an error handled by a later test must not land in a
// slice nothing reads any more.
func captureErrors(tb testing.TB) *errorRecorder {
	tb.Helper()

	prev := otel.GetErrorHandler()
	h := &errorRecorder{}
	otel.SetErrorHandler(h)

	tb.Cleanup(func() {
		otel.SetErrorHandler(prev)

		h.mu.Lock()
		defer h.mu.Unlock()
		h.off = true
	})

	return h
}

// countingMeter hands out a freshly allocated instrument on every call and
// counts the calls per kind and name. It is what tells "the Otx cached it"
// apart from "the SDK meter cached it": the SDK keeps a cache of its own, keyed
// by the full identity of the instrument, so pointer identity alone would hold
// even with no cache here at all.
type countingMeter struct {
	metricnoop.Meter

	mu sync.Mutex
	n  map[string]int

	// err, when set, is returned alongside every instrument, the way the SDK
	// reports a name it rejects.
	err error

	// enter and release, when set, hold every caller inside the meter until
	// the test lets it out: the meter reports its arrival on enter and then
	// waits for release to be closed.
	enter   chan struct{}
	release chan struct{}
}

func (m *countingMeter) called(kind string, name string) error {
	m.mu.Lock()
	if m.n == nil {
		m.n = map[string]int{}
	}
	m.n[kind+" "+name]++
	m.mu.Unlock()

	if m.enter == nil {
		return m.err
	}

	m.enter <- struct{}{}
	<-m.release

	return m.err
}

// count reports how many times the meter was asked for an instrument of the
// given kind and name.
func (m *countingMeter) count(kind string, name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.n[kind+" "+name]
}

type countingInt64Counter struct{ metricnoop.Int64Counter }

func (m *countingMeter) Int64Counter(name string, opts ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	return &countingInt64Counter{}, m.called("Int64Counter", name)
}

type countingInt64UpDownCounter struct{ metricnoop.Int64UpDownCounter }

func (m *countingMeter) Int64UpDownCounter(name string, opts ...metric.Int64UpDownCounterOption) (metric.Int64UpDownCounter, error) {
	return &countingInt64UpDownCounter{}, m.called("Int64UpDownCounter", name)
}

type countingInt64Histogram struct{ metricnoop.Int64Histogram }

func (m *countingMeter) Int64Histogram(name string, opts ...metric.Int64HistogramOption) (metric.Int64Histogram, error) {
	return &countingInt64Histogram{}, m.called("Int64Histogram", name)
}

type countingInt64Gauge struct{ metricnoop.Int64Gauge }

func (m *countingMeter) Int64Gauge(name string, opts ...metric.Int64GaugeOption) (metric.Int64Gauge, error) {
	return &countingInt64Gauge{}, m.called("Int64Gauge", name)
}

type countingFloat64Counter struct{ metricnoop.Float64Counter }

func (m *countingMeter) Float64Counter(name string, opts ...metric.Float64CounterOption) (metric.Float64Counter, error) {
	return &countingFloat64Counter{}, m.called("Float64Counter", name)
}

type countingFloat64UpDownCounter struct {
	metricnoop.Float64UpDownCounter
}

func (m *countingMeter) Float64UpDownCounter(name string, opts ...metric.Float64UpDownCounterOption) (metric.Float64UpDownCounter, error) {
	return &countingFloat64UpDownCounter{}, m.called("Float64UpDownCounter", name)
}

type countingFloat64Histogram struct{ metricnoop.Float64Histogram }

func (m *countingMeter) Float64Histogram(name string, opts ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return &countingFloat64Histogram{}, m.called("Float64Histogram", name)
}

type countingFloat64Gauge struct{ metricnoop.Float64Gauge }

func (m *countingMeter) Float64Gauge(name string, opts ...metric.Float64GaugeOption) (metric.Float64Gauge, error) {
	return &countingFloat64Gauge{}, m.called("Float64Gauge", name)
}

// nilMeter reports an error and returns no instrument, the shape of a meter
// that follows the "nil, err" convention of ordinary Go rather than the one
// the metric SDK follows.
// nilMeter follows the ordinary Go "nil, err" convention rather than the SDK's
// "usable instrument alongside the error", which is the case the constructors
// have to stand in for.
type nilMeter struct{ metricnoop.Meter }

var errNoInstrument = errors.New("no instrument for you")

func (m nilMeter) Int64Counter(name string, opts ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	return nil, errNoInstrument
}

func (m nilMeter) Int64UpDownCounter(name string, opts ...metric.Int64UpDownCounterOption) (metric.Int64UpDownCounter, error) {
	return nil, errNoInstrument
}

func (m nilMeter) Int64Histogram(name string, opts ...metric.Int64HistogramOption) (metric.Int64Histogram, error) {
	return nil, errNoInstrument
}

func (m nilMeter) Int64Gauge(name string, opts ...metric.Int64GaugeOption) (metric.Int64Gauge, error) {
	return nil, errNoInstrument
}

func (m nilMeter) Float64Counter(name string, opts ...metric.Float64CounterOption) (metric.Float64Counter, error) {
	return nil, errNoInstrument
}

func (m nilMeter) Float64UpDownCounter(name string, opts ...metric.Float64UpDownCounterOption) (metric.Float64UpDownCounter, error) {
	return nil, errNoInstrument
}

func (m nilMeter) Float64Histogram(name string, opts ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return nil, errNoInstrument
}

func (m nilMeter) Float64Gauge(name string, opts ...metric.Float64GaugeOption) (metric.Float64Gauge, error) {
	return nil, errNoInstrument
}

type nilMeterProvider struct{ metricnoop.MeterProvider }

func (p nilMeterProvider) Meter(name string, opts ...metric.MeterOption) metric.Meter {
	return nilMeter{}
}

// countingMeterProvider hands the same countingMeter to every Otx built on it,
// so that two Otx sharing a meter can be told apart from two Otx sharing a
// cache.
type countingMeterProvider struct {
	metricnoop.MeterProvider

	meter *countingMeter
}

func (p *countingMeterProvider) Meter(name string, opts ...metric.MeterOption) metric.Meter {
	if p.meter == nil {
		p.meter = &countingMeter{}
	}

	return p.meter
}
