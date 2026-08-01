package otx

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// The metric instrument constructors below exist because creating an
// instrument through [metric.Meter] returns an error that almost no call site
// can act on, and takes the meter's lock every time. These return the
// instrument directly, hand the error to [otel.Handle], and cache the result
// on the [Otx].
//
// The cache is keyed by name and kind, not by the description and unit as the
// SDK's own cache is. Two instruments of the same kind and name must therefore
// be declared identically, which OpenTelemetry requires anyway: differing
// declarations of one name are a duplicate registration.
//
// An instrument that could not be created is still usable and still cached: the
// SDK returns a working instrument alongside the error, and a Meter that
// returns nothing at all is replaced with a no-op. So a bad name costs a report
// when the instrument is created rather than one per use, and never a nil
// dereference. Callers that race on the first use of one name each pay for a
// report; they still agree on the instrument.

type instrumentKind uint8

const (
	kindInt64Counter instrumentKind = iota
	kindInt64UpDownCounter
	kindInt64Histogram
	kindInt64Gauge
	kindFloat64Counter
	kindFloat64UpDownCounter
	kindFloat64Histogram
	kindFloat64Gauge
)

func (k instrumentKind) String() string {
	switch k {
	case kindInt64Counter:
		return "Int64Counter"
	case kindInt64UpDownCounter:
		return "Int64UpDownCounter"
	case kindInt64Histogram:
		return "Int64Histogram"
	case kindInt64Gauge:
		return "Int64Gauge"
	case kindFloat64Counter:
		return "Float64Counter"
	case kindFloat64UpDownCounter:
		return "Float64UpDownCounter"
	case kindFloat64Histogram:
		return "Float64Histogram"
	case kindFloat64Gauge:
		return "Float64Gauge"

	default:
		return "instrument"
	}
}

type instrumentKey struct {
	kind instrumentKind
	name string
}

// instrumentOf returns the cached instrument of the given kind and name,
// creating it with create on the first call. fallback stands in for a Meter
// that returns nothing, so that nothing nil is ever cached or handed back.
func instrumentOf[T any](o *Otx, kind instrumentKind, name string, create func() (T, error), fallback T) T {
	k := instrumentKey{kind: kind, name: name}
	if v, ok := o.instruments.Load(k); ok {
		return v.(T)
	}

	v, err := create()
	if err != nil {
		otel.Handle(fmt.Errorf("otx: %s %q: %w", kind, name, err))
	}
	// The SDK builds the instrument before it validates the name, so it always
	// returns one. Another Meter - a mock, a decorator, another
	// implementation - may follow the ordinary "nil, err" convention instead,
	// and caching that nil would turn one bad name into a panic on every use.
	if any(v) == nil {
		v = fallback
	}

	actual, _ := o.instruments.LoadOrStore(k, v)

	return actual.(T)
}

// Int64Counter returns a counter of int64 measurements that only go up.
func (o *Otx) Int64Counter(name string, opts ...metric.Int64CounterOption) metric.Int64Counter {
	return instrumentOf(o, kindInt64Counter, name, func() (metric.Int64Counter, error) {
		return o.meter.Int64Counter(name, opts...)
	}, metric.Int64Counter(noop.Int64Counter{}))
}

// Int64UpDownCounter returns a counter of int64 measurements that go both ways.
func (o *Otx) Int64UpDownCounter(name string, opts ...metric.Int64UpDownCounterOption) metric.Int64UpDownCounter {
	return instrumentOf(o, kindInt64UpDownCounter, name, func() (metric.Int64UpDownCounter, error) {
		return o.meter.Int64UpDownCounter(name, opts...)
	}, metric.Int64UpDownCounter(noop.Int64UpDownCounter{}))
}

// Int64Histogram returns a histogram of int64 measurements.
func (o *Otx) Int64Histogram(name string, opts ...metric.Int64HistogramOption) metric.Int64Histogram {
	return instrumentOf(o, kindInt64Histogram, name, func() (metric.Int64Histogram, error) {
		return o.meter.Int64Histogram(name, opts...)
	}, metric.Int64Histogram(noop.Int64Histogram{}))
}

