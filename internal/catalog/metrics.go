package catalog

import (
	"database/sql"
	"os"
	"sync/atomic"

	"github.com/srgsf/adsvc/internal/metrics"
)

var (
	writeSeconds = metrics.NewHistogram("adsvc_catalog_write_duration_seconds",
		"Time of a write transaction on the catalogue, waiting for the writer included.",
		metrics.ExpBuckets(0.0005, 4, 8)) // 0.5 ms … 8 s
	ingested = metrics.NewCounterVec("adsvc_records_ingested_total",
		"Records received from peers, pushes and imports, by result (accepted, duplicate, rejected) and rejection reason.",
		"result", "reason")

	exported atomic.Pointer[DB]
)

// ExportMetrics makes db the catalogue that the adsvc_catalog_db_* metrics describe.
func (db *DB) ExportMetrics() { exported.Store(db) }

// pools are the connection pools of the exported catalogue: the writer, and the readers
// when they are separate.
func pools(e func(name string, st sql.DBStats)) {
	db := exported.Load()
	if db == nil {
		return
	}
	e("writer", db.w.Stats())
	if db.r != db.w {
		e("reader", db.r.Stats())
	}
}

func init() {
	metrics.NewCounterFunc("adsvc_catalog_db_wait_seconds_total",
		"Time spent waiting for a free connection, by pool (writer, reader).", func(e metrics.Emit) {
			pools(func(name string, st sql.DBStats) { e(st.WaitDuration.Seconds(), name) })
		}, "pool")
	metrics.NewGaugeFunc("adsvc_catalog_db_in_use",
		"Connections in use, by pool (writer, reader).", func(e metrics.Emit) {
			pools(func(name string, st sql.DBStats) { e(float64(st.InUse), name) })
		}, "pool")
	metrics.NewGaugeFunc("adsvc_catalog_db_bytes",
		"Size of the catalogue file and its write-ahead log.", func(e metrics.Emit) {
			db := exported.Load()
			if db == nil || db.path == "" {
				return
			}
			var n int64
			for _, p := range []string{db.path, db.path + "-wal"} {
				if fi, err := os.Stat(p); err == nil {
					n += fi.Size()
				}
			}
			e(float64(n))
		})
}

// countIngest adds the result of one Ingest to the metrics.
func countIngest(res IngestResult) {
	ingested.With("accepted", "").Add(float64(res.Accepted))
	ingested.With("duplicate", "").Add(float64(res.Duplicate))
	for reason, n := range res.Rejected {
		ingested.With("rejected", reason).Add(float64(n))
	}
}
