# Catalogue, index, config and sync

How adsvc stores ads and file maps, indexes them for matching, shares them between nodes,
and how its config and web page work.

The catalogue file can be handed to anyone, so it holds **no secrets, file names or URLs**
(the one exception is the local-only `reports` table, §3). Files are known only by
`ih:<infohash>/<idx>`, `c:<content key>` or `id:<hash>` keys (`internal/filekey`), and ads
by a content id plus a label.

## 1. Overview

```
            records (signed, replicated)
                    │ materialize
                    ▼
   ┌──────────── catalogue.db (SQLite, shareable) ─────────────┐
   │ records · ads · labels · votes · dups · file maps          │
   │ local-only: origin_policy · peers · pushers · pins ·       │
   │             reports · meta                                 │
   └────────────────────────────────────────────────────────────┘
                    │ select tracking set (trust, pins, ≤ max_ads)
                    ▼
        tracking.csr (immutable, mmapped)  +  overlay (in-memory map
                    │                          index, ads added since
                    ▼                          the last build)
        library.Library: Match / Get  ──►  detect.Scan / Pick
```

- **SQLite is the source of truth.** Shared state is a log of signed, immutable records;
  the other tables are materialized from it.
- The **tracking set** is a query result, compiled into an immutable CSR file that is
  memory-mapped.
- A new mark writes an `ad.add` record and goes into the in-memory overlay, so it matches
  at once; the next rebuild folds it into the CSR.
- Detection needs only `Match` results plus each ad's duration, label and type.

## 2. Identity and records (`internal/record`)

### Node identity

- Each node has an ed25519 key pair in `data_dir/identity.key` (`adsvc ed25519 seed <hex>`,
  mode 0600), created on first start. The private key is never stored in the catalogue.
- The public key in hex is the node's **origin id**; `adsvc identity` prints it.
- The id is pseudonymous, but stable, so it links everything a node publishes.

### Ad id

- The first 16 bytes of `SHA-256(fp_version ‖ canonical points)`, shown in UUID form
  (`fingerprint.IDFor`).
- An exact copy relayed through any number of peers is one ad, and a relay cannot swap the
  points under an id.
- Near-duplicates (the same ad enrolled with slightly different boundaries) are separate
  ids, linked by `ad.dup` to a canonical one.

### Record envelope

```
{kind, origin, seq, ts, body, sig}
```

| Field | Meaning |
|---|---|
| `origin` | the author's public key |
| `seq` | per origin, monotonic |
| `ts` | milliseconds, from a hybrid logical clock (last-writer-wins stays stable against clock skew) |
| `body` | JSON bytes, stored verbatim |
| `sig` | ed25519 over a domain prefix and every field; the record hash is SHA-256 of the same bytes |

- A record is unique on `(origin, seq)` and on its hash.
- On the wire each record travels in a binary frame (minimal varints, then the signature),
  so the signed JSON is never re-encoded.
- Unknown JSON fields are ignored. Unknown kinds are stored and relayed but not
  materialized.
- A record more than 10 minutes in the future is rejected and never moves a clock.

### Kinds

All durations and positions are **integer milliseconds**.

| Kind | Body | Semantics |
|---|---|---|
| `ad.add` | `{id, fp_version, duration_ms, points, type?: "ad"\|"intro", source?: {file_key, start_ms, end_ms}, supersedes?}` | adds a fingerprint; `source` says where it was enrolled from |
| `ad.label` | `{ad, label, type?}` | last writer wins by `(ts, origin)`; this node's own label always wins locally. `type` (`ad` or `intro`) changes the ad's type the same way; a label without one leaves it alone |
| `ad.vote` | `{ad, value: -1\|+1, reason: good\|not_ad\|boundary\|dup, file_key?, start_ms?}` | only the latest vote per `(ad, origin)` counts |
| `ad.dup` | `{ad, canonical}` | near-duplicate link |
| `ad.retract` | `{ad}` | valid only from the ad's author |
| `file.map` | `{key, aliases, size, duration_ms, analyzed_ms, ads: [{ad, start_ms, end_ms, score, confirmed}]}` | replaces this origin's map for `key` |

- A body never contains a URL or a file name. An ad marked without a label is
  `ad-<short id>` (`intro-<short id>` for an intro).
- A `file.map` carries only confirmed ads (no analysed ranges), is published only when that
  set changes, and its `ts` is rounded to the start of the day.

### Points encoding

