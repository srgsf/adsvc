package library

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/srgsf/adsvc/internal/metrics"
)

var (
	// histogram_quantile(0.99, rate(adsvc_match_duration_seconds_bucket[5m])) > 0.05
	matchSeconds = metrics.NewHistogram("adsvc_match_duration_seconds",
		"Time to match one block's landmarks against the index.",
		metrics.ExpBuckets(0.0005, 2, 10)) // 0.5 ms … 256 ms
	builds = metrics.NewCounterVec("adsvc_library_builds_total",
		"Builds of the ad index, by result: ok or error.", "result")
	buildSeconds = metrics.NewHistogram("adsvc_library_build_duration_seconds",
		"Time to build the ad index.", metrics.ExpBuckets(0.1, 3, 7)) // 0.1 s … 73 s

	exported atomic.Pointer[Library]
)

// statsMemo serves one Stats to every adsvc_library_* gauge of a scrape.
var statsMemo struct {
	sync.Mutex
	l  *Library
	at time.Time
	s  Stats
}

func scrapeStats() (Stats, bool) {
	l := exported.Load()
	if l == nil {
		return Stats{}, false
	}
	statsMemo.Lock()
	defer statsMemo.Unlock()
	if statsMemo.l != l || time.Since(statsMemo.at) >= time.Second {
		statsMemo.l, statsMemo.at, statsMemo.s = l, time.Now(), l.Stats()
	}
	return statsMemo.s, true
}

// ExportMetrics makes l the library that the adsvc_library_* gauges describe (the
// proxy's; copies made for a benchmark stay out of them).
func (l *Library) ExportMetrics() { exported.Store(l) }

func init() {
	stat := func(name, help string, fn func(Stats) float64) {
		metrics.NewGaugeFunc(name, help, func(e metrics.Emit) {
			if s, ok := scrapeStats(); ok {
				e(fn(s))
			}
		})
	}
	stat("adsvc_library_ads", "Tracked ads: in the index or the overlay.", func(s Stats) float64 { return float64(s.Ads) })
	stat("adsvc_library_index_ads", "Ads in the built index, removed ones included.", func(s Stats) float64 { return float64(s.IndexAds) })
	stat("adsvc_library_postings", "Postings of the built index.", func(s Stats) float64 { return float64(s.Postings) })
	stat("adsvc_library_dead_ads", "Ads of the built index removed since the build.", func(s Stats) float64 { return float64(s.DeadAds) })
	stat("adsvc_library_dead_postings", "Postings of removed ads still in the built index.", func(s Stats) float64 { return float64(s.DeadPostings) })
	stat("adsvc_library_overlay_ads", "Ads added since the build (the overlay).", func(s Stats) float64 { return float64(s.OverlayAds) })
	stat("adsvc_library_overlay_postings", "Postings of the overlay.", func(s Stats) float64 { return float64(s.OverlayPostings) })
	stat("adsvc_library_mapped_bytes", "Size of the memory-mapped index file (page cache, not heap).", func(s Stats) float64 { return float64(s.MappedBytes) })
	stat("adsvc_library_heap_bytes", "Estimated heap used by the index and the overlay.", func(s Stats) float64 { return float64(s.HeapBytes) })
	stat("adsvc_library_last_build_timestamp_seconds", "When the index in use was built, since the Unix epoch.", func(s Stats) float64 {
		return float64(s.Built.UnixMilli()) / 1e3
	})
	metrics.NewGaugeFunc("adsvc_library_version", "Changes whenever the tracked ads or the index change.", func(e metrics.Emit) {
		if l := exported.Load(); l != nil {
			e(float64(l.Version()))
		}
	})
}
