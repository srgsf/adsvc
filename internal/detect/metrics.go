package detect

import "github.com/srgsf/adsvc/internal/metrics"

var (
	// rate(adsvc_analysed_media_seconds_total[5m]): media seconds analysed per second
	analysedSeconds = metrics.NewCounter("adsvc_analysed_media_seconds_total",
		"Media time fingerprinted and matched.")
	fingerprintSeconds = metrics.NewHistogram("adsvc_fingerprint_duration_seconds",
		"Time to fingerprint one analysis block (10 s of audio plus the overlap).",
		metrics.ExpBuckets(0.001, 2, 10)) // 1 ms … 512 ms
	confirmations = metrics.NewCounter("adsvc_confirmations_total",
		"Detections that became confirmed (safe to skip).")
)