- `fingerprint.Point` holds a 24-bit hash (`f1<<15 | f2<<6 | dt`) and a frame number `T`.
- Points are grouped by anchor frame: `varint ΔT, varint count, count × 3-byte hash`.
- That is about 3.2 bytes per point: at about 180 points/s, a 30 s ad takes about 17 KB.
  Hashes are random, so general-purpose compression gains little.

## 3. SQLite catalogue (`internal/catalog`)

### Driver and connections

- `modernc.org/sqlite`: pure Go, no CGO, runs on android/arm64, arm and amd64.
- Pragmas are set per connection through `_pragma` DSN parameters: `journal_mode=WAL`,
  `synchronous=NORMAL`, `busy_timeout=5000`, `foreign_keys=ON`, `temp_store=MEMORY`,
  `trusted_schema=OFF`. No `mmap_size`, and the default `cache_size` (2 MB): the CSR
  relies on the page cache too, and the list queries read stored columns and indexes
  (`label_fold`, `analyzed_ms`, `ads_created`) rather than scanning.
- One writer connection, plus a read-only pool of 4 for the API, the web page and index
  builds. `catalog.View` gives one read snapshot.

### Migrations

- Embedded files `migrations/NNNN_name.sql`, up-only, recorded in `schema_migrations`.
- Applied at `catalog.Open`, sorted by parsed version, **one transaction per file**.
- Duplicate versions and gaps are errors. A catalogue newer than the binary (a snapshot from
  a newer peer, say) is refused and left untouched.

### Tables

All times are integer milliseconds.

**Replicated** (in snapshots):

| Table | Contents |
|---|---|
| `records` | `id` (the local receive sequence peers pull by), `hash`, `origin`, `seq`, `kind`, `ts`, `body`, `sig`, `via` (the node it came from; NULL = written here), `received` |

**Materialized** from the records:

| Table | Contents |
|---|---|
| `ads` | `id`, `fp_version`, `label`, `type` (`ad`/`intro`, as added), `type_eff`, `duration_ms`, `n_points`, `points`, `created`, `source_key`/`source_start_ms`/`source_end_ms`, `author`, `author_ts`, `record` |
| `ad_labels` | per `(ad, origin)`: the latest label |
| `ad_types` | per `(ad, origin)`: the latest type that origin named (`ad.label` with a `type`); `ads.type_eff` is this node's own, else the latest of any origin, else the one the ad was added with |
| `votes` | per `(ad, origin)`: `value`, `reason`, `file_key`, `start_ms` |
| `dups` | per `(ad, origin)`: `canonical` |
| `retractions` | `(ad, origin)` |
| `ad_records` | the records about each ad, for its history |
| `file_maps`, `file_aliases`, `detections` | this node's own file maps |
| `peer_maps`, `peer_aliases`, `peer_detections` | other origins' file maps |

**Local-only** (local opinion and state, dropped from snapshots):

| Table | Contents |
|---|---|
| `origin_policy` | per origin: `name`, `weight`, `blocked`, `managed` (set from the config file) |
| `peers` | per configured peer name: `node_id`, pull and push cursors, last success or error, received, rejections by reason |
| `pushers` | per node that pushed: last push, received, duplicates, rejections by reason |
| `pins` | ads always in the tracking set |
| `published_maps` | digest of the last `file.map` published per key |
| `meta` | e.g. the digest of the last CSR build |
| `reports` | ads a user saw, to mark later (§8). **The one table with URLs**, private to its user and the admins. Share a catalogue only as a snapshot (`adsvc export`, `/sync/snapshot`), never as the raw `catalogue.db` |

Peer URLs and credentials live in the config file, not here.

## 4. Trust and tracking selection

Trust is **local policy**: computed on each node, never replicated.

```
trust(ad) = w(author)                                   # implicit +1 from the author
          + Σ_origin w(origin) · vote(origin, ad)
          + Σ_origin 0.5 · w(origin) · [origin has a confirmed detection of ad]
```

| Origin | Weight `w` |
|---|---|
| this node | `trust.self` (default 10) |
| unknown origins | `trust.unknown` (default 0.2) |
| listed origins | `trust.origins`, or set on the Origins page |
| blocked origins | 0; their records are dropped on import and never relayed |

- Minting keys is free, so a crowd of unknown origins counts only when the admin raises
  their weight.
- Per-origin state is last-writer-wins by `(ts, seq)`. The earliest `ad.add` (by time, then
  origin) names the author. Ingesting records in any order gives the same state.
