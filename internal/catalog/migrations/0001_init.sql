-- The catalogue may be handed to anyone: it holds no secrets, no URLs and no file names.
-- Files are identified by key only (ih:<infohash>/<idx>, c:<content key>, id:<hash>).
-- Durations and positions are integer milliseconds.

-- Signed records (package record) and what they materialize into.

-- The replication log. id is this node's receive order: the cursor peers pull by.
CREATE TABLE records (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	hash     BLOB NOT NULL UNIQUE,
	origin   BLOB NOT NULL,
	seq      INTEGER NOT NULL,
	kind     TEXT NOT NULL,
	ts       INTEGER NOT NULL,         -- unix ms, the origin's clock
	body     BLOB NOT NULL,            -- JSON, exactly as signed
	sig      BLOB NOT NULL,
	via      BLOB,                     -- node it came from; NULL = written here
	received INTEGER NOT NULL,         -- unix ms, this node's clock
	UNIQUE (origin, seq)
) STRICT;
CREATE INDEX records_origin_received ON records (origin, received);

-- Reference ads. id = fingerprint.IDOf(points), so the same landmarks have the same id
-- on every node.
CREATE TABLE ads (
	id           BLOB PRIMARY KEY CHECK (length(id) = 16),
	fp_version   INTEGER NOT NULL,
	label        TEXT NOT NULL,
	duration_ms  INTEGER NOT NULL,
	n_points     INTEGER NOT NULL,
	points       BLOB NOT NULL,            -- fingerprint.Encode
	created      INTEGER NOT NULL,         -- unix milliseconds
	source_key      TEXT,                  -- file key the ad was enrolled from, if known
	source_start_ms INTEGER,               -- position in that file
	source_end_ms   INTEGER,
	-- The author of an ad is the origin of its earliest ad.add; record is that ad.add.
	author       BLOB,
	author_ts    INTEGER,
	record       INTEGER REFERENCES records (id)
) STRICT;

-- Per-file ad maps.
CREATE TABLE file_maps (
	key        TEXT PRIMARY KEY,
	fp_version INTEGER NOT NULL,           -- fingerprint version analyzed was computed with
	size        INTEGER NOT NULL,
	duration_ms INTEGER NOT NULL,
	analyzed    TEXT NOT NULL DEFAULT '[]', -- JSON [[from_ms, to_ms], ...]
	updated    INTEGER NOT NULL            -- unix milliseconds
) STRICT;

CREATE TABLE file_aliases (
	alias TEXT PRIMARY KEY,
	key   TEXT NOT NULL REFERENCES file_maps (key) ON DELETE CASCADE ON UPDATE CASCADE
) STRICT;
CREATE INDEX file_aliases_key ON file_aliases (key);

CREATE TABLE detections (
	key       TEXT NOT NULL REFERENCES file_maps (key) ON DELETE CASCADE ON UPDATE CASCADE,
	ad        BLOB NOT NULL,
	start_ms  INTEGER NOT NULL,
	end_ms    INTEGER NOT NULL,
	score     INTEGER NOT NULL,
	confirmed INTEGER NOT NULL,            -- 0 or 1
	PRIMARY KEY (key, ad, start_ms)
) STRICT;
CREATE INDEX detections_ad ON detections (ad);

-- Local state and settings of this node.
CREATE TABLE meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
) STRICT;

-- The latest label, vote and duplicate claim of each origin, per ad.
CREATE TABLE ad_labels (
	ad     BLOB NOT NULL,
	origin BLOB NOT NULL,
	label  TEXT NOT NULL,
	ts     INTEGER NOT NULL,
	seq    INTEGER NOT NULL,
	PRIMARY KEY (ad, origin)
) STRICT;

CREATE TABLE votes (
	ad       BLOB NOT NULL,
	origin   BLOB NOT NULL,
	value    INTEGER NOT NULL,         -- +1 or -1
	reason   TEXT NOT NULL,
	file_key TEXT,
	start_ms INTEGER,
	ts       INTEGER NOT NULL,
	seq      INTEGER NOT NULL,
	PRIMARY KEY (ad, origin)
) STRICT;

CREATE TABLE dups (
	ad        BLOB NOT NULL,
	origin    BLOB NOT NULL,
	canonical BLOB NOT NULL,
	ts        INTEGER NOT NULL,
	seq       INTEGER NOT NULL,
	PRIMARY KEY (ad, origin)
) STRICT;

-- Retractions count only when made by the ad's author.
CREATE TABLE retractions (
	ad     BLOB NOT NULL,
	origin BLOB NOT NULL,
	ts     INTEGER NOT NULL,
	PRIMARY KEY (ad, origin)
) STRICT;

