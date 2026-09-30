// Package catalog is the SQLite store of reference ads and per-file ad maps
// (docs/database.md). It holds no secrets, URLs or file names, so the file can be shared.
//
// Writes go through one connection (SQLite has one writer at a time anyway); reads use a
// separate read-only pool, so a long read (building the index) does not block writes.
package catalog

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"modernc.org/sqlite" // database/sql driver "sqlite", pure Go

	"github.com/srgsf/adsvc/internal/adtype"
	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/record"
)

// DB is an open catalogue.
type DB struct {
	path string
	uri  string  // file: URI of path ("" in memory)
	w    *sql.DB // one connection: all writes
	r    *sql.DB // read-only pool (w itself for an in-memory catalogue)

	self   atomic.Pointer[identity] // set by SetIdentity
	share  atomic.Pointer[Share]
	policy atomic.Pointer[Policy]
}

// Open opens (creating it if needed) the catalogue at path and brings its schema up to
// date. An empty path is an in-memory catalogue.
func Open(ctx context.Context, path string) (*DB, error) {
	db, err := open(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("catalogue %s: %w", path, err)
	}
	return db, nil
}

func open(ctx context.Context, path string) (*DB, error) {
	pragmas := []string{"busy_timeout(5000)", "foreign_keys(1)", "temp_store(MEMORY)", "trusted_schema(0)"}
	if path == "" {
		w, err := sql.Open("sqlite", dsn("file::memory:", pragmas, nil))
		if err != nil {
			return nil, err
		}
		w.SetMaxOpenConns(1) // every connection would be a different empty database
		db := &DB{w: w, r: w}
		return db, db.init(ctx)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uri := (&url.URL{Scheme: "file", Path: "/" + strings.TrimPrefix(filepath.ToSlash(abs), "/")}).String()
	w, err := sql.Open("sqlite", dsn(uri, append(pragmas, "journal_mode(WAL)", "synchronous(NORMAL)"), url.Values{"_txlock": {"immediate"}}))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	db := &DB{path: path, uri: uri, w: w}
	if err := db.init(ctx); err != nil {
		w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite", dsn(uri, append(pragmas, "query_only(1)"), url.Values{"mode": {"ro"}}))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	db.r = r
	return db, nil
}

// fold(s) is s in lower case, for case-insensitive search: SQLite's lower() knows ASCII
// only, and labels are in any language. NULL stays NULL.
func init() {
	sqlite.MustRegisterDeterministicScalarFunction("fold", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		switch v := args[0].(type) {
		case string:
			return strings.ToLower(v), nil
		case []byte:
			return strings.ToLower(string(v)), nil
		}
		return args[0], nil
	})
}

func dsn(uri string, pragmas []string, q url.Values) string {
	if q == nil {
		q = url.Values{}
	}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	return uri + "?" + q.Encode()
}

func (db *DB) init(ctx context.Context) error {
	if err := db.w.PingContext(ctx); err != nil {
		return err
	}
	return migrate(ctx, db.w, migrationsFS())
}

// Path is the file of the catalogue ("" when in memory).
func (db *DB) Path() string { return db.path }

// Close closes the catalogue.
func (db *DB) Close() error {
	var err error
	if db.r != db.w {
		err = db.r.Close()
	}
	return errors.Join(err, db.w.Close())
}

// Ad is a reference ad without its landmarks.
type Ad struct {
	ID         fingerprint.ID
	FPVersion  int
	Label      string
	Type       string // adtype.Ad or adtype.Intro (the effective one, as labels are)
	DurationMs int32
	NPoints    int
	Created    time.Time
	Source     *Source       // where it was enrolled from, when known
	Author     record.Origin // the origin of its earliest ad.add; zero if not published
	Trust      float64       // filled in by Tracking
	Pinned     bool          // filled in by Tracking
}

// Source is the file and time range an ad was enrolled from.
type Source struct {
	Key            string // a file key: ih:…, c:… or id:…
	StartMs, EndMs int32
}

// PutAd stores an ad with its encoded landmarks. Storing an ad that exists (same id)
// changes nothing and reports added = false.
func (db *DB) PutAd(ctx context.Context, a Ad, points []byte) (added bool, err error) {
	var key sql.NullString
	var start, end sql.NullInt32
	if a.Source != nil {
		key = sql.NullString{String: a.Source.Key, Valid: true}
		start = sql.NullInt32{Int32: a.Source.StartMs, Valid: true}
		end = sql.NullInt32{Int32: a.Source.EndMs, Valid: true}
	}
	res, err := db.w.ExecContext(ctx, `INSERT INTO ads
		(id, fp_version, label, type, duration_ms, n_points, points, created, source_key, source_start_ms, source_end_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		a.ID[:], a.FPVersion, a.Label, adtype.Of(a.Type), a.DurationMs, a.NPoints, points, a.Created.UnixMilli(), key, start, end)
	if err != nil {
		return false, fmt.Errorf("storing ad %s: %w", a.ID, err)
	}
	n, err := res.RowsAffected()
	if err == nil && n > 0 {
		err = setLabels(ctx, db.w, db.Self(), `a.id = :id`, sql.Named("id", a.ID[:]))
	}
	return n > 0, err
}

// deleteAd removes an ad and its detections from every file map. It reports whether the
// ad existed.
func (db *DB) deleteAd(ctx context.Context, id fingerprint.ID) (bool, error) {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	res, err := tx.ExecContext(ctx, `DELETE FROM ads WHERE id = ?`, id[:])
	if err != nil {
		return false, fmt.Errorf("deleting ad %s: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM detections WHERE ad = ?`, id[:]); err != nil {
		return false, fmt.Errorf("deleting detections of ad %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, tx.Commit()
}

// querier runs reads: the read pool, or one read transaction (View).
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Ads returns every ad of fingerprint version fpVersion, by id.
func (db *DB) Ads(ctx context.Context, fpVersion int) ([]Ad, error) {
	return ads(ctx, db.r, fpVersion, db.Self())
}

// EachPoints calls fn with the encoded landmarks of every ad of fingerprint version
// fpVersion, by id. fn must not use db (the rows hold a connection) nor keep points.
func (db *DB) EachPoints(ctx context.Context, fpVersion int, fn func(id fingerprint.ID, points []byte) error) error {
	return eachPoints(ctx, db.r, fpVersion, fn)
}

// Points returns the encoded landmarks of one ad, or nil if there is no such ad.
func (db *DB) Points(ctx context.Context, id fingerprint.ID) ([]byte, error) {
	var b []byte
	err := db.r.QueryRowContext(ctx, `SELECT points FROM ads WHERE id = ?`, id[:]).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading landmarks of %s: %w", id, err)
	}
	return b, nil
}

// View is a consistent snapshot of the catalogue: reads through it all see the same data,
// whatever is written meanwhile.
type View struct {
	db *DB
	tx *sql.Tx
}

// View calls fn with a snapshot of the catalogue. fn must not write to db: for an
// in-memory catalogue, the snapshot holds its only connection.
func (db *DB) View(ctx context.Context, fn func(v View) error) error {
	tx, err := db.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("opening a snapshot: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read only
	return fn(View{db, tx})
}

// Ads is DB.Ads on the snapshot.
func (v View) Ads(ctx context.Context, fpVersion int) ([]Ad, error) {
	return ads(ctx, v.tx, fpVersion, v.db.Self())
}

// EachPoints is DB.EachPoints on the snapshot.
func (v View) EachPoints(ctx context.Context, fpVersion int, fn func(id fingerprint.ID, points []byte) error) error {
	return eachPoints(ctx, v.tx, fpVersion, fn)
}

// labelOf is the effective label of ads a: this node's own label, else the latest label
// of any origin, else the label the ad was added with. It takes the named argument :self.
const labelOf = `COALESCE((SELECT l.label FROM ad_labels l WHERE l.ad = a.id
	ORDER BY l.origin = :self DESC, l.ts DESC, l.origin DESC LIMIT 1), a.label)`

// typeOf is the effective type of ads a, chosen like the label among the origins that
// named one (a label record without a type names none). It takes :self.
const typeOf = `COALESCE((SELECT t.type FROM ad_types t WHERE t.ad = a.id
	ORDER BY t.origin = :self DESC, t.ts DESC, t.origin DESC LIMIT 1), a.type)`

// setLabels stores the effective label (labelOf, for this node self) of the ads a that
// match cond in label_eff and label_fold, which the list queries search and sort by, and
// the effective type (typeOf) in type_eff. It runs wherever a label may change: an ad
// stored, a label record, a new identity.
func setLabels(ctx context.Context, x interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, self record.Origin, cond string, args ...any) error {
	_, err := x.ExecContext(ctx, `UPDATE ads AS a SET label_eff = `+labelOf+`, label_fold = fold(`+labelOf+`),
		type_eff = `+typeOf+`
		WHERE `+cond, append([]any{sql.Named("self", self[:])}, args...)...)
	if err != nil {
		return fmt.Errorf("storing effective labels: %w", err)
	}
	return nil
}

func ads(ctx context.Context, q querier, fpVersion int, self record.Origin) ([]Ad, error) {
	return listAds(ctx, q, self, `a.fp_version = :fp ORDER BY a.id`, sql.Named("fp", fpVersion))
}

// listAds lists the ads of table ads a that match cond (a constant SQL condition, with
// ORDER BY and LIMIT if wanted, that takes named arguments args).
func listAds(ctx context.Context, q querier, self record.Origin, cond string, args ...any) ([]Ad, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.id, a.fp_version, a.label_eff, a.type_eff, a.duration_ms, a.n_points, a.created,
		a.source_key, a.source_start_ms, a.source_end_ms, a.author FROM ads a WHERE `+cond,
		append([]any{sql.Named("self", self[:])}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("listing ads: %w", err)
	}
	defer rows.Close()
	var out []Ad
	for rows.Next() {
		var a Ad
		var id []byte
		var created int64
		var key sql.NullString
		var start, end sql.NullInt32
		var author []byte
		if err := rows.Scan(&id, &a.FPVersion, &a.Label, &a.Type, &a.DurationMs, &a.NPoints, &created, &key, &start, &end, &author); err != nil {
			return nil, fmt.Errorf("listing ads: %w", err)
		}
		copy(a.ID[:], id)
		copy(a.Author[:], author)
		a.Created = time.UnixMilli(created)
		if key.Valid {
			a.Source = &Source{Key: key.String, StartMs: start.Int32, EndMs: end.Int32}
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing ads: %w", err)
	}
	return out, nil
}

func eachPoints(ctx context.Context, q querier, fpVersion int, fn func(id fingerprint.ID, points []byte) error) error {
	return eachPointsWhere(ctx, q, `fp_version = ?`, fpVersion, fn)
}

// EachPointsOf is EachPoints for the ads ids (those that exist), by id: what an index
// build reads, rather than every ad of the version.
func (v View) EachPointsOf(ctx context.Context, ids []fingerprint.ID, fn func(id fingerprint.ID, points []byte) error) error {
	if len(ids) == 0 {
		return nil
	}
	return eachPointsWhere(ctx, v.tx, adIn("id"), sql.Named("ids", adIDsOf(ids)), fn)
}

func eachPointsWhere(ctx context.Context, q querier, cond string, arg any, fn func(id fingerprint.ID, points []byte) error) error {
	rows, err := q.QueryContext(ctx, `SELECT id, points FROM ads WHERE `+cond+` ORDER BY id`, arg)
	if err != nil {
		return fmt.Errorf("reading landmarks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw, points sql.RawBytes
		if err := rows.Scan(&raw, &points); err != nil {
			return fmt.Errorf("reading landmarks: %w", err)
		}
		var id fingerprint.ID
		copy(id[:], raw)
		if err := fn(id, points); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading landmarks: %w", err)
	}
	return nil
}

// FileMap is what is known about one file. Times are milliseconds (int32: up to 24 days).
type FileMap struct {
	Key        string
	Aliases    []string
	FPVersion  int // of Analyzed
	Size       int64
	DurationMs int32
	Analyzed   [][2]int32 // analysed ranges [from, to)
	Detections []Detection
	Updated    time.Time
}

// Detection is one ad found in a file.
type Detection struct {
	Ad             fingerprint.ID
	Label          string // the ad's current label ("" if the ad is gone); ignored by PutFileMap
	Type           string // the ad's current type (adtype.Ad if the ad is gone); ignored by PutFileMap
	StartMs, EndMs int32
	Score          int
	Confirmed      bool
}

// record is d as a file.map record has it (no label).
func (d Detection) record() record.Detection {
	return record.Detection{Ad: d.Ad, StartMs: d.StartMs, EndMs: d.EndMs, Score: d.Score, Confirmed: d.Confirmed}
}

// detectionOf is a detection of a file.map record.
func detectionOf(d record.Detection) Detection {
	return Detection{Ad: d.Ad, StartMs: d.StartMs, EndMs: d.EndMs, Score: d.Score, Confirmed: d.Confirmed}
}

// FileMap returns the map stored under any of keys (as its key or an alias), or nil.
func (db *DB) FileMap(ctx context.Context, keys ...string) (*FileMap, error) {
	for _, k := range keys {
		if k == "" {
			continue
		}
		var key string
		err := db.r.QueryRowContext(ctx, `SELECT key FROM file_maps WHERE key = ?
			UNION ALL SELECT key FROM file_aliases WHERE alias = ? LIMIT 1`, k, k).Scan(&key)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("looking up file %s: %w", k, err)
		}
		ms, err := db.fileMaps(ctx, key)
		if err != nil || len(ms) == 0 {
			return nil, err
		}
		return &ms[0], nil
	}
	return nil, nil
}

// FileMaps returns every file map, most recently updated first.
func (db *DB) FileMaps(ctx context.Context) ([]FileMap, error) {
	return db.fileMaps(ctx, "")
}

// fileMaps reads the map of key, or every map when key is "". The filter is chosen here,
// not in SQL: SQLite plans `? = ” OR key = ?` as a scan whatever the value.
func (db *DB) fileMaps(ctx context.Context, key string) ([]FileMap, error) {
	where := func(col string) string {
		if key == "" {
			return "1"
		}
		return col + " = :key"
	}
	//nolint:gosec // G202: only a constant condition is concatenated
	rows, err := db.r.QueryContext(ctx, `SELECT key, fp_version, size, duration_ms, analyzed, updated
		FROM file_maps WHERE `+where("key")+` ORDER BY updated DESC, key`, sql.Named("key", key))
	if err != nil {
		return nil, fmt.Errorf("reading file maps: %w", err)
	}
	var out []FileMap
	idx := map[string]int{}
	for rows.Next() {
		var m FileMap
		var analyzed string
		var updated int64
		if err := rows.Scan(&m.Key, &m.FPVersion, &m.Size, &m.DurationMs, &analyzed, &updated); err != nil {
			rows.Close()
			return nil, fmt.Errorf("reading file maps: %w", err)
		}
		if err := json.Unmarshal([]byte(analyzed), &m.Analyzed); err != nil {
			rows.Close()
			return nil, fmt.Errorf("file map %s: analyzed: %w", m.Key, err)
		}
		m.Updated = time.UnixMilli(updated)
		idx[m.Key] = len(out)
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading file maps: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	if err := db.eachRow(ctx, `SELECT key, alias FROM file_aliases WHERE `+where("key")+` ORDER BY key, alias`,
		[]any{sql.Named("key", key)}, func(rows *sql.Rows) error {
			var k, alias string
			if err := rows.Scan(&k, &alias); err != nil {
				return err
			}
			if i, ok := idx[k]; ok {
				out[i].Aliases = append(out[i].Aliases, alias)
			}
			return nil
		}); err != nil {
		return nil, fmt.Errorf("reading file aliases: %w", err)
	}
	self := db.Self()
	if err := db.eachRow(ctx, `SELECT d.key, d.ad, COALESCE(a.label_eff, ''), COALESCE(a.type_eff, 'ad'), d.start_ms, d.end_ms, d.score, d.confirmed
		FROM detections d LEFT JOIN ads a ON a.id = d.ad WHERE `+where("d.key")+` ORDER BY d.key, d.start_ms, d.ad`,
		[]any{sql.Named("self", self[:]), sql.Named("key", key)}, func(rows *sql.Rows) error {
			var k string
			var ad []byte
			var d Detection
			if err := rows.Scan(&k, &ad, &d.Label, &d.Type, &d.StartMs, &d.EndMs, &d.Score, &d.Confirmed); err != nil {
				return err
			}
			copy(d.Ad[:], ad)
			if i, ok := idx[k]; ok {
				out[i].Detections = append(out[i].Detections, d)
			}
			return nil
		}); err != nil {
		return nil, fmt.Errorf("reading detections: %w", err)
	}
	return out, nil
}

func (db *DB) eachRow(ctx context.Context, query string, args []any, fn func(*sql.Rows) error) error {
	return eachRowQ(ctx, db.r, query, args, fn)
}

func eachRowQ(ctx context.Context, q querier, query string, args []any, fn func(*sql.Rows) error) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// PutFileMap stores m under m.Key with m.Aliases, replacing whatever was stored under
// any of those keys: a map known by an alias is re-filed under its new key (for example a
// content key that became known, or a torrent key found later).
func (db *DB) PutFileMap(ctx context.Context, m *FileMap) error {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if err := putFileMap(ctx, tx, m, time.Now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// SaveFileMap is PutFileMap and then PublishFileMap, in one transaction.
func (db *DB) SaveFileMap(ctx context.Context, m *FileMap) error {
	body, digest, publish := db.fileMapBody(m)
	return db.write(ctx, func(tx *sql.Tx) error {
		if err := putFileMap(ctx, tx, m, time.Now().UnixMilli()); err != nil {
			return err
		}
		if !publish {
			return nil
		}
		return db.publishFileMap(ctx, tx, m.Key, body, digest)
	})
}

// putFileMap is PutFileMap in tx, with updated (unix ms) as the map's time.
func putFileMap(ctx context.Context, tx *sql.Tx, m *FileMap, updated int64) error {
	analyzed, err := json.Marshal(nonNil(m.Analyzed))
	if err != nil {
		return err
	}
	keys := append([]string{m.Key}, m.Aliases...)
	if same, err := filedUnder(ctx, tx, m.Key, keys); err != nil {
		return err
	} else if same {
		// The usual case (a session storing its map again): update in place rather than
		// delete the map, its aliases and detections and insert them again.
		if _, err := tx.ExecContext(ctx, `UPDATE file_maps SET fp_version = ?, size = ?, duration_ms = ?, analyzed = ?,
			analyzed_ms = ?, updated = ? WHERE key = ?`, m.FPVersion, m.Size, m.DurationMs, string(analyzed), analyzedMs(m.Analyzed),
			updated, m.Key); err != nil {
			return fmt.Errorf("storing file map %s: %w", m.Key, err)
		}
		for _, a := range m.Aliases {
			if a == m.Key {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO file_aliases (alias, key) VALUES (?, ?) ON CONFLICT DO NOTHING`,
				a, m.Key); err != nil {
				return fmt.Errorf("storing alias %s of %s: %w", a, m.Key, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM detections WHERE key = ?`, m.Key); err != nil {
			return fmt.Errorf("storing detections of %s: %w", m.Key, err)
		}
		return putDetections(ctx, tx, m)
	}
	aliases := map[string]bool{}
	for _, k := range keys {
		// Everything stored under k, as a key or an alias, is replaced; its other keys
		// become aliases of m so that no lookup is lost.
		var old string
		err := tx.QueryRowContext(ctx, `SELECT key FROM file_maps WHERE key = ?
			UNION ALL SELECT key FROM file_aliases WHERE alias = ? LIMIT 1`, k, k).Scan(&old)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		aliases[old] = true
		rows, err := tx.QueryContext(ctx, `SELECT alias FROM file_aliases WHERE key = ?`, old)
		if err != nil {
			return err
		}
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				rows.Close()
				return err
			}
			aliases[a] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM file_maps WHERE key = ?`, old); err != nil {
			return err
		}
	}
	for _, a := range m.Aliases {
		aliases[a] = true
	}
	delete(aliases, m.Key)
	if _, err := tx.ExecContext(ctx, `INSERT INTO file_maps (key, fp_version, size, duration_ms, analyzed, analyzed_ms, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, m.Key, m.FPVersion, m.Size, m.DurationMs, string(analyzed), analyzedMs(m.Analyzed), updated); err != nil {
		return fmt.Errorf("storing file map %s: %w", m.Key, err)
	}
	for a := range aliases {
		if _, err := tx.ExecContext(ctx, `INSERT INTO file_aliases (alias, key) VALUES (?, ?)`, a, m.Key); err != nil {
			return fmt.Errorf("storing alias %s of %s: %w", a, m.Key, err)
		}
	}
	return putDetections(ctx, tx, m)
}

// filedUnder reports whether key is a stored map and every one of keys is either key, an
// alias of it, or unknown: storing the map then changes no other map.
func filedUnder(ctx context.Context, tx *sql.Tx, key string, keys []string) (bool, error) {
	for i, k := range keys {
		var owner string
		err := tx.QueryRowContext(ctx, `SELECT key FROM file_maps WHERE key = ?
			UNION ALL SELECT key FROM file_aliases WHERE alias = ? LIMIT 1`, k, k).Scan(&owner)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if i == 0 {
				return false, nil
			}
		case err != nil:
			return false, err
		case owner != key:
			return false, nil
		}
	}
	return true, nil
}

// putDetections inserts the detections of m.
func putDetections(ctx context.Context, tx *sql.Tx, m *FileMap) error {
	if len(m.Detections) == 0 {
		return nil
	}
	st, err := tx.PrepareContext(ctx, `INSERT INTO detections (key, ad, start_ms, end_ms, score, confirmed)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT DO UPDATE SET end_ms = excluded.end_ms,
		score = MAX(score, excluded.score), confirmed = MAX(confirmed, excluded.confirmed)`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, d := range m.Detections {
		if _, err := st.ExecContext(ctx, m.Key, d.Ad[:], d.StartMs, d.EndMs, d.Score, d.Confirmed); err != nil {
			return fmt.Errorf("storing detections of %s: %w", m.Key, err)
		}
	}
	return nil
}

// analyzedMs is the length of the ranges iv, in milliseconds.
func analyzedMs(iv [][2]int32) int64 {
	var n int64
	for _, r := range iv {
		n += int64(r[1] - r[0])
	}
	return n
}

func nonNil(iv [][2]int32) [][2]int32 {
	if iv == nil {
		return [][2]int32{}
	}
	return iv
}

// ResetAnalyzed forgets what has been analysed in every file (after a new ad is enrolled,
// every file has to be looked at again); detections are kept.
func (db *DB) ResetAnalyzed(ctx context.Context) error {
	if _, err := db.w.ExecContext(ctx, `UPDATE file_maps SET analyzed = '[]', analyzed_ms = 0`); err != nil {
		return fmt.Errorf("resetting analysed ranges: %w", err)
	}
	return nil
}

// Meta returns a local setting.
func (db *DB) Meta(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := db.r.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", key, err)
	}
	return v, true, nil
}

// SetMeta stores a local setting.
func (db *DB) SetMeta(ctx context.Context, key, value string) error {
	if _, err := db.w.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
		return fmt.Errorf("storing %s: %w", key, err)
	}
	return nil
}
