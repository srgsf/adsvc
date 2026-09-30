# Using adsvc from a client

adsvc is a pass-through proxy. The client decides, per play, whether a stream goes through it. **The client composes the URL**, because it is the only party that knows both addresses: how it reaches the source (TorrServer, a CDN, anything else over HTTP) and how it reaches adsvc.

Sources need no changes and never learn about adsvc. Clients that know nothing about adsvc keep playing the source directly.

```
client knows:  source  = https://ts.example.com/stream/Film.mkv?link=<ih>&index=1&play
               adsvc   = https://ads.example.com   (+ a user and token)

plays:         https://ads.example.com/s?u=<source, percent-encoded>

adsvc fetches: the source (after -route rewriting), forwarding Range etc., and analyses
               exactly the bytes it forwards; no extra reads.
```

## Stream URL

```
GET|HEAD {adsvc}/s?u=<source URL, percent-encoded>[&id=<file id> | &ih=<infohash>&idx=<n>]
```

- **One plain URL.** There is no redirect, so ExoPlayer/Media3, mpv, VLC and ffmpeg need no special settings. Range, `If-Range` and the other conditional headers are forwarded, and so are the response headers (minus hop-by-hop ones). Upstream status codes, including 401 and 404, reach the player unchanged. Cookies are forwarded except adsvc's own (`adsvc_*`, the web page's session). Upstream redirects are followed by adsvc, and each one must pass the routes and the allow-list (a refused one is a 403).
- **The file ID lets a known file's findings be used from the first byte.** adsvc keeps a per-file ad map, and it can only use that map if it knows which file is playing. It works this out in this order:
  1. `id`: any stable, opaque ID the client or the source has for the file. adsvc stores it hashed (`id:<hash>`), never as sent, since the catalogue may be shared.
  2. `ih` + `idx`: a torrent infohash and a file index.
  3. **TorrServer URLs are recognised by adsvc** without `id` or `ih`, so the client doesn't need to understand them: `/stream[/name]?link=<infohash|magnet>&index=N` and `/play/<infohash>/<N>`.
  4. For anything else, the source URL without user info or volatile parameters (`token`, `sig`, `expires`, `X-Amz-*`, …). A re-signed URL is therefore the same file.
  5. After both ends of the file have streamed through, a content key, `SHA-256(size ‖ first 64 KiB ‖ last 64 KiB)`, is added as an alias.

## Authentication

adsvc's own credentials and the source's credentials are kept apart.

