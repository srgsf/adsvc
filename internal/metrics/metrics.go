// Package metrics is a small Prometheus client: counters, gauges and
// histograms (plain or with fixed label names), values computed at scrape
// time, and the text exposition format (0.0.4).
//
// Metrics register in one process-wide registry when they are declared,
// usually as package-level variables next to the code they measure, and
// Handler serves them all. Updates are atomic and allocate nothing, apart
// from the first use of a label tuple.
//
// Label values must come from fixed sets or from the configuration: never
// URLs, file keys, tokens or user names.
package metrics

import (
	"bytes"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// family is one metric name with its HELP and TYPE lines and its samples.
type family interface {
	meta() *desc
	// write appends the samples (no HELP/TYPE lines).
	write(b *bytes.Buffer)
}

type desc struct {
	name, help, typ string
	labels          []string
}

func (d *desc) meta() *desc { return d }

var (
	regMu sync.Mutex
	reg   = map[string]family{}
)

var nameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

func register(f family) {
	d := f.meta()
	if !nameRE.MatchString(d.name) {
		panic("metrics: invalid name " + strconv.Quote(d.name))
	}
	for _, l := range d.labels {
		if !nameRE.MatchString(l) || strings.HasPrefix(l, "__") || l == "le" {
			panic("metrics: invalid label " + strconv.Quote(l) + " on " + d.name)
		}
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := reg[d.name]; dup {
		panic("metrics: duplicate metric " + d.name)
	}
	reg[d.name] = f
}

// Handler serves every registered metric in the text format.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write(Text())
	})
}

// Text renders every registered metric, sorted by name.
func Text() []byte {
	regMu.Lock()
	fams := make([]family, 0, len(reg))
	for _, f := range reg {
		fams = append(fams, f)
	}
	regMu.Unlock()
	slices.SortFunc(fams, func(a, b family) int { return strings.Compare(a.meta().name, b.meta().name) })

	var b bytes.Buffer
	for _, f := range fams {
		d := f.meta()
		n := b.Len()
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", d.name, escapeHelp(d.help), d.name, d.typ)
		m := b.Len()
		f.write(&b)
		if b.Len() == m { // no samples (an unused vector): leave the family out
			b.Truncate(n)
		}
	}
	return b.Bytes()
}

// value is a float64 updated atomically.
type value struct{ bits atomic.Uint64 }

func (v *value) load() float64   { return math.Float64frombits(v.bits.Load()) }
func (v *value) store(f float64) { v.bits.Store(math.Float64bits(f)) }
func (v *value) add(f float64) {
	for {
		old := v.bits.Load()
		if v.bits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+f)) {
			return
		}
	}
}

// Counter only goes up.
type Counter struct{ v value }

// Inc adds 1.
func (c *Counter) Inc() { c.v.add(1) }

// Add adds d, which must not be negative (negative values are ignored).
func (c *Counter) Add(d float64) {
	if d > 0 {
		c.v.add(d)
	}
}

// Value returns the current value.
func (c *Counter) Value() float64 { return c.v.load() }

// Gauge goes up and down.
type Gauge struct{ v value }

// Set sets the value.
func (g *Gauge) Set(f float64) { g.v.store(f) }

// Add adds d (which may be negative).
func (g *Gauge) Add(d float64) { g.v.add(d) }

// Inc adds 1.
func (g *Gauge) Inc() { g.v.add(1) }

// Dec subtracts 1.
func (g *Gauge) Dec() { g.v.add(-1) }

// Value returns the current value.
func (g *Gauge) Value() float64 { return g.v.load() }

// Histogram counts observations into fixed buckets.
type Histogram struct {
	upper  []float64       // bucket upper bounds, ascending, +Inf excluded
	le     []string        // their le labels, "+Inf" last
	counts []atomic.Uint64 // per bucket (not cumulative), the last one is +Inf
	sum    value
}

func newHistogram(buckets []float64) *Histogram {
	if !slices.IsSorted(buckets) || len(buckets) == 0 {
		panic("metrics: histogram buckets must be ascending and non-empty")
	}
	b := slices.Clone(buckets)
	if math.IsInf(b[len(b)-1], 1) {
		b = b[:len(b)-1]
	}
	return &Histogram{upper: b, le: leLabels(b), counts: make([]atomic.Uint64, len(b)+1)}
}

