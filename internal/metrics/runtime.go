package metrics

import (
	"bytes"
	"math"
	"runtime"
	"runtime/metrics"
	"sync"
	"time"
)

// Go runtime metrics under the names of the Prometheus Go collector, so
// stock Go dashboards work. Read from runtime/metrics at scrape time.

const (
	rmGoroutines = "/sched/goroutines:goroutines"
	rmHeapObj    = "/memory/classes/heap/objects:bytes"
	rmHeapUnused = "/memory/classes/heap/unused:bytes"
	rmTotal      = "/memory/classes/total:bytes"
	rmHeapGoal   = "/gc/heap/goal:bytes"
	rmGCCycles   = "/gc/cycles/total:gc-cycles"
	rmGCPauses   = "/sched/pauses/total/gc:seconds"
	rmSchedLat   = "/sched/latencies:seconds"
	rmMemLimit   = "/gc/gomemlimit:bytes"
	rmGOGC       = "/gc/gogc:percent"
)

var (
	rmMu      sync.Mutex
	rmSamples = func() []metrics.Sample {
		names := []string{rmGoroutines, rmHeapObj, rmHeapUnused, rmTotal, rmHeapGoal,
			rmGCCycles, rmGCPauses, rmSchedLat, rmMemLimit, rmGOGC}
		s := make([]metrics.Sample, len(names))
		for i, n := range names {
			s[i].Name = n
		}
		return s
	}()
)

// readRuntime reads every runtime sample and returns them by name. Names
// this Go version does not know come back with KindBad and are skipped. One read serves
// every family of a scrape: a read within scrapeMemo of the last returns it again.
func readRuntime() map[string]metrics.Value {
	rmMu.Lock()
	defer rmMu.Unlock()
	if rmLast != nil && time.Since(rmAt) < scrapeMemo {
		return rmLast
	}
	metrics.Read(rmSamples)
	out := make(map[string]metrics.Value, len(rmSamples))
	for _, s := range rmSamples {
		if s.Value.Kind() != metrics.KindBad {
			out[s.Name] = s.Value
		}
	}
	rmLast, rmAt = out, time.Now()
	return out
}

// scrapeMemo is how long one reading of something expensive serves the gauges computed
// from it: long enough for one scrape, short next to any scrape interval.
const scrapeMemo = time.Second

var (
	rmLast map[string]metrics.Value
	rmAt   time.Time
)

func runtimeGauge(name, help string, fn func(map[string]metrics.Value) (float64, bool)) {
	NewGaugeFunc(name, help, func(e Emit) {
		if v, ok := fn(readRuntime()); ok {
			e(v)
		}
	})
}

func u64(names ...string) func(map[string]metrics.Value) (float64, bool) {
	return func(m map[string]metrics.Value) (float64, bool) {
		var sum float64
		for _, n := range names {
			v, ok := m[n]
			if !ok || v.Kind() != metrics.KindUint64 {
				return 0, false
			}
			sum += float64(v.Uint64())
		}
		return sum, true
	}
}

// runtimeHist re-buckets a runtime/metrics histogram into fixed bounds.
type runtimeHist struct {
	desc
	sample string
	upper  []float64
	leOnce sync.Once
	le     []string
}

func (h *runtimeHist) write(b *bytes.Buffer) {
	v, ok := readRuntime()[h.sample]
	if !ok || v.Kind() != metrics.KindFloat64Histogram {
		return
	}
	src := v.Float64Histogram()
	counts := make([]uint64, len(h.upper)+1)
	var sum float64
	for i, n := range src.Counts {
		if n == 0 {
			continue
		}
		lo, hi := src.Buckets[i], src.Buckets[i+1]
		// Place the source bucket by its upper bound; estimate the sum from
		// its midpoint (runtime histograms carry no sum).
		j := len(h.upper)
		for k, u := range h.upper {
			if hi <= u {
				j = k
				break
			}
		}
		counts[j] += n
		switch {
		case math.IsInf(lo, -1):
			sum += float64(n) * hi
		case math.IsInf(hi, 1):
			sum += float64(n) * lo
		default:
			sum += float64(n) * (lo + hi) / 2
		}
	}
	h.leOnce.Do(func() { h.le = leLabels(h.upper) })
	var cum uint64
	for i, n := range counts {
		cum += n
		writeSample(b, h.name+"_bucket", nil, nil, "le", h.le[i], float64(cum))
	}
	writeSample(b, h.name+"_sum", nil, nil, "", "", sum)
	writeSample(b, h.name+"_count", nil, nil, "", "", float64(cum))
}

var startTime = time.Now()

func init() {
	NewGaugeFunc("go_info", "Information about the Go environment.",
		func(e Emit) { e(1, runtime.Version()) }, "version")
	runtimeGauge("go_goroutines", "Number of goroutines that currently exist.", u64(rmGoroutines))
	NewGaugeFunc("go_threads", "Number of OS threads created.", Value(func() float64 {
		n, _ := runtime.ThreadCreateProfile(nil)
		return float64(n)
	}))
	runtimeGauge("go_memstats_heap_alloc_bytes", "Bytes of allocated heap objects.", u64(rmHeapObj))
	runtimeGauge("go_memstats_heap_inuse_bytes", "Bytes in in-use heap spans.", u64(rmHeapObj, rmHeapUnused))
	runtimeGauge("go_memstats_sys_bytes", "Bytes of memory obtained from the OS by the Go runtime.", u64(rmTotal))
	runtimeGauge("go_memstats_next_gc_bytes", "Heap size target of the next GC cycle.", u64(rmHeapGoal))
	runtimeGauge("go_gc_gomemlimit_bytes", "The Go runtime memory limit (GOMEMLIMIT).", u64(rmMemLimit))
	runtimeGauge("go_gc_gogc_percent", "The GC target percentage (GOGC).", u64(rmGOGC))
	NewCounterFunc("go_gc_cycles_total", "Completed GC cycles.", func(e Emit) {
		if v, ok := u64(rmGCCycles)(readRuntime()); ok {
			e(v)
		}
	})
	register(&runtimeHist{
		// Not go_gc_duration_seconds: client_golang has that as a summary, and panels
		// asking it for {quantile="0.5"} would silently show nothing.
		name: "go_gc_pauses_seconds", help: "Stop-the-world pauses for GC (sum estimated).", typ: "histogram",
		sample: rmGCPauses,
		upper:  ExpBuckets(1e-5, 4, 8), // 10 µs … 164 ms
	})
	register(&runtimeHist{
		name: "go_sched_latencies_seconds", help: "Time goroutines spent runnable before running (sum estimated).", typ: "histogram",
		sample: rmSchedLat,
		upper:  ExpBuckets(1e-6, 4, 10), // 1 µs … 262 ms
	})

	NewGaugeFunc("process_start_time_seconds", "Start time of the process since the Unix epoch.",
		Value(func() float64 { return float64(startTime.UnixNano()) / 1e9 }))
	registerProcess()
}