// Int64Gauge returns a gauge of int64 measurements.
func (o *Otx) Int64Gauge(name string, opts ...metric.Int64GaugeOption) metric.Int64Gauge {
	return instrumentOf(o, kindInt64Gauge, name, func() (metric.Int64Gauge, error) {
		return o.meter.Int64Gauge(name, opts...)
	}, metric.Int64Gauge(noop.Int64Gauge{}))
}

// Float64Counter returns a counter of float64 measurements that only go up.
func (o *Otx) Float64Counter(name string, opts ...metric.Float64CounterOption) metric.Float64Counter {
	return instrumentOf(o, kindFloat64Counter, name, func() (metric.Float64Counter, error) {
		return o.meter.Float64Counter(name, opts...)
	}, metric.Float64Counter(noop.Float64Counter{}))
}

// Float64UpDownCounter returns a counter of float64 measurements that go both
// ways.
func (o *Otx) Float64UpDownCounter(name string, opts ...metric.Float64UpDownCounterOption) metric.Float64UpDownCounter {
	return instrumentOf(o, kindFloat64UpDownCounter, name, func() (metric.Float64UpDownCounter, error) {
		return o.meter.Float64UpDownCounter(name, opts...)
	}, metric.Float64UpDownCounter(noop.Float64UpDownCounter{}))
}

// Float64Histogram returns a histogram of float64 measurements.
func (o *Otx) Float64Histogram(name string, opts ...metric.Float64HistogramOption) metric.Float64Histogram {
	return instrumentOf(o, kindFloat64Histogram, name, func() (metric.Float64Histogram, error) {
		return o.meter.Float64Histogram(name, opts...)
	}, metric.Float64Histogram(noop.Float64Histogram{}))
}

// Float64Gauge returns a gauge of float64 measurements.
func (o *Otx) Float64Gauge(name string, opts ...metric.Float64GaugeOption) metric.Float64Gauge {
	return instrumentOf(o, kindFloat64Gauge, name, func() (metric.Float64Gauge, error) {
		return o.meter.Float64Gauge(name, opts...)
	}, metric.Float64Gauge(noop.Float64Gauge{}))
}

// Int64Counter returns a counter of int64 measurements that only go up, from
// the Otx carried by ctx.
func Int64Counter(ctx context.Context, name string, opts ...metric.Int64CounterOption) metric.Int64Counter {
	return From(ctx).Int64Counter(name, opts...)
}

// Int64UpDownCounter returns a counter of int64 measurements that go both ways,
// from the Otx carried by ctx.
func Int64UpDownCounter(ctx context.Context, name string, opts ...metric.Int64UpDownCounterOption) metric.Int64UpDownCounter {
	return From(ctx).Int64UpDownCounter(name, opts...)
}

// Int64Histogram returns a histogram of int64 measurements, from the Otx
// carried by ctx.
func Int64Histogram(ctx context.Context, name string, opts ...metric.Int64HistogramOption) metric.Int64Histogram {
	return From(ctx).Int64Histogram(name, opts...)
}

// Int64Gauge returns a gauge of int64 measurements, from the Otx carried by
// ctx.
func Int64Gauge(ctx context.Context, name string, opts ...metric.Int64GaugeOption) metric.Int64Gauge {
	return From(ctx).Int64Gauge(name, opts...)
}

// Float64Counter returns a counter of float64 measurements that only go up,
// from the Otx carried by ctx.
func Float64Counter(ctx context.Context, name string, opts ...metric.Float64CounterOption) metric.Float64Counter {
	return From(ctx).Float64Counter(name, opts...)
}

// Float64UpDownCounter returns a counter of float64 measurements that go both
// ways, from the Otx carried by ctx.
func Float64UpDownCounter(ctx context.Context, name string, opts ...metric.Float64UpDownCounterOption) metric.Float64UpDownCounter {
	return From(ctx).Float64UpDownCounter(name, opts...)
}

// Float64Histogram returns a histogram of float64 measurements, from the Otx
// carried by ctx.
func Float64Histogram(ctx context.Context, name string, opts ...metric.Float64HistogramOption) metric.Float64Histogram {
	return From(ctx).Float64Histogram(name, opts...)
}

// Float64Gauge returns a gauge of float64 measurements, from the Otx carried by
// ctx.
func Float64Gauge(ctx context.Context, name string, opts ...metric.Float64GaugeOption) metric.Float64Gauge {
	return From(ctx).Float64Gauge(name, opts...)
}