| | How the client sends it | Where it goes |
|---|---|---|
| **adsvc** (`auth.users`) | `Authorization: Bearer <token>`, or, for players that can only be given a URL, the user and token as the user info of the adsvc URL: `https://<user>:<token>@ads.example.com/s?…` (players send it as Basic auth after adsvc's 401; the token must be that user's) | checked by adsvc and **never forwarded** |
| **source**, preferred | a signed or tokenised `u` (the source's own URL signing) | inside `u`; adsvc holds no secrets |
| **source** | user info inside `u` (`https://user:pass@ts.example.com/…`) | sent to the source as Basic auth |
| **source** | `X-Upstream-Authorization: <value>` | sent to the source as `Authorization` |

Without users that have tokens, adsvc forwards the player's `Authorization` header to the source. This is only for local development.

`GET /healthz` returns 200 with `adsvc <version>` as plain text and needs no auth. A client can call it before choosing the adsvc URL, and play the source directly if adsvc is down. **An adsvc failure must never stop playback.**

## Ads API

adsvc finds ads and intros and reports them; what a client does with a report is its own choice. A client that takes action asks with the same selector it used for `/s` (`u`, `id`, or `ih`/`idx`), plus adsvc's credentials:

| Route | |
|---|---|
| `GET /ads/file?…` | `{key, ads: [{adId, startMs, endMs, label, type, confirmed}], analyzed: [[fromMs, toMs]…], durationMs, …}`. `type` is `"ad"` or `"intro"`. A client that acts on detections automatically should act only on `confirmed` ones. `adId` is a string (UUID form: the hash of the reference's fingerprint). |
| `POST /ads/mark?…&startMs=S&endMs=E&label=L&type=ad\|intro` | enrol an ad (or an intro; `ad` is the default, anything else is a 400) from what was already streamed; clears the caller's reports of the same file within the ad ±15 s |
| `POST /ads/report?…&position=MS[&note=TEXT]` | **report an ad** the viewer saw at `position`, to be marked later; needs `u` (the play link for marking is made of it). 201 `{id, position}`. A second report within 10 s of one is the same report |
| `GET /ads/reports` | the caller's reports: `[{id, user, fileKey, url, sel, posMs, note, created}]` |
| `DELETE /ads/reports/{id}` | delete one of them (204, or 404) |

All times, in parameters and in JSON, are **integer milliseconds** on the player's media timeline.

### Ads and intros

Every reference, and so every detection, has a type. An `ad` is a commercial or promo break; an `intro` is a series' opening sequence, which repeats in each episode. There is no type for credits. A player may treat the types differently (skip ads but offer "skip intro" as a button, say); `adskip.lua` skips both.

### Reporting instead of marking

Marking an ad to the frame is impractical with a TV remote. A TV client only needs a "Report ad" action that sends `POST /ads/report` with the current file's selector and position. The user later logs in to adsvc's web page (`/`, with the same token), downloads `adskip.lua` once, and runs each report's mpv command there: it plays the file through adsvc (a plain `/s` URL) from 20 s before the reported position, and marking the ad (`a`, `A` and a label) deletes the report. While a file has an open report, adsvc decodes all of it, including ranges analysed before, so the ad's audio is there to mark; the report's mpv command also adds `&mark=1` to the URL, which does the same for the whole session (add it by hand to mark ads in a file nobody reported). Reports hold the source URL, so they are private to the user (and admins) and never leave the node.

## Deployment

### Next to the source (the common case)

adsvc runs on the TorrServer host, behind the same reverse proxy. Clients reach TorrServer at a public URL. Tell adsvc to use the loopback address instead:

```sh
adsvc proxy -listen 127.0.0.1:8080 \
  -route https://ts.example.com=http://127.0.0.1:8090
```

**Routes also act as the allow-list.** Once any route is set, only routed sources are fetched, unless `-allow host,…` adds more. Without routes or `-allow`, adsvc fetches any http(s) URL it is given. In that case keep it on loopback or behind a token.

### Elsewhere

adsvc fetches `u` exactly as the client sent it, so it must be able to reach that URL. If it can't (for example, the source is on the client's home LAN and adsvc is in the cloud), the client should play the source directly.

## TorrServer recipe

- The client wraps the URL it already plays:
  - `…/stream/<name>?link=<ih>&index=<n>&play`, or
  - `…/play/<ih>/<n>`.

  Both are recognised, so passing `ih`/`idx` is optional.
- **When TorrServer has `HttpAuth` on:**
  - `/play/<ih>/<n>` and `/stream…&play` for a torrent that is already in TorrServer's DB are served without auth. The client adds the torrent through the normal API first, which it does anyway.
  - For anything else, pass `X-Upstream-Authorization: Basic …` or put `user:pass@` inside `u`.
- **Playlists** (`/playlist?hash=…`, `&m3u`) point straight at TorrServer. A client that plays from a playlist wraps each item URL itself.

## Examples

```sh
# mpv, token in a header (mpv/adskip.lua with ads_token= does this automatically)
mpv --http-header-fields='Authorization: Bearer TOKEN' \
  "http://127.0.0.1:8080/s?u=$(jq -rn --arg u 'https://ts.example.com/play/<ih>/1' '$u|@uri')"

# a player that can only take a URL: user and token as the URL's user info
mpv "http://alice:TOKEN@127.0.0.1:8080/s?u=..."
```

```kotlin
// Media3: adsvc key for adsvc, TorrServer credentials passed through
val ds = DefaultHttpDataSource.Factory().setDefaultRequestProperties(mapOf(
    "Authorization" to "Bearer $adsvcKey",
    "X-Upstream-Authorization" to tsBasicAuth,   // only if the source needs it
))
val uri = "$adsvcBase/s?u=" + Uri.encode(sourceUrl)
```
