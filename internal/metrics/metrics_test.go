package metrics

import (
	"bufio"
	"bytes"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// familyText returns the lines of one metric family from Text.
func familyText(t *testing.T, name string) string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(Text()))
	for sc.Scan() {
		l := sc.Text()
		f := strings.Fields(strings.TrimPrefix(strings.TrimPrefix(l, "# HELP "), "# TYPE "))
		if len(f) == 0 {
			continue
		}
		base := f[0]
		if i := strings.IndexByte(base, '{'); i >= 0 {
			base = base[:i]
		}
		if base == name || base == name+"_bucket" || base == name+"_sum" || base == name+"_count" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func TestCounterGauge(t *testing.T) {
	c := NewCounter("test_things_total", "Things.\nDone \\ here.")
	c.Inc()
	c.Add(2.5)
	c.Add(-1) // ignored
	g := NewGauge("test_level", "Level.")
	g.Set(3)
	g.Dec()
	want := `# HELP test_things_total Things.\nDone \\ here.
# TYPE test_things_total counter
test_things_total 3.5`
	if got := familyText(t, "test_things_total"); got != want {
		t.Errorf("counter:\n%s\nwant:\n%s", got, want)
	}
	if got := familyText(t, "test_level"); !strings.HasSuffix(got, "\ntest_level 2") {
		t.Errorf("gauge:\n%s", got)
	}
}

func TestVecAndEscaping(t *testing.T) {
	v := NewCounterVec("test_requests_total", "Requests.", "handler", "code")
	v.With("b", "200").Inc()
	v.With("a", `q"\`+"\n").Add(2)
	v.With("b", "200").Inc()
	NewGaugeVec("test_unused", "Never set.", "x") // no samples: left out
	want := `# HELP test_requests_total Requests.
# TYPE test_requests_total counter
test_requests_total{handler="a",code="q\"\\\n"} 2
test_requests_total{handler="b",code="200"} 2`
	if got := familyText(t, "test_requests_total"); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := familyText(t, "test_unused"); got != "" {
		t.Errorf("empty vector rendered:\n%s", got)
	}
}

func TestHistogram(t *testing.T) {
	h := NewHistogram("test_seconds", "Latency.", []float64{0.1, 1, 10})
	for _, v := range []float64{0.05, 0.1, 0.5, 20} {
		h.Observe(v)
	}
	want := `# HELP test_seconds Latency.
# TYPE test_seconds histogram
test_seconds_bucket{le="0.1"} 2
test_seconds_bucket{le="1"} 3
test_seconds_bucket{le="10"} 3
test_seconds_bucket{le="+Inf"} 4
test_seconds_sum 20.65
test_seconds_count 4`
	if got := familyText(t, "test_seconds"); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	hv := NewHistogramVec("test_vec_seconds", "Per peer.", []float64{1}, "peer")
	hv.With("x").Observe(2)
	if got := familyText(t, "test_vec_seconds"); !strings.Contains(got, `test_vec_seconds_bucket{peer="x",le="+Inf"} 1`) {
		t.Errorf("vec:\n%s", got)
	}
}

func TestFunc(t *testing.T) {
	NewGaugeFunc("test_pool", "Pool.", func(e Emit) {
		e(1, "reader")
		e(2, "writer")
	}, "pool")
	want := `# HELP test_pool Pool.
# TYPE test_pool gauge
test_pool{pool="reader"} 1
test_pool{pool="writer"} 2`
	if got := familyText(t, "test_pool"); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRegisterPanics(t *testing.T) {
	for name, fn := range map[string]func(){
		"duplicate": func() { NewCounter("test_dup", ""); NewCounter("test_dup", "") },
		"bad name":  func() { NewCounter("test-bad", "") },
		"le label":  func() { NewCounterVec("test_le", "", "le") },
		"buckets":   func() { NewHistogram("test_buckets", "", []float64{2, 1}) },
		"arity":     func() { NewCounterVec("test_arity", "", "a").With() },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("no panic")
				}
			}()
			fn()
		})
	}
}

func TestConcurrent(t *testing.T) {
	c := NewCounter("test_concurrent_total", "")
	h := NewHistogram("test_concurrent_seconds", "", []float64{1})
	v := NewCounterVec("test_concurrent_vec_total", "", "k")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				c.Inc()
				h.Observe(0.5)
				v.With("x").Inc()
			}
		})
	}
	wg.Go(func() {
		for range 50 {
			Text()
		}
	})
	wg.Wait()
	if c.Value() != 8000 || h.Count() != 8000 || v.With("x").Value() != 8000 {
		t.Errorf("counter %v, histogram %v, vec %v", c.Value(), h.Count(), v.With("x").Value())
	}
}

// Every line of the whole output parses as the text format.
func TestHandlerFormat(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("content type %q", ct)
	}
	sample := regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{([a-zA-Z_][a-zA-Z0-9_]*="([^"\\]|\\.)*",?)*\})? (\+Inf|-Inf|NaN|[-+0-9.eE]+)$`)
	meta := regexp.MustCompile(`^# (HELP [a-zA-Z_:][a-zA-Z0-9_:]* .*|TYPE [a-zA-Z_:][a-zA-Z0-9_:]* (counter|gauge|histogram))$`)
	for l := range strings.Lines(rec.Body.String()) {
		l = strings.TrimSuffix(l, "\n")
		if !sample.MatchString(l) && !meta.MatchString(l) {
			t.Errorf("bad line %q", l)
		}
	}
	for _, n := range []string{"go_goroutines", "go_memstats_heap_alloc_bytes", "process_start_time_seconds"} {
		if !strings.Contains(rec.Body.String(), "\n"+n+" ") {
			t.Errorf("%s missing", n)
		}
	}
}

func TestExpBuckets(t *testing.T) {
	b := ExpBuckets(1, 2, 4)
	if len(b) != 4 || b[3] != 8 {
		t.Errorf("%v", b)
	}
	if formatFloat(math.Inf(1)) != "+Inf" {
		t.Error("inf")
	}
}
