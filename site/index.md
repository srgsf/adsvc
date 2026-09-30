<section class="hero">
<div class="title">
<img class="logo" src="logo-256.png" alt="adsvc logo">
<div><span class="name">adsvc</span><span class="tagline">Finds known ads in video streams</span></div>
</div>

adsvc recognises the ads and intros it already knows while a video streams, and reports
where they are, so that your player can take action.

<div class="cta">
<a class="primary" href="usage.html">User guide</a>
<a href="handoff.html">Client integration</a>
</div>
</section>

## How it works

adsvc sits between the player and the source as a pass-through HTTP proxy. The player opens
the stream through it, and adsvc analyses **exactly the bytes the player downloads**: no
second connection and no extra reads. P2P sources such as TorrServer, caches and
connection-limited debrid services see the same access pattern as without it.

```
player ──GET /s?u=<upstream>──► adsvc ──GET <upstream>──► TorrServer / any HTTP source
   ▲         the same bytes, forwarded unchanged │
   │                                              ▼
 /ads/file (findings)     demuxer → ffmpeg → fingerprint → match
```

adsvc only finds and reports. What a player does with a report is its own choice; the
included mpv client acts on the confirmed ones.

## What you get

- **Fingerprints.** They do not depend on video resolution or codec, and they survive
  re-encoding, downmixing, volume changes and noise.
- **Ads and intros.** Every reference has a type: `ad` (a commercial or promo break) or
  `intro` (a series' opening sequence, which repeats in each episode). Each finding tells the
  player which one it is, so it can treat them differently.
- **Mark once, find everywhere.** An ad marked in one file is found in every file, from any
  release. The findings for a file watched once are known from the first frame.
- **Own demuxers** for Matroska/WebM, MP4/MOV (including fragmented), MPEG-TS/M2TS and AVI.
  Seeks work on the bytes already flowing to the player.
- **Federated.** Nodes share ads as signed records, and each node chooses whom to trust.
- **Small.** Pure Go, no CGO, and a minimal ffmpeg (about 2.5 MB) as an external binary.

## Try it

Run the proxy, then play a stream through it with the mpv client, which acts on confirmed ads
and intros and lets you mark new ones (`a` at the start of an ad, `t` at the start of an
intro, `A` at the end of either):

```sh
adsvc proxy
cp mpv/adskip.lua ~/.config/mpv/scripts/
mpv "http://localhost:8080/s?u=$(jq -rn --arg u 'http://127.0.0.1:8090/play/<infohash>/1' '$u|@uri')"
```

A client of your own asks `GET /ads/file` with the same selector as the stream and gets the
findings for that file:

```json
{"ads": [{"adId": "…", "label": "Brand X", "type": "ad",
          "startMs": 276100, "endMs": 306100, "score": 212, "confirmed": true}]}
```

## Read on

- [User guide](usage.md): running adsvc, users and tokens, configuration, the HTTP API,
  federation, logging and metrics.
- [Client integration](handoff.md): how a player or an app composes the stream URL, the
  findings API, reporting an ad from a remote-controlled TV, and deployment next to
  TorrServer.
- [Source, releases and the design notes](https://github.com/srgsf/adsvc).
