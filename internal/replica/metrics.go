package replica

import "github.com/srgsf/adsvc/internal/metrics"

// Sync metrics, by peer name (from the config, so a bounded set). A removed peer's series
// stay until the restart.
var (
	syncPeers = metrics.NewGauge("adsvc_sync_peers",
		"Peers this node syncs with.")
	syncRounds = metrics.NewCounterVec("adsvc_sync_rounds_total",
		"Sync rounds, by peer and result: ok or error.", "peer", "result")
	syncRoundSeconds = metrics.NewHistogramVec("adsvc_sync_round_duration_seconds",
		"Time of a sync round, by peer.", metrics.ExpBuckets(0.05, 4, 7), "peer") // 50 ms … 205 s
	// time() - adsvc_sync_last_success_timestamp_seconds > 3 * the peer's interval
	syncLastSuccess = metrics.NewGaugeVec("adsvc_sync_last_success_timestamp_seconds",
		"When the last sync round with a peer succeeded, since the Unix epoch.", "peer")
	syncPullLag = metrics.NewGaugeVec("adsvc_sync_pull_lag_records",
		"Records of a peer's log not pulled yet, as of the last round.", "peer")
	syncRecords = metrics.NewCounterVec("adsvc_sync_records_total",
		"Records transferred, by peer and direction: pull or push.", "peer", "direction")
)
