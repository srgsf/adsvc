# User guide

How a player or app uses adsvc is in [Client integration](handoff.md); how the catalogue,
and sync work is in the [design notes](database.md).

## Playing through adsvc

`u` is the upstream URL (percent-encoded). `ih`/`idx` (or an opaque `id`) are optional and
only used as the file key. TorrServer URLs (`/stream?link=…&index=…`, `/play/<ih>/<n>`) are
recognised on their own. Otherwise the file is keyed by its URL without tokens, and then
by a hash of its own bytes once both ends of the file have passed through.

adsvc analyses the stream and reports what it finds; the player decides what to do with it.
The mpv client, `mpv/adskip.lua`, acts on them: press `a` at the start of an ad, or `t` at the start
of an intro, and `A` at its end to enroll it (`,` and `.` step frames). The fingerprint is taken
from what the proxy already analysed, so nothing is read again. From then on the confirmed
findings are acted on when playback reaches them (`Ctrl+a` toggles this).

Every reference is an `ad` or an `intro`. An intro is a title sequence that repeats in each
episode of a series; there is no type for credits. A type is
chosen when the reference is enrolled (`type=` of `/ads/mark`), and can be changed on the
Ad page.

The data directory holds `catalogue.db` (SQLite: the ads and the per-file ad maps),
`tracking.csr` (the ad index, rebuilt when the ads change) and `identity.key`. Ads are
identified by the hash of their fingerprint (a UUID-shaped id, the same on every machine).
The catalogue holds no secrets, URLs or file names; labels are whatever you type (an ad
marked without one is `ad-<short id>`, or `intro-<short id>`).

## Local files

```sh
adsvc -media ~/Videos          # proxy is the default command
```

The folder is served read-only on `localhost:8000` (`-media-listen`, loopback only) and
added to the upstream allow-list. Open <http://localhost:8000/> in a browser: every file
has a *play* URL to give the player, such as
`http://localhost:8080/s?u=http://localhost:8000/episode1.mkv`. With users configured, put
`NAME:TOKEN@` after `http://`. Hidden files and symlinks that lead out of the folder are
not served. Keep the folder and address the same between runs: the play URL is the file's
key until its content hash is known.

**Bind to localhost** (the default), or protect adsvc with users and restrict sources with
`-route` / `-allow`. Otherwise it will fetch any URL anyone asks it for.

## Configuration file

`adsvc proxy` reads `config.yml` in the current directory when it exists, or the file named
by `-config` or `$ADSVC_CONFIG`; see [config.yml.example](../config.yml.example) for every
key. Flags and the environment win over the file. Users come from the file only. Unknown
keys are errors.

```yaml
auth:
  users:
    alice:
      tokens: [9vW3…]           # any number; each belongs to one user
      admin: true               # its tokens also log in to the web page's admin pages
    tv-box:
      tokens: [Qm7c…]
```

A request carries `Authorization: Bearer TOKEN`, or the user and token in the URL:
`http://alice:TOKEN@host:8080/s?u=…`. Without a user that has a token, adsvc is open to
anyone who can reach it. `adsvc token` prints a new random token; add it to a user's
`tokens` and reload.

`kill -HUP <pid>` reloads the file. Auth, routes, the allow-list, `max_stall`,
`idle_timeout`, `confirm_score` and logging apply at once; `min_score`, `mark_window` and
`ffmpeg` apply to new sessions; `listen`, `data_dir`, `media` and `metrics` need a restart. An invalid file
changes nothing and is reported in the log. Removing a token revokes it and ends web
sessions opened with it.

## HTTP API

Routes accept `id`, `ih`+`idx` (aliases `hash`, `index`), `u` or `key`. With users
configured, all but `/healthz` need a token. Times are **integer milliseconds** on the
player's media timeline.

| Route | Purpose |
|---|---|
| `GET /s?u=…` | the media itself, forwarded byte for byte |
| `GET /ads/file` | `{key, aliases, container, size, durationMs, analysedToMs, analyzed: [[fromMs,toMs]…], ads: [{adId,label,type,startMs,endMs,score,confirmed}]}` |
| `POST /ads/mark?startMs=S&endMs=E&label=L&type=ad\|intro` | enroll `[S,E)` of this file as an ad (the default) or an intro, from what was already analysed |
| `POST /ads/report?u=…&position=MS` | report an ad seen at MS, to mark it later |
| `GET /ads/library` | the reference ads |
| `GET /ads/maps` | stored per-file ad maps |
| `GET /healthz` | 200 `adsvc <version>`, no auth |

`type` is `ad` or `intro`. **A player that acts on detections automatically
should use only `confirmed: true` ones.** A detection is confirmed once its whole interval
has been analysed, or when two consecutive blocks agree on the same alignment with a score
of at least `confirm_score` (default 2× `min_score`).

## Federation

Nodes share ads, labels, votes and file maps as signed records. Each node has an ed25519
identity; a node decides for itself what to trust (`trust.*`: weights per node id, blocks,
quotas). Peers pull each other's logs (`sync.peers`); a node behind NAT pushes to a
reachable one. `/sync/*` needs a user's token unless `sync.public: true`.

```sh
adsvc identity                  # give this id to your peers
adsvc export -o snapshot.db     # every record: a file anyone may have
adsvc import snapshot.db        # bootstrap from someone's snapshot
```

Snapshots hold no secrets, URLs or file names, and never include the node's own file maps.

## Logging

Logs go to stderr (`log/slog`), command results to stdout. `-log-level debug|info|warn|error`
(or `$ADSVC_LOG_LEVEL`, `log.level`), `-log-format text|json` (or `$ADSVC_LOG_FORMAT`,
`log.format`); `proxy -v` is `-log-level debug`. Upstream URLs are logged with credentials
and signed-URL parameters redacted.