-- File maps of other origins (this node's own are file_maps / detections).
CREATE TABLE peer_maps (
	key         TEXT NOT NULL,
	origin      BLOB NOT NULL,
	fp_version  INTEGER NOT NULL,
	size        INTEGER NOT NULL,
	duration_ms INTEGER NOT NULL,
	ts          INTEGER NOT NULL,
	seq         INTEGER NOT NULL,
	PRIMARY KEY (key, origin)
) STRICT;

CREATE TABLE peer_aliases (
	alias  TEXT NOT NULL,
	origin BLOB NOT NULL,
	key    TEXT NOT NULL,
	PRIMARY KEY (alias, origin),
	FOREIGN KEY (key, origin) REFERENCES peer_maps (key, origin) ON DELETE CASCADE
) STRICT;

CREATE TABLE peer_detections (
	key       TEXT NOT NULL,
	origin    BLOB NOT NULL,
	ad        BLOB NOT NULL,
	start_ms  INTEGER NOT NULL,
	end_ms    INTEGER NOT NULL,
	score     INTEGER NOT NULL,
	confirmed INTEGER NOT NULL,
	PRIMARY KEY (key, origin, ad, start_ms),
	FOREIGN KEY (key, origin) REFERENCES peer_maps (key, origin) ON DELETE CASCADE
) STRICT;
CREATE INDEX peer_detections_ad ON peer_detections (ad);

-- Local only (dropped from snapshots): this node's opinion of origins, its peers, its
-- pins, and what it last published per file.
CREATE TABLE origin_policy (
	origin  BLOB PRIMARY KEY,
	name    TEXT NOT NULL DEFAULT '',
	weight  REAL,                      -- NULL = the default for unknown origins
	blocked INTEGER NOT NULL DEFAULT 0,
	managed INTEGER NOT NULL DEFAULT 0 -- set from the config file
) STRICT;

CREATE TABLE peers (
	name        TEXT PRIMARY KEY,      -- as in the config file (its URL stays there)
	node_id     BLOB,
	pull_cursor INTEGER NOT NULL DEFAULT 0,
	push_cursor INTEGER NOT NULL DEFAULT 0,
	last_ok     INTEGER,
	last_error  TEXT NOT NULL DEFAULT '',
	received    INTEGER NOT NULL DEFAULT 0,
	rejected    TEXT NOT NULL DEFAULT '{}' -- JSON: reason -> count
) STRICT;

CREATE TABLE pins (
	ad BLOB PRIMARY KEY
) STRICT;

CREATE TABLE published_maps (
	key    TEXT PRIMARY KEY,
	digest BLOB NOT NULL
) STRICT;

-- The records about each ad (ad.add, ad.label, ad.vote, ad.dup, ad.retract): its history.
-- Derived from records, like the other materialized tables. An ad.dup is filed under
-- both of its ads.
CREATE TABLE ad_records (
	ad     BLOB NOT NULL,
	record INTEGER NOT NULL REFERENCES records (id),
	PRIMARY KEY (ad, record)
) STRICT, WITHOUT ROWID;

-- Local only: nodes that pushed records to this one, and what became of them (pulls are
-- counted in peers). Times are unix ms.
CREATE TABLE pushers (
	node_id   BLOB PRIMARY KEY,
	last_push INTEGER NOT NULL,
	received  INTEGER NOT NULL DEFAULT 0,
	duplicate INTEGER NOT NULL DEFAULT 0,
	rejected  TEXT NOT NULL DEFAULT '{}' -- JSON: reason -> count
) STRICT;

-- Ad reports: a user saw an ad at a position of a file and wants to mark it later (from
-- the web page, in mpv). Local only and private to that user: unlike the rest of the
-- catalogue a report holds the source URL, as the client sent it, so that a play link can
-- be made of it. Snapshots drop the table; share a catalogue only through export.
-- file_key is the file key when the request named one (id, ih/idx, a TorrServer URL),
-- norm the normalised URL; sel is the rest of the selector (id=, ih=&idx=) as a query
-- string. Times are ms: pos_ms on the media timeline, created unix ms.
CREATE TABLE reports (
	id       INTEGER PRIMARY KEY,
	user     TEXT NOT NULL,
	file_key TEXT NOT NULL,
	norm     TEXT NOT NULL,
	url      TEXT NOT NULL,
	sel      TEXT NOT NULL DEFAULT '',
	pos_ms   INTEGER NOT NULL,
	note     TEXT NOT NULL DEFAULT '',
	created  INTEGER NOT NULL
) STRICT;

CREATE INDEX reports_user ON reports (user, created);