- Removing this node's own ad publishes `ad.retract`. Removing another node's ad publishes
  a −1 `not_ad` vote, since only the author can retract. Both can be undone by marking the
  same audio again (the same id): the author's new `ad.add` is newer than the retraction, which
  then no longer counts (table `ad_adds`: the latest add per origin), and a +1 `good` vote
  replaces the −1.

### Tracking set

Active ads of the current `fp_version`, pinned ones first, then those with
`trust ≥ trust.min_trust`, most trusted first, at most `tracking.max_ads`. Duplicates are
left out: the canonical ad matches for them.

### File maps at play time

When a session starts, its keys (primary key and aliases) are looked up and every origin's
detections are merged:

1. Each detection's ad is mapped to its canonical id, and detections are clustered by
   (canonical ad, start within ±0.25 s).
2. A cluster is accepted if its ad is trusted and the summed origin weights reach
   `trust.file_map_min`. It is confirmed if any accepted origin confirmed it.
3. Coverage (`analyzed`) comes **only from this node's own map**: a peer's claim that a
   range has no ads does not stop local analysis.

Other nodes' detections are shown, never stored in or republished from this node's own
maps.

### Who produces trash

The Origins and Peers pages (`catalog.Origins`, `catalog.Peers`) show:

- **per origin**: ads authored, how many a trusted origin (one whose weight alone reaches
  `min_trust`) voted `not_ad` or `boundary`, duplicates, retractions;
- **per peer or pusher**: records received, rejections by reason, and the share of relayed
  ads that became trash.

The Files page flags a peer's detection in a range this node analysed without finding it.

## 5. CSR tracking index (`internal/library`)

### Layout

A posting is one landmark of one ad, packed in a `u32` as `dense ad:18 | frame:14`. Hash
lookup is two-level:

- `top`: 2^16 + 1 entries, indexed by the top 16 bits of the hash, each pointing to a range
  of distinct hashes;
- `low`: the low 8 bits of each distinct hash, sorted within its range and binary-searched;
- `off`: an offset into `postings` for each distinct hash;
- `postings`: sorted by (ad, frame) within each hash.

A flat `2^24 × u32` table would cost 64 MB whatever the library size; the two-level form
costs 5 bytes per distinct hash plus 256 KB.

Limits: 18 bits for the ad (`library.MaxAds` = 262,144) and 14 bits for the frame
(`library.MaxDuration` = 524 s per ad; `Add` returns `ErrTooLong` beyond that).

### Voting

Votes go into a pooled **dense tally**: one run of `u16` cells per ad, one cell per offset
the query can align it at, with a guard cell at each end. Only touched cells are read and
reset. It takes about 27 MB per concurrent `Match` at 10k ads; an index too big for
2^28 cells falls back to map voting.

### Measured

Synthetic landmarks shaped like real ones (dt and frequency skew from the ads of the
Babylon bench). 10k ads of 20–45 s is about 61M postings. Apple M3 Pro,
`go test -bench . ./internal/library`:

| | map index | CSR, map votes | CSR + dense tally |
|---|---|---|---|
| index heap, 10k ads | 781 MiB | 237.5 MiB | 237.5 MiB |
| build, 10k ads | 4.08 s | 0.42 s | 0.42 s |
| Match, 10 s block of programme audio | 10.3 ms | 9.8 ms | **2.29 ms** |
| Match, 10 s block inside an ad | 21.4 ms | 20.8 ms | **4.80 ms** |
| Match allocations | 9–18 MiB | same | ~0 (pooled) |

Built from the catalogue into the mapped file: 1.23 s and 490 MiB allocated; the open index
keeps **1.3 MiB** of heap, and the file is about 290 MB of page cache.

### Overlay, tombstones, rebuild

- New ads go into an in-memory map index (the overlay), so they match at once. Removed CSR
  ads get a tombstone.
- The CSR is rebuilt when the overlay exceeds max(64k, 1/16 of the CSR postings), when dead
  postings exceed max(64k, 1/8), or when the tracking set changes (`Library.Refresh`).
- During a rebuild `Match` keeps running on the old index. One rebuild runs at a time, and
  ads added or removed meanwhile are reconciled when the new index is installed.

### File

Little-endian, sections aligned to 8 bytes:

