package proxy

import "github.com/srgsf/adsvc/internal/metrics"

// Metrics of the proxy (the HTTP ones are in accesslog.go). Labels are fixed sets.
var (
	// rate(adsvc_upstream_requests_total{result=~"error|5xx"}[5m]) / rate(adsvc_upstream_requests_total[5m])
	upstreamRequests = metrics.NewCounterVec("adsvc_upstream_requests_total",
		"Requests to the source, by result: 2xx, 3xx, 4xx, 5xx, or error (no response).", "result")
	// histogram_quantile(0.9, rate(adsvc_upstream_response_seconds_bucket[5m]))
	upstreamResponse = metrics.NewHistogram("adsvc_upstream_response_seconds",
		"Time from the upstream request to its response headers.", metrics.ExpBuckets(0.005, 3, 9)) // 5 ms … 33 s
	streamBytes = metrics.NewCounter("adsvc_stream_bytes_total",
		"Bytes forwarded from sources to players.")
	streamCopyErrors = metrics.NewCounter("adsvc_stream_copy_errors_total",
		"Streams that ended with an error: mostly players hanging up, also broken sources.")

	sessionsActive = metrics.NewGauge("adsvc_sessions_active",
		"Files being streamed and analysed.")
	sessionsOpened = metrics.NewCounterVec("adsvc_sessions_opened_total",
		"Sessions opened, by kind: normal, reported (a file with an open report), detached (during shutdown: forward only).", "kind")
	sessionsClosed = metrics.NewCounterVec("adsvc_sessions_closed_total",
		"Sessions closed, by reason: idle or shutdown.", "reason")
	containers = metrics.NewCounterVec("adsvc_containers_total",
		"Containers recognised at the start of a session, by type: avi, matroska, mp4, mpegts, mp3.", "type")
	// increase(adsvc_analysis_disabled_total{reason="panic"}[1h]) > 0
	analysisDisabled = metrics.NewCounterVec("adsvc_analysis_disabled_total",
		"Sessions whose analysis was turned off, by reason: unknown_container, no_head, open_failed, demux_error, panic.", "reason")

	queueBytes = metrics.NewGauge("adsvc_analyser_queue_bytes",
		"Bytes queued between players and analysers, over every session.")
	stallSeconds = metrics.NewCounter("adsvc_analyser_stall_seconds_total",
		"Time players waited for an analyser with a full queue (at most max_stall each time).")
	// rate(adsvc_analyser_skip_ahead_total[10m]) > 0: the analyser cannot keep up
	skipAhead = metrics.NewCounter("adsvc_analyser_skip_ahead_total",
		"Times an analyser fell behind by more than max_stall and skipped ahead (a gap in coverage).")
	droppedBytes = metrics.NewCounter("adsvc_analyser_dropped_bytes_total",
		"Bytes forwarded but not analysed because the analyser was behind.")

	detections = metrics.NewCounter("adsvc_detections_total",
		"Ads found in sessions: new detections, or a better alignment of one.")
	// histogram_quantile(0.1, rate(adsvc_detection_score_bucket[1d])) against min_score
	detectionScore = metrics.NewHistogram("adsvc_detection_score",
		"Match score of the detections.", metrics.ExpBuckets(10, 2, 8)) // 10 … 1280
	fileMaps = metrics.NewCounterVec("adsvc_file_map_requests_total",
		"/ads/file answers, by result: hit (a session or a stored map) or miss (unknown file).", "result")
	marks = metrics.NewCounterVec("adsvc_marks_total",
		"/ads/mark calls that reached a session, by result: ok or error.", "result")
	reports = metrics.NewCounter("adsvc_reports_total",
		"Ads reported by users (/ads/report).")
)

// statusClass is the upstreamRequests label of a status code.
func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	}
	return "2xx"
}