// leLabels are the le label values of buckets, and +Inf.
func leLabels(upper []float64) []string {
	le := make([]string, len(upper)+1)
	for i, u := range upper {
		le[i] = formatFloat(u)
	}
	le[len(upper)] = "+Inf"
	return le
}

// Observe records one value.
func (h *Histogram) Observe(f float64) {
	i, _ := slices.BinarySearch(h.upper, f) // first bound >= f; le is inclusive
	h.counts[i].Add(1)
	h.sum.add(f)
}

// ObserveDuration records d in seconds.
func (h *Histogram) ObserveDuration(d time.Duration) { h.Observe(d.Seconds()) }

// Since records the time elapsed since t in seconds.
func (h *Histogram) Since(t time.Time) { h.Observe(time.Since(t).Seconds()) }

// Count returns the number of observations.
func (h *Histogram) Count() uint64 {
	var n uint64
	for i := range h.counts {
		n += h.counts[i].Load()
	}
	return n
}

func (h *Histogram) writeTo(b *bytes.Buffer, name string, names, values []string) {
	var cum uint64
	for i := range h.counts {
		cum += h.counts[i].Load()
		writeSample(b, name+"_bucket", names, values, "le", h.le[i], float64(cum))
	}
	writeSample(b, name+"_sum", names, values, "", "", h.sum.load())
	writeSample(b, name+"_count", names, values, "", "", float64(cum))
}

// ExpBuckets returns n bucket bounds starting at start, each factor times
// the previous one.
func ExpBuckets(start, factor float64, n int) []float64 {
	b := make([]float64, n)
	for i := range b {
		b[i] = start
		start *= factor
	}
	return b
}

// NewCounter registers a counter.
func NewCounter(name, help string) *Counter {
	c := &Counter{}
	register(&scalar{name: name, help: help, typ: "counter", v: &c.v})
	return c
}

// NewGauge registers a gauge.
func NewGauge(name, help string) *Gauge {
	g := &Gauge{}
	register(&scalar{name: name, help: help, typ: "gauge", v: &g.v})
	return g
}

// NewHistogram registers a histogram with the given bucket upper bounds.
func NewHistogram(name, help string, buckets []float64) *Histogram {
	h := newHistogram(buckets)
	register(&histFamily{name: name, help: help, typ: "histogram", h: h})
	return h
}

type scalar struct {
	desc
	v *value
}

func (s *scalar) write(b *bytes.Buffer) { writeSample(b, s.name, nil, nil, "", "", s.v.load()) }

type histFamily struct {
	desc
	h *Histogram
}

func (f *histFamily) write(b *bytes.Buffer) { f.h.writeTo(b, f.name, nil, nil) }

// vec holds one child per label tuple.
type vec[T any] struct {
	desc
	mk       func() *T
	mu       sync.Mutex
	children map[string]*child[T]
}

type child[T any] struct {
	values []string
	m      *T
}

func (v *vec[T]) with(values []string) *T {
	if len(values) != len(v.labels) {
		panic(fmt.Sprintf("metrics: %s takes %d label values, got %d", v.name, len(v.labels), len(values)))
	}
	k := strings.Join(values, "\xff")
	v.mu.Lock()
	defer v.mu.Unlock()
	c, ok := v.children[k]
	if !ok {
		c = &child[T]{values: slices.Clone(values), m: v.mk()}
		v.children[k] = c
	}
	return c.m
}

func (v *vec[T]) sorted() []*child[T] {
	v.mu.Lock()
	cs := make([]*child[T], 0, len(v.children))
	for _, c := range v.children {
		cs = append(cs, c)
	}
	v.mu.Unlock()
	slices.SortFunc(cs, func(a, b *child[T]) int { return slices.Compare(a.values, b.values) })
	return cs
}

// CounterVec is a counter per label tuple.
type CounterVec struct{ v *vec[Counter] }

// With returns the counter for the label values, in the declared order.
func (c *CounterVec) With(values ...string) *Counter { return c.v.with(values) }

// GaugeVec is a gauge per label tuple.
type GaugeVec struct{ v *vec[Gauge] }

// With returns the gauge for the label values, in the declared order.
func (g *GaugeVec) With(values ...string) *Gauge { return g.v.with(values) }