```
header    magic "ADSVCSR1", format, fp_version, hash_bits (=24), low_bits (=8), n_ads,
          n_distinct, n_postings, set_digest [32]B, built_at   (80 bytes)
ids       n_ads × 16 B        sizes  n_ads × u32        start  (n_ads + 1) × u32
top       (2^16 + 1) × u32    post   n_postings × u32
low       n_distinct × u8     off    (n_distinct + 1) × u32
```

- The postings come before `low` and `off`: they are written first, through a writable
  mapping, before `n_distinct` is known. Labels and durations stay in the catalogue.
- Built from one catalogue read snapshot: stream the tracking set in id order, count per
  top range and scatter, counting-sort each range by the low byte, write
  `tracking.csr.tmp`, fsync, rename, fsync the directory, map read-only, swap in.
- The file is reused while its `set_digest` matches the current tracking set, otherwise
  rebuilt.
- On open, the layout and every invariant `Match` relies on are validated (ranges ascending
  and in bounds, every posting within its ad's span), so a corrupt file is rebuilt, never
  used.
- `syscall.Mmap` read-only on unix (linux, android, darwin); other platforms read the file
  into one `[]byte`. The index lives in the page cache, not the Go heap, so the kernel can
  drop it under pressure.
- `Match` holds the library's read lock for its whole run; an install swaps the index under
  the write lock and unmaps the old one at once.

## 6. Config and SIGHUP (`internal/config`)

Every key is documented in [config.yml.example](../config.yml.example).

- The file is `-config <path>`, `$ADSVC_CONFIG`, or `config.yml` in the current directory
  when it exists. Precedence: **flags > env > file > defaults**. Users (`auth.users`) come
  from the file only; `-route` and `-allow` replace the file's lists.
- Unknown keys are errors.
- `proxy.Proxy` keeps its `Config` in an `atomic.Pointer`, and each request reads **one**
  snapshot, so a reload can never mix the auth decision of one config with the forwarding
  rules of another.

`SIGHUP`, or the Config page's Reload button, re-reads and validates the whole file. If it
is invalid, the old config stays and a warning is logged. Otherwise:

| Takes effect | Keys |
|---|---|
| at once | `auth.*`, `upstream.*`, `detect.max_stall`, `detect.idle_timeout`, `detect.confirm_score`, `log.*`, `trust.*`, `tracking.*`, `sync.*` |
| for new sessions | `detect.min_score`, `detect.mark_window`, `ffmpeg` |
| after a restart (logged) | `listen`, `data_dir`, `media.*` |

`trust.origins` rows are re-seeded on every reload and are read-only on the Origins page.

## 7. Federated sync (`internal/replica`)

Every node serves and consumes the same endpoints. There is no hub.

| Endpoint | Purpose |
|---|---|
| `GET /sync/info?nonce=` | `{node_id, name, fp_version, schema, head}`, signed over the caller's nonce, so a relay cannot pass itself off as another node |
| `GET /sync/log?after=<id>` | accepted records in receive order, as gzip-compressed record frames; never from blocked origins |
| `GET /sync/ad/{id}` | the `ad.add` record of one ad |
| `GET /sync/snapshot` | a SQLite file with only the records and the node id |
| `POST /sync/push` | records from a node that cannot be pulled from (behind NAT) |

- `/sync/*` needs a user's token unless `sync.public: true`. That is safe because the
  catalogue holds no secrets, and pushed records are still checked one by one.
- **Pull**: every `interval`, each peer in `pull` or `both` mode is asked for its log after
  the pull cursor. Records are stored with `via` = the peer's node id, and the cursor moves
  after each committed batch.
- **Push**: for peers in `push` or `both` mode, records newer than the push cursor are
  POSTed, except those that came from that same peer. Pushes are signed with
  `X-Adsvc-Node`, `X-Adsvc-Date` (±10 min) and `X-Adsvc-Signature` over the SHA-256 of the
  body; the receiver stores them with `via` = the pusher.
- A peer with a new identity, or a log shorter than the cursor, is synced from the start.
- **Snapshots** (`/sync/snapshot`, `adsvc export`) clear `via` and receive times and drop
  every local and derived table, including this node's own file maps (its watch history).
  `adsvc import` checks every record as if a peer had sent it.

### Import checks

Every record passes these checks in order. Each rejection is counted against the peer
(`peers.rejected`) or the pusher (`pushers.rejected`).

1. The envelope is well-formed; a record already held is a duplicate, not a rejection.
2. The signature is valid.
3. The origin is not blocked.
4. The origin is within its quota (`trust.quota_per_day`).
5. The body is valid for its kind: for `ad.add`, the id matches the points and the duration
   and points are within limits; `ad.retract` only from the author; `file.map` keys only in
   `ih:`/`c:`/`id:` form.

Records for an unknown `fp_version` are kept and relayed but never tracked.

## 8. Web page (`internal/admin`, assets in `web/`)

### Routing

At the root of the proxy's listener. `frontHandler` gives the proxy its own routes
(`proxy.APIPath`: `/s`, `/healthz`, `/ads/*`), `/sync/*` to the replica, and everything else
to the page.

### Access

- A login form trades any user's token for a session cookie (`adsvc_session`: HttpOnly,
  SameSite=Strict, Path `/`, Secure over TLS, 12 h). The cookie is `expiry ‖ key id ‖ HMAC`
  under a secret made at start, where the key id is an HMAC of the user's token. So the
  token is never in the cookie, a restart ends every session, and removing a token ends its
  sessions.