## Metrics

`-metrics-listen localhost:9464` (or `metrics.listen`) serves Prometheus metrics at
`/metrics` on a port of its own, without auth. It is off by default. An address without a
host means localhost, so a scraper on another machine or container needs an explicit host
(`0.0.0.0:9464`). The metrics hold no URLs, file keys, tokens or user names; sync metrics
are labelled with the peer names from the config.

```yaml
scrape_configs:
  - job_name: adsvc
    static_configs: [{targets: ["localhost:9464"]}]
```

| Area | Metrics |
|---|---|
| Build, process | `adsvc_build_info`, `adsvc_config_reloads_total{result}`, `process_cpu_seconds_total`, `process_start_time_seconds`; on Linux and Android also `process_resident_memory_bytes` (includes the mapped ad index; the ffmpeg processes are separate), `process_virtual_memory_bytes`, `process_open_fds` |
| Go runtime | `go_goroutines`, `go_threads`, `go_memstats_heap_alloc_bytes`, `go_memstats_heap_inuse_bytes`, `go_memstats_sys_bytes`, `go_memstats_next_gc_bytes`, `go_gc_gomemlimit_bytes`, `go_gc_gogc_percent`, `go_gc_cycles_total`, `go_gc_pauses_seconds` (a histogram; client_golang's `go_gc_duration_seconds` is a summary, so it is not reused), `go_sched_latencies_seconds` |
| HTTP | `adsvc_http_requests_total{handler,method,code}`, `adsvc_http_request_duration_seconds{handler}` (not for streams), `adsvc_http_requests_in_flight{handler}` (for `stream`: open player connections). `handler` is `stream`, `file`, `mark`, `report`, `reports`, `library`, `maps`, `healthz`, `admin`, `sync` or `other` |
| Sources | `adsvc_upstream_requests_total{result=2xx\|3xx\|4xx\|5xx\|error}`, `adsvc_upstream_response_seconds` (time to headers), `adsvc_stream_bytes_total`, `adsvc_stream_copy_errors_total` (mostly players hanging up) |
| Sessions | `adsvc_sessions_active`, `adsvc_sessions_opened_total{kind}`, `adsvc_sessions_closed_total{reason}`, `adsvc_containers_total{type}`, `adsvc_analysis_disabled_total{reason=unknown_container\|no_head\|open_failed\|demux_error\|panic}` |
| Keeping up | `adsvc_analyser_queue_bytes`, `adsvc_analyser_stall_seconds_total` (players waiting, up to `max_stall`), `adsvc_analyser_skip_ahead_total` and `adsvc_analyser_dropped_bytes_total` (gaps in coverage), `adsvc_analysed_media_seconds_total` |
| Tracking | `adsvc_fingerprint_duration_seconds` and `adsvc_match_duration_seconds` (per 10 s block), `adsvc_detections_total`, `adsvc_detection_score`, `adsvc_confirmations_total`, `adsvc_file_map_requests_total{result=hit\|miss}`, `adsvc_marks_total{result}`, `adsvc_reports_total` |
| Decoder | `adsvc_ffmpeg_running`, `adsvc_ffmpeg_runs_total{result=ok\|failed\|start_failed\|read_error\|shutdown}`, `adsvc_ffmpeg_run_seconds`, `adsvc_decoder_restarts_total`, `adsvc_decoder_idle_frames_total` (skipped: analysed before), `adsvc_pcm_ring_bytes` |
| Ad index | `adsvc_library_ads`, `_index_ads`, `_postings`, `_overlay_ads`, `_overlay_postings`, `_dead_ads`, `_dead_postings`, `_mapped_bytes`, `_heap_bytes`, `_version`, `_last_build_timestamp_seconds`, `adsvc_library_builds_total{result}`, `adsvc_library_build_duration_seconds` |
| Catalogue | `adsvc_catalog_write_duration_seconds`, `adsvc_catalog_db_wait_seconds_total{pool}`, `adsvc_catalog_db_in_use{pool}`, `adsvc_catalog_db_bytes`, `adsvc_records_ingested_total{result,reason}` |
| Sync | `adsvc_sync_peers`, `adsvc_sync_rounds_total{peer,result}`, `adsvc_sync_round_duration_seconds{peer}`, `adsvc_sync_last_success_timestamp_seconds{peer}`, `adsvc_sync_pull_lag_records{peer}`, `adsvc_sync_records_total{peer,direction}` |

Useful queries and alerts:

```promql
# sources failing (over 5% of requests)
sum(rate(adsvc_upstream_requests_total{result=~"error|5xx"}[5m])) / sum(rate(adsvc_upstream_requests_total[5m])) > 0.05
# the analyser cannot keep up with playback: coverage gaps for 10 minutes
rate(adsvc_analyser_skip_ahead_total[10m]) > 0
# media seconds analysed per second of wall time, per open stream (>= 1 keeps up)
rate(adsvc_analysed_media_seconds_total[5m]) / clamp_min(adsvc_http_requests_in_flight{handler="stream"}, 1)
# ffmpeg runs failing: often a codec missing from the ffmpeg build
rate(adsvc_ffmpeg_runs_total{result=~"failed|start_failed"}[10m]) > 0
# a demuxer crashed on some file
increase(adsvc_analysis_disabled_total{reason="panic"}[1h]) > 0
# matching slows down (index too big for the box)
histogram_quantile(0.99, rate(adsvc_match_duration_seconds_bucket[5m])) > 0.05
# a peer has not synced for three intervals (10 min default)
time() - adsvc_sync_last_success_timestamp_seconds > 1800
# close to the memory limit
go_memstats_heap_inuse_bytes / go_gc_gomemlimit_bytes > 0.9
```
