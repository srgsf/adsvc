package library

import (
	"context"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"

	"github.com/srgsf/adsvc/internal/fingerprint"
)

var benchSizes = []int{1000, 10000}

var benchAds = sync.OnceValue(func() map[int][]testAd {
	m := map[int][]testAd{}
	for _, n := range benchSizes {
		ads := synthAds(n, 42)
		slices.SortFunc(ads, func(a, b testAd) int { return compareID(a.ID, b.ID) })
		m[n] = ads
	}
	return m
})

func heapInUse() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

func heapSource(ads []testAd) ([]fingerprint.ID, adSource) {
	ids := make([]fingerprint.ID, len(ads))
	for i, a := range ads {
		ids[i] = a.ID
	}
	return ids, func(fn func(d int, pts []fingerprint.Point) error) error {
		for d, a := range ads {
			if err := fn(d, a.Points); err != nil {
				return err
			}
		}
		return nil
	}
}

// BenchmarkBuild is the in-memory build from landmarks already decoded (as in phase 1).
func BenchmarkBuild(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("ads=%d", n), func(b *testing.B) {
			ids, src := heapSource(benchAds()[n])
			for b.Loop() {
				if _, _, err := buildCSR(ids, src, nil); err != nil {
					b.Fatal(err)
				}
			}
			// Heap held by the index alone: ad landmarks are allocated before. Reported after
			// the loop, which resets extra metrics when it starts.
			before := heapInUse()
			c, _, _ := buildCSR(ids, src, nil)
			b.ReportMetric(float64(heapInUse()-before)/(1<<20), "index-MB")
			runtime.KeepAlive(c)
		})
	}
}

// BenchmarkOpen builds the index file from the catalogue (decoding every ad twice) and
// maps it; index-MB is what the opened library keeps on the heap.
func BenchmarkOpen(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("ads=%d", n), func(b *testing.B) {
			dir := b.TempDir()
			db := openCatalog(b, filepath.Join(dir, "catalogue.db"))
			store(b, db, benchAds()[n])
			path := filepath.Join(dir, "tracking.csr")
			for b.Loop() {
				l := &Library{db: db, csrPath: path}
				c, err := l.build(context.Background())
				if err != nil {
					b.Fatal(err)
				}
				c.release()
			}
			before := heapInUse()
			l, err := Open(context.Background(), db, path)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(int64(heapInUse())-int64(before))/(1<<20), "index-MB")
			l.Close()
		})
	}
}

func BenchmarkMatch(b *testing.B) {
	for _, n := range benchSizes {
		ads := benchAds()[n]
		ids, src := heapSource(ads)
		c, _, err := buildCSR(ids, src, nil)
		if err != nil {
			b.Fatal(err)
		}
		l := &Library{csr: c}
		r := rand.New(rand.NewPCG(3, 4))
		// noise: a block of programme audio (the usual case); hit: a block inside an ad.
		qs := map[string][][]fingerprint.Point{}
		for range 32 {
			qs["noise"] = append(qs["noise"], query(r, nil, 0))
			a := &ads[r.IntN(len(ads))]
			qs["hit"] = append(qs["hit"], query(r, a, int32(r.IntN(len(a.Points)/6/2))))
		}
		for _, kind := range []string{"noise", "hit"} {
			b.Run(fmt.Sprintf("ads=%d/%s", n, kind), func(b *testing.B) {
				i := 0
				for b.Loop() {
					l.Match(qs[kind][i%len(qs[kind])], 20)
					i++
				}
			})
		}
	}
}
