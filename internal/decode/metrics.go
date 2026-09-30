package decode

import "github.com/srgsf/adsvc/internal/metrics"

var (
	// Each run is an ffmpeg process: its memory is not in this process's.
	ffmpegRunning = metrics.NewGauge("adsvc_ffmpeg_running",
		"ffmpeg decoder processes running.")
	// rate(adsvc_ffmpeg_runs_total{result=~"failed|start_failed"}[10m]) > 0: often a codec
	// missing from the ffmpeg build
	ffmpegRuns = metrics.NewCounterVec("adsvc_ffmpeg_runs_total",
		"ffmpeg decoder runs, by result: ok, failed, start_failed, read_error, shutdown.", "result")
	ffmpegRunSeconds = metrics.NewHistogram("adsvc_ffmpeg_run_seconds",
		"Wall time of an ffmpeg decoder run.", metrics.ExpBuckets(1, 4, 7)) // 1 s … 68 min
	decoderRestarts = metrics.NewCounter("adsvc_decoder_restarts_total",
		"Decoder runs restarted because the media timeline jumped (a seek, a gap).")
	ringBytes = metrics.NewGauge("adsvc_pcm_ring_bytes",
		"Memory of the decoded-audio rings kept for /ads/mark, over every session.")
	idleFrames = metrics.NewCounter("adsvc_decoder_idle_frames_total",
		"Audio frames not decoded because their media time was analysed before.")
)