// HistogramVec is a histogram per label tuple.
type HistogramVec struct{ v *vec[Histogram] }

// With returns the histogram for the label values, in the declared order.
func (h *HistogramVec) With(values ...string) *Histogram { return h.v.with(values) }

type counterVecFamily struct{ *vec[Counter] }

func (f counterVecFamily) write(b *bytes.Buffer) {
	for _, c := range f.sorted() {
		writeSample(b, f.name, f.labels, c.values, "", "", c.m.Value())
	}
}

type gaugeVecFamily struct{ *vec[Gauge] }

func (f gaugeVecFamily) write(b *bytes.Buffer) {
	for _, c := range f.sorted() {
		writeSample(b, f.name, f.labels, c.values, "", "", c.m.Value())
	}
}

type histVecFamily struct{ *vec[Histogram] }

func (f histVecFamily) write(b *bytes.Buffer) {
	for _, c := range f.sorted() {
		c.m.writeTo(b, f.name, f.labels, c.values)
	}
}

func newVec[T any](name, help, typ string, labels []string, mk func() *T) *vec[T] {
	return &vec[T]{
		name: name, help: help, typ: typ, labels: slices.Clone(labels),
		mk:       mk,
		children: map[string]*child[T]{},
	}
}

// NewCounterVec registers a counter with the given label names.
func NewCounterVec(name, help string, labels ...string) *CounterVec {
	v := newVec(name, help, "counter", labels, func() *Counter { return &Counter{} })
	register(counterVecFamily{v})
	return &CounterVec{v}
}

// NewGaugeVec registers a gauge with the given label names.
func NewGaugeVec(name, help string, labels ...string) *GaugeVec {
	v := newVec(name, help, "gauge", labels, func() *Gauge { return &Gauge{} })
	register(gaugeVecFamily{v})
	return &GaugeVec{v}
}

// NewHistogramVec registers a histogram with the given buckets and label names.
func NewHistogramVec(name, help string, buckets []float64, labels ...string) *HistogramVec {
	newHistogram(buckets) // validate now, not on first use
	v := newVec(name, help, "histogram", labels, func() *Histogram { return newHistogram(buckets) })
	register(histVecFamily{v})
	return &HistogramVec{v}
}

// Emit reports one sample of a func metric: its value and label values.
type Emit func(v float64, labelValues ...string)

type funcFamily struct {
	desc
	fn func(Emit)
}

func (f *funcFamily) write(b *bytes.Buffer) {
	f.fn(func(v float64, values ...string) {
		if len(values) != len(f.labels) {
			panic(fmt.Sprintf("metrics: %s takes %d label values, got %d", f.name, len(f.labels), len(values)))
		}
		writeSample(b, f.name, f.labels, values, "", "", v)
	})
}

// NewGaugeFunc registers a gauge whose samples fn reports at scrape time.
// fn must be cheap and safe to call concurrently.
func NewGaugeFunc(name, help string, fn func(Emit), labels ...string) {
	register(&funcFamily{name: name, help: help, typ: "gauge", labels: slices.Clone(labels), fn: fn})
}

// NewCounterFunc is NewGaugeFunc for values that only go up.
func NewCounterFunc(name, help string, fn func(Emit), labels ...string) {
	register(&funcFamily{name: name, help: help, typ: "counter", labels: slices.Clone(labels), fn: fn})
}

// Value is a func metric of one unlabelled sample.
func Value(fn func() float64) func(Emit) { return func(e Emit) { e(fn()) } }

func writeSample(b *bytes.Buffer, name string, names, values []string, extraName, extraValue string, v float64) {
	b.WriteString(name)
	if len(names) > 0 || extraName != "" {
		b.WriteByte('{')
		sep := false
		for i, n := range names {
			if sep {
				b.WriteByte(',')
			}
			writeLabel(b, n, values[i])
			sep = true
		}
		if extraName != "" {
			if sep {
				b.WriteByte(',')
			}
			writeLabel(b, extraName, extraValue)
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(formatFloat(v))
	b.WriteByte('\n')
}

func writeLabel(b *bytes.Buffer, name, value string) {
	b.WriteString(name)
	b.WriteString(`="`)
	for _, r := range value {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

func escapeHelp(s string) string { return helpEscaper.Replace(s) }

func formatFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	case math.IsNaN(f):
		return "NaN"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
