<img src="docs/logo/header.svg" alt="adsvc: finds known ads in video streams" width="560">

adsvc finds known ads (and intros) inside a video while it streams, and reports where they
are. What to do with a report is up to the player.

It is a pass-through HTTP proxy. The player opens the stream through adsvc, and adsvc
analyses **exactly the bytes the player downloads**, with no second connection and no
extra reads. P2P sources (TorrServer), caches and connection-limited debrid services see the
same access pattern as without it.

```
player ──GET /s?u=<upstream>──► adsvc ──GET <upstream>──► TorrServer / any HTTP source
   ▲         the same bytes, forwarded unchanged │
   │                                              ▼
 /ads/file (ad map)       demuxer → ffmpeg → fingerprint → match
```

- **Fingerprints.** It does not depend on video resolution or codec, and it survives
  re-encoding, downmixing, volume changes and noise.
- **Own demuxers** for Matroska/WebM, MP4/MOV (including fragmented), MPEG-TS/M2TS and AVI.
  Seeks work on the bytes already flowing to the player.
- **Mark once, find everywhere.** An ad marked in one file is found in every file, from any
  release. The findings for a file watched once are known from the first frame.
- **Ads and intros.** Every reference has a type, `ad` or `intro` (a series' opening
  sequence, which repeats in each episode). There is no type for credits. Each detection tells the player which one it is.
- **Federated.** Nodes share ads as signed records and each node chooses whom to trust.
- **Small.** Pure Go, no CGO, a minimal ffmpeg (about 2.5 MB) as an external binary.

## Build

You only need `make` and a container runtime (Docker or Apple `container`):

```sh
make ffmpeg              # minimal ffmpeg for the host: dist/ffmpeg
make                     # dist/adsvc (uses the ffmpeg next to it)
```

Not required, but available:

```sh
make docker                  # scratch image adsvc/adsvc
make test
```

## Usage

```sh
adsvc proxy                                                # users and tokens: config.yml
adsvc proxy -route https://ts.example.com=http://127.0.0.1:8090   # next to TorrServer
adsvc -media ~/Videos                                      # serve a local folder
adsvc proxy -metrics-listen :9464                          # Prometheus metrics at /metrics
```

Play through it with the mpv client, which acts on confirmed ads and intros and marks new ones
(`a` at the start of an ad, `t` at the start of an intro, `A` at the end of either):

```sh
cp mpv/adskip.lua ~/.config/mpv/scripts/
mpv "http://localhost:8080/s?u=$(jq -rn --arg u 'http://127.0.0.1:8090/play/<infohash>/1' '$u|@uri')"
```

Players and apps poll `GET /ads/file` with the same selector; every detection has a `type`
and a `confirmed` flag, and a player takes action on the confirmed ones. The web page at `/` manages ads, reports and peers.

- Client integration: [docs/handoff.md](docs/handoff.md)
- User guide: [docs/usage.md](docs/usage.md), with every config key in [config.yml.example](config.yml.example)
- Design: [docs/database.md](docs/database.md)
- Offline check of a file: `adsvc scan -in episode.mkv`

## Results

`adsvc bench` on 9 real episodes (6.8 h, 10 ads, 27 occurrences; see
[bench/README.md](bench/README.md)). The labels were drafted by the detector and most are
not verified yet, so read clean-audio recall as a baseline, not a proof.

| | recall | FP | weakest true / strongest noise | look-ahead to confirm (median / max) |
|---|---|---|---|---|
| clean, offline | 27/27 | 0 | 106 / 20 | 17.5 s / 38.8 s |
| clean, proxy (MKV and AVI) | 51/51 | 0 | 103 / – | – |
| AAC 48k mono | 27/27 | 0 | 60 / 18 | 17.5 s / 38.8 s |
| MP3 32k mono | 27/27 | 0 | 50 / 11 | 17.5 s / 38.8 s |
| PAL speed-up, pitch corrected | 27/27 | 0 | 39 / 14 | 32.2 s / 53.5 s |
| PAL speed-up, pitch up | **0/27** | 0 | – / 8 | – |

Across releases: ads marked in a 1080p MKV were found in a 400p AVI of the same episode,
within 60 ms of where they were marked.

## Contributing and license

See [CONTRIBUTING.md](CONTRIBUTING.md) and [SECURITY.md](SECURITY.md). adsvc is under the
[MIT License](LICENSE). The minimal ffmpeg shipped next to it is LGPL-licensed (its license
text travels with it as `ffmpeg-LICENSE`).