- A wrong token costs a second, one attempt at a time. Without any user token the page
  answers 404.
- Admins (`admin: true`) see every page and start at Ads. Other users see only Reports
  (other pages redirect there, or answer 403 to JSON or a POST).
- **CSRF**: every non-GET needs `X-Adsvc-Admin: 1` (htmx sends it) and passes `net/http`'s
  `CrossOriginProtection`. The login form is covered by `CrossOriginProtection` only.
- **Headers**: a strict Content-Security-Policy (`script-src 'self'`, `style-src 'self'`, no
  inline script or style), `X-Frame-Options: DENY`, `Referrer-Policy: same-origin`,
  `Vary: HX-Request`. htmx runs with eval off, script tags off and no history cache.

### Reports

A player that shows an ad it has no way to skip reports it:
`POST /ads/report?u=<source>[&id=|&ih=&idx=]&position=MS[&note=]`. The Reports page lists
the user's reports (an admin sees everyone's) by file, each with an mpv command
(`mpv --start=<t−20> '<base>/s?u=…'`, where base is `public_url` or the request's host), and
hands out `adskip.lua` with the user's token written in. While a file has an open report,
its session decodes everything, including ranges analysed before, so `/ads/mark` finds the
audio. A successful `/ads/mark` deletes the user's reports of that file within the ad
±15 s.

### Pages

| Page | Contents |
|---|---|
| **Reports** | (every user) reported ads by file, each with its mpv command and a delete button; the `adskip.lua` download and how to mark |
| **Ads** | search by label or id; filters: origin, type, pinned, fp version; newest first or by label, 100 per page (filtered, sorted and paged in SQL, on stored columns only); trust, votes and state shown for the page's rows; bulk vote, pin or delete; a dot for likely duplicates |
| **Ad** | edit the label and type (`ad.label`); vote with a reason (`ad.vote`); pin; mark as a duplicate (`ad.dup`); delete (`ad.retract` for this node's own ad, a −1 `not_ad` vote for another's); its records (author, via, time); the files it was found in; **quality**: the weakest self score and the strongest other tracked ad per window of detect's block, against `detect.min_score` |
| **Files** | the list, most recently updated first, 100 per page; by key: the merged map, per-origin detections side by side, disagreements highlighted, this node's own coverage |
| **Origins** | the trash statistics of §4; set a weight or block; config-managed rows are read-only |
| **Peers** | cursors, last success or error, received, rejections by reason, relayed trash |
| **Tracking** | ads, postings, file size, overlay, last build, and a rebuild button |
| **Config** | the effective config with secrets redacted, and a reload button (as SIGHUP) |

Every change that affects trust (vote, pin, dup, origin policy) calls `Library.Refresh` in
the admin's root context, so the index follows even if the request goes away.

### Formats, language, assets

- Pages are rendered on the server with `html/template`; htmx swaps fragments. Each route
  also answers with JSON under `/api/` (or with `Accept: application/json`); POSTs are
  form-encoded and need the same cookie and header.
- English and Russian (`web/i18n/*.json`), chosen from `Accept-Language` and translated on
  the server, so fragments arrive translated. A test checks that both files have the same
  keys and that the templates use no missing key.
- Assets are embedded from an explicit list and served under `/static/`. Each URL carries a
  hash of that file's contents (`?v=`): the matching response is `immutable`, any other
  `no-cache`. htmx is vendored, so the page works offline. `static/js/admin.js` is served
  as written, with no build step.
