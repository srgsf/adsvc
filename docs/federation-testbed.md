# Federation testbed

Five real adsvc servers with synthetic data, to see on the web page what federation does:
trust, trash producers, collisions, blocking, the pagers, and how records travel over
several hops. Nothing here is audio: the fingerprints are random landmarks, so the
testbed exercises the catalogue and the pages, not matching.

```sh
make build                      # dist/adsvc for this machine (needs the container runtime)
make fed-data                   # writes .cache/fed (about 40 s, 250 MB)
make fed-up                     # starts the five nodes; fed-status, fed-down
```

`make fed-data FED_ARGS='…'` takes the flags of `internal/testdb/cmd/adsgen`
(`-ads`, `-files`, `-collisions`, `-trash-ads`, `-trash-maps`, `-days`, `-seed`,
`-honest`, `-hosts`, `-import=false`). It replaces `.cache/fed`, so nodes start fresh.
`.cache/fed/PLAN.txt` lists the origin ids and the admin token.

## Origins (who wrote the records)

Each is a made-up node with its own key. They are not servers: their records reach the
nodes as snapshots (`.cache/fed/snapshots/<name>.db`, the file `adsvc import` reads) or
through sync.

| Origin | Publishes | Default size |
|---|---|---|
| `honest-1…3` | ads with labels (30% unlabelled: `ad-<id>`), and file maps confirming one to three ads per file; each file is confirmed by about half of them | 900 ads, 2500 files in all |
| `curator` | `+1 good` on 70% of the honest ads, a `boundary` −1 on 5%, `−1 not_ad` on 60% of the trash, and `ad.dup` marks on two thirds of the collisions | about 2700 records |
| `collider` | copies of honest ads: near-duplicates (92% of the landmarks, shifted), half an ad, and a shared 4 s music bed | 60 ads |
| `trash` | short ads (2–8 s) and file maps with 5–25 low-score detections each | 3500 ads + 1800 maps: 5300 records, over the default daily quota of 5000 |

## Nodes (who reads them)

Ports 8081–8085, all on `:PORT`. Every node has the users `admin` (web page; the token is
in PLAN.txt) and `sync` (the token nodes use on each other). Peers sync every minute, the
minimum, so each hop of a relay costs up to a minute, and a large log takes a few rounds.

| Node | Imports | Trust | Peers | What it shows |
|---|---|---|---|---|
| `hub` 8081 | honest-*, curator | honest 2, curator 3, unknown 0.2 | none (receives) | a healthy node; trash arrives only by the spoke's push |
| `spoke` 8083 | collider, trash | defaults | pushes to hub | a node behind NAT; the hub's Peers page counts what it pushed |
| `mirror` 8082 | nothing | as hub | pulls hub | initial sync of about 13,000 records over several rounds; relay: the trash comes via the hub |
| `stranger` 8084 | nothing | trash **blocked** | pulls mirror | two hops away; blocked records are rejected and counted |
| `naive` 8085 | everything | unknown 1, `max_ads` 1000 | none | no policy: trash tracked, the tracking cap in force |

Start the hub first (`fed-up` does): a peer whose first round fails waits a minute.

### On several machines

`make fed-data FED_ARGS='-hosts hub=192.168.0.10,spoke=192.168.0.11,mirror=192.168.0.12,stranger=192.168.0.13'`
then copy `.cache/fed/nodes/<name>` (config, data) and `dist/adsvc` (+ `ffmpeg` beside
it) to its machine, and run `adsvc proxy -config nodes/<name>/config.yml`. `data_dir` in the
configs is an absolute path of this machine: edit it. The peers' URLs use the `-hosts`.

## What to look at

- **Ads pager** (`/ads`): hub, mirror and naive have 4460 ads (45 pages of 100), the
  stranger 960. Sort, search a label (`Ozon`, Cyrillic `промо`), filter by state.
- **Files pager** (`/files`): 2500 files on every node that has the honest maps.
- **Collisions**: on the Ads list the dot column: rose on the 40 pairs the curator marked as
  duplicates, amber on the 20 unmarked ones whose collision score is over `min_score`; the
  Ad page shows the banner and links the other ad. Near-duplicates collide hard, halves
  less, music beds least (they may stay below the threshold: that is a finding too).
- **Trash producers** (`/origins`, `/peers`): on the hub, `trash` has 3500 ads and about
  2100 trashed (the curator's `not_ad` votes at weight 3); on the Peers page, the spoke as
  a pusher, with the share of what it pushed that became trash. Quota: the trash origin has 5300
  records against 5000 a day, so the first node to receive them (the spoke and the naive
  node, at import; `fed-data` prints `rejected map[quota:300]`) drops 300, and only 5000
  travel on.
- **Exclusion**: on the stranger the trash origin is blocked: none of its records are
  stored, its rows show `blocked`, and the Peers page counts the rejections against the
  mirror, which relayed them. On the naive node they are all tracked (unknown weight 1
  passes `min_trust`), then cut at 1000 ads by `max_ads`: see the Tracking page.
- **Trust changes**: on the hub, raise the collider to 2 (Origins page), or block the
  trash origin: tracking, Ads states and the duplicate dots update with the next
  refresh. Peers that already pulled keep what they have.
- **Restart and re-sync**: stop a node, delete its `data`, start it: it is a new identity
  and pulls from the start (the peers see a "peer reset"). Or stop the hub, add an ad on
  the spoke, restart the hub and watch the push catch up.

## Snapshots by hand

```sh
dist/adsvc import -data-dir .cache/fed/nodes/stranger/data .cache/fed/snapshots/collider.db
```

`-import=false` skips the seeding in `fed-data` and leaves empty data directories for
this.
