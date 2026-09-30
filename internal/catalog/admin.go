package catalog

// What the admin page reads and changes (docs/database.md §9).

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/record"
)

// Errors of the admin writes.
var (
	ErrNoAd    = errors.New("no such ad")
	ErrManaged = errors.New("origin is managed by the config file (trust.origins)")
	ErrSelf    = errors.New("that is this node")
)

// AdRow is an ad with its standing on this node.
type AdRow struct {
	Ad
	State          string         // "" when active, else a State*
	Canonical      fingerprint.ID // the ad this node says it duplicates; zero if none
	CanonicalLabel string
	DupedBy        []AdRef   // ads this node says duplicate this one
	Up, Down       int       // votes for and against
	Files          int       // files it was found in, by any origin
	Confirmations  int       // origins that confirmed it in a file
	LastSeen       time.Time // the latest file map that has it; zero if none
}

// AdRef names an ad.
type AdRef struct {
	ID    fingerprint.ID
	Label string
}

// AdQuery selects a page of the ad list. It filters and sorts on stored columns only.
type AdQuery struct {
	Q         string // part of the label (any case), or a prefix of the id
	Author    string // a prefix of the author's origin, or part of this node's name for it
	FPVersion int    // 0: any
	Type      string // an adtype; "": any
	Pinned    *bool  // nil: either
	Sort      string // SortCreated (the default: newest first) or SortLabel
	Offset    int
	Limit     int // 0: no limit
}

// Orders of AdQuery.
const (
	SortCreated = "created"
	SortLabel   = "label"
)

// AdPage returns the ads q selects, with their standing, and how many match in all.
func (db *DB) AdPage(ctx context.Context, q AdQuery) (rows []AdRow, total int, err error) {
	self := db.Self()
	conds := []string{"1"}
	var args []any // listAds adds :self
	if q.Q != "" {
		c := `instr(a.label_fold, fold(:q)) > 0`
		args = append(args, sql.Named("q", q.Q))
		if lo, hi, ok := idRange(q.Q); ok {
			c = `(` + c + ` OR a.id BETWEEN :idlo AND :idhi)`
			args = append(args, sql.Named("idlo", lo), sql.Named("idhi", hi))
		}
		conds = append(conds, c)
	}
	if q.Author != "" {
		conds = append(conds, `(substr(hex(a.author), 1, length(:ahex)) = :ahex AND :ahex != ''
			OR EXISTS (SELECT 1 FROM origin_policy p WHERE p.origin = a.author AND instr(fold(p.name), fold(:author)) > 0))`)
		args = append(args, sql.Named("author", q.Author), sql.Named("ahex", hexOnly(strings.ToUpper(q.Author))))
	}
	if q.FPVersion != 0 {
		conds = append(conds, `a.fp_version = :fp`)
		args = append(args, sql.Named("fp", q.FPVersion))
	}
	if q.Type != "" {
		conds = append(conds, `a.type_eff = :type`)
		args = append(args, sql.Named("type", q.Type))
	}
	if q.Pinned != nil {
		c := `EXISTS (SELECT 1 FROM pins p WHERE p.ad = a.id)`
		if !*q.Pinned {
			c = "NOT " + c
		}
		conds = append(conds, c)
	}
	where := strings.Join(conds, " AND ")
	order := `a.created DESC, a.id`
	if q.Sort == SortLabel {
		order = `a.label_fold, a.id`
	}
	limit := ""
	if q.Limit > 0 {
		limit = ` LIMIT :limit OFFSET :offset`
	}
	err = db.View(ctx, func(v View) error {
		count := append([]any{sql.Named("self", self[:])}, args...)
		if err := v.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ads a WHERE `+where, count...).Scan(&total); err != nil {
			return err
		}
		pageArgs := args
		if limit != "" {
			pageArgs = append(slices.Clip(args), sql.Named("limit", q.Limit), sql.Named("offset", max(q.Offset, 0)))
		}
		ads, err := listAds(ctx, v.tx, self, where+` ORDER BY `+order+limit, pageArgs...)
		if err != nil {
			return err
		}
		if len(ads) == 0 {
			return nil
		}
		w, err := db.weigher(ctx, v.tx)
		if err != nil {
			return err
		}
		rows, err = db.adRows(ctx, v.tx, w, ads)
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// hexOnly is s if it is all hex digits, else "" (so that it matches no id).
// idRange is the range of ids that start with the hex digits prefix (a search by id
// prefix as a primary-key range, not a hex() of every id); ok is false for anything but
// 1 to 32 hex digits.
func idRange(prefix string) (lo, hi []byte, ok bool) {
	prefix = strings.ToUpper(strings.ReplaceAll(prefix, "-", ""))
	if prefix == "" || len(prefix) > 32 || hexOnly(prefix) == "" {
		return nil, nil, false
	}
	lo, _ = hex.DecodeString(prefix + strings.Repeat("0", 32-len(prefix)))
	hi, _ = hex.DecodeString(prefix + strings.Repeat("F", 32-len(prefix)))
	return lo, hi, true
}

func hexOnly(s string) string {
	for _, c := range s {
		if !strings.ContainsRune("0123456789ABCDEF", c) {
			return ""
		}
	}
	return s
}

// FPVersions lists the fingerprint versions of the stored ads.
func (db *DB) FPVersions(ctx context.Context) ([]int, error) {
	var out []int
	err := db.eachRow(ctx, `SELECT DISTINCT fp_version FROM ads ORDER BY 1`, nil, func(rows *sql.Rows) error {
		var v int
		if err := rows.Scan(&v); err != nil {
			return err
		}
		out = append(out, v)
		return nil
	})
	return out, err
}

// adIDs is ads' ids as a JSON array of hex strings, for adIn.
func adIDs(ads []Ad) string {
	ids := make([]fingerprint.ID, len(ads))
	for i, a := range ads {
		ids[i] = a.ID
	}
	return adIDsOf(ids)
}

// adIDsOf is ids as the JSON array of hex strings adIn reads.
func adIDsOf(ids []fingerprint.ID) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"%x"`, id[:])
	}
	b.WriteByte(']')
	return b.String()
}

// adIn is the condition that col is one of the ads of the named argument :ids (adIDs).
func adIn(col string) string { return col + ` IN (SELECT unhex(value) FROM json_each(:ids))` }

// adRows is the standing of ads (in their order), computed for them only.
func (db *DB) adRows(ctx context.Context, q querier, w *weigher, ads []Ad) ([]AdRow, error) {
	if len(ads) == 0 {
		return nil, nil
	}
	self := db.Self()
	st, err := db.trust(ctx, q, w, ads, true)
	if err != nil {
		return nil, err
	}
	rows := make([]AdRow, len(ads))
	idx := make(map[fingerprint.ID]int, len(ads))
	for i, a := range ads {
		x := st[a.ID]
		a.Trust, a.Pinned = x.Trust, x.Pinned
		rows[i] = AdRow{Ad: a, State: x.State}
		idx[a.ID] = i
	}
	args := []any{sql.Named("ids", adIDs(ads)), sql.Named("self", self[:])}
	// each scans rows of (ad, a, b) and calls fn for the listed ads.
	each := func(query string, fn func(r *AdRow, a, b int64)) error {
		return eachRowQ(ctx, q, query, args, func(rs *sql.Rows) error {
			var ad []byte
			var a, b int64
			if err := rs.Scan(&ad, &a, &b); err != nil {
				return err
			}
			if i, ok := idx[fingerprint.ID(ad)]; ok && len(ad) == len(fingerprint.ID{}) {
				fn(&rows[i], a, b)
			}
			return nil
		})
	}
	steps := []struct {
		query string
		fn    func(r *AdRow, a, b int64)
	}{
		{`SELECT ad, SUM(value > 0), SUM(value < 0) FROM votes WHERE ` + adIn("ad") + ` GROUP BY ad`,
			func(r *AdRow, a, b int64) { r.Up, r.Down = int(a), int(b) }},
		{`SELECT ad, COUNT(DISTINCT key), 0 FROM (SELECT ad, key FROM detections WHERE ` + adIn("ad") + ` UNION ALL
			SELECT ad, key FROM peer_detections WHERE ` + adIn("ad") + `) GROUP BY ad`,
			func(r *AdRow, a, _ int64) { r.Files = int(a) }},
		{`SELECT ad, COUNT(DISTINCT origin), 0 FROM (SELECT ad, origin FROM peer_detections WHERE confirmed = 1 AND ` + adIn("ad") + `
			UNION ALL SELECT ad, :self FROM detections WHERE confirmed = 1 AND ` + adIn("ad") + `) GROUP BY ad`,
			func(r *AdRow, a, _ int64) { r.Confirmations = int(a) }},
		{`SELECT ad, MAX(t), 0 FROM (SELECT d.ad, m.updated AS t FROM detections d JOIN file_maps m ON m.key = d.key WHERE ` + adIn("d.ad") + `
			UNION ALL SELECT d.ad, m.ts FROM peer_detections d JOIN peer_maps m ON m.key = d.key AND m.origin = d.origin
			WHERE ` + adIn("d.ad") + `) GROUP BY ad`,
			func(r *AdRow, a, _ int64) { r.LastSeen = time.UnixMilli(a) }},
	}
	for _, s := range steps {
		if err := each(s.query, s.fn); err != nil {
			return nil, err
		}
	}
	// The duplicate claims of this node from and to these ads, with the other ad's label.
	err = eachRowQ(ctx, q, `SELECT d.ad, d.canonical, c.label_eff, o.label_eff FROM dups d JOIN ads c ON c.id = d.canonical JOIN ads o ON o.id = d.ad
		WHERE d.origin = :self AND (`+adIn("d.ad")+` OR `+adIn("d.canonical")+`) ORDER BY d.ad`, args, func(rs *sql.Rows) error {
		var ad, canon []byte
		var canonLabel, adLabel string
		if err := rs.Scan(&ad, &canon, &canonLabel, &adLabel); err != nil {
			return err
		}
		if i, ok := idx[fingerprint.ID(ad)]; ok {
			copy(rows[i].Canonical[:], canon)
			rows[i].CanonicalLabel = canonLabel
		}
		if i, ok := idx[fingerprint.ID(canon)]; ok {
			rows[i].DupedBy = append(rows[i].DupedBy, AdRef{ID: fingerprint.ID(ad), Label: adLabel})
		}
		return nil
	})
	return rows, err
}

// AdDetail is everything this node knows about one ad.
type AdDetail struct {
	AdRow
	Labels    []OriginLabel  // the latest label of each origin
	Votes     []OriginVote   // the latest vote of each origin
	Dups      []DupClaim     // duplicate claims naming this ad, either way
	History   []HistoryEntry // records about the ad, newest first (at most MaxHistory)
	Sightings []Sighting     // where it was found
}

// Sighting is an origin's detection of an ad in a file; Origin is this node for its own
// maps.
type Sighting struct {
	Key    string
	Origin record.Origin
	Detection
}

// MaxHistory bounds AdDetail.History.
const MaxHistory = 200

// OriginLabel is one origin's label for an ad.
type OriginLabel struct {
	Origin record.Origin
	Label  string
	TS     int64 // ms
}

// OriginVote is one origin's vote on an ad, with the origin's weight here.
type OriginVote struct {
	Origin  record.Origin
	Value   int
	Reason  string
	Key     string // the file it is about, if any
	StartMs *int32
	TS      int64 // ms
	Weight  float64
}

// DupClaim is an origin's word that Ad duplicates Canonical.
type DupClaim struct {
	Origin        record.Origin
	Ad, Canonical fingerprint.ID
	TS            int64 // ms
}

// HistoryEntry is a record in the log, with when this node received it.
type HistoryEntry struct {
	Entry
	Received time.Time
}

// AdDetail returns what is known about ad id, or nil if there is no such ad.
func (db *DB) AdDetail(ctx context.Context, id fingerprint.ID) (*AdDetail, error) {
	var d *AdDetail
	err := db.View(ctx, func(v View) error {
		ads, err := listAds(ctx, v.tx, db.Self(), `a.id = :id`, sql.Named("id", id[:]))
		if err != nil {
			return err
		}
		if len(ads) == 0 {
			return nil
		}
		w, err := db.weigher(ctx, v.tx)
		if err != nil {
			return err
		}
		rows, err := db.adRows(ctx, v.tx, w, ads)
		if err != nil {
			return err
		}
		d = &AdDetail{AdRow: rows[0]}
		return db.adDetail(ctx, v.tx, w, d)
	})
	if err != nil {
		return nil, fmt.Errorf("reading ad %s: %w", id, err)
	}
	return d, nil
}

func (db *DB) adDetail(ctx context.Context, q querier, w *weigher, d *AdDetail) error {
	id, self := d.ID, db.Self()
	if err := eachRowQ(ctx, q, `SELECT origin, label, ts FROM ad_labels WHERE ad = ? ORDER BY ts DESC`, []any{id[:]},
		func(rs *sql.Rows) error {
			var l OriginLabel
			var o []byte
			if err := rs.Scan(&o, &l.Label, &l.TS); err != nil {
				return err
			}
			copy(l.Origin[:], o)
			d.Labels = append(d.Labels, l)
			return nil
		}); err != nil {
		return err
	}
	if err := eachRowQ(ctx, q, `SELECT origin, value, reason, file_key, start_ms, ts FROM votes WHERE ad = ? ORDER BY ts DESC`,
		[]any{id[:]}, func(rs *sql.Rows) error {
			var v OriginVote
			var o []byte
			var key sql.NullString
			var start sql.NullInt32
			if err := rs.Scan(&o, &v.Value, &v.Reason, &key, &start, &v.TS); err != nil {
				return err
			}
			copy(v.Origin[:], o)
			v.Key, v.Weight = key.String, w.of(v.Origin)
			if start.Valid {
				v.StartMs = &start.Int32
			}
			d.Votes = append(d.Votes, v)
			return nil
		}); err != nil {
		return err
	}
	if err := eachRowQ(ctx, q, `SELECT origin, ad, canonical, ts FROM dups WHERE ad = ?1 OR canonical = ?1 ORDER BY ts DESC`,
		[]any{id[:]}, func(rs *sql.Rows) error {
			var c DupClaim
			var o, ad, canon []byte
			if err := rs.Scan(&o, &ad, &canon, &c.TS); err != nil {
				return err
			}
			copy(c.Origin[:], o)
			copy(c.Ad[:], ad)
			copy(c.Canonical[:], canon)
			d.Dups = append(d.Dups, c)
			return nil
		}); err != nil {
		return err
	}
	if err := eachRowQ(ctx, q, `SELECT r.id, r.kind, r.origin, r.seq, r.ts, r.body, r.sig, r.via, r.received
		FROM ad_records ar JOIN records r ON r.id = ar.record WHERE ar.ad = ? ORDER BY r.id DESC LIMIT ?`,
		[]any{id[:], MaxHistory}, func(rs *sql.Rows) error {
			var h HistoryEntry
			var o, sig, via []byte
			var received int64
			if err := rs.Scan(&h.ID, &h.Kind, &o, &h.Seq, &h.TS, &h.Body, &sig, &via, &received); err != nil {
				return err
			}
			copy(h.Origin[:], o)
			copy(h.Sig[:], sig)
			copy(h.Via[:], via)
			h.Received = time.UnixMilli(received)
			d.History = append(d.History, h)
			return nil
		}); err != nil {
		return err
	}
	return eachRowQ(ctx, q, `SELECT key, ?1, start_ms, end_ms, score, confirmed FROM detections WHERE ad = ?2
		UNION ALL SELECT key, origin, start_ms, end_ms, score, confirmed FROM peer_detections WHERE ad = ?2
		ORDER BY 1, 3`, []any{self[:], id[:]}, func(rs *sql.Rows) error {
		var s Sighting
		var o []byte
		if err := rs.Scan(&s.Key, &o, &s.StartMs, &s.EndMs, &s.Score, &s.Confirmed); err != nil {
			return err
		}
		copy(s.Origin[:], o)
		s.Ad, s.Label = id, d.Label
		d.Sightings = append(d.Sightings, s)
		return nil
	})
}

// Dup says, as this node, that ad id duplicates canonical: id leaves the tracking set
// and canonical stands for it. It is published with the votes (Share.Votes).
func (db *DB) Dup(ctx context.Context, id, canonical fingerprint.ID) (err error) {
	defer wrap(&err, "marking ad %s a duplicate", id)
	if id == canonical {
		return errors.New("an ad cannot duplicate itself")
	}
	return db.write(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ads WHERE id IN (?, ?)`, id[:], canonical[:]).Scan(&n); err != nil {
			return err
		}
		if n != 2 {
			return ErrNoAd
		}
		body := record.AdDup{Ad: id, Canonical: canonical}
		if db.shares().Votes {
			_, err := db.publish(ctx, tx, record.KindAdDup, body, 0)
			return err
		}
		self := db.Self()
		_, err := tx.ExecContext(ctx, `INSERT INTO dups (ad, origin, canonical, ts, seq) VALUES (?, ?, ?, ?, 0)
			ON CONFLICT (ad, origin) DO UPDATE SET canonical = excluded.canonical, ts = excluded.ts, seq = excluded.seq`,
			id[:], self[:], canonical[:], db.Clock().Next())
		return err
	})
}

// Labels returns the labels of ads ids (those that exist).
func (db *DB) Labels(ctx context.Context, ids []fingerprint.ID) (map[fingerprint.ID]string, error) {
	out := make(map[fingerprint.ID]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	err := db.eachRow(ctx, `SELECT a.id, a.label_eff FROM ads a WHERE `+adIn("a.id"), []any{sql.Named("ids", adIDsOf(ids))},
		func(rs *sql.Rows) error {
			var id []byte
			var l string
			if err := rs.Scan(&id, &l); err != nil {
				return err
			}
			out[fingerprint.ID(id)] = l
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("reading labels: %w", err)
	}
	return out, nil
}

// OriginRow is an origin: this node's opinion of it and what it produced.
type OriginRow struct {
	Origin       record.Origin
	Name         string
	Weight       float64  // in effect
	Own          *float64 // its own weight in origin_policy; nil = the default
	Blocked      bool
	Managed      bool // set by the config file: read-only here
	Self         bool
	Records      int
	LastReceived time.Time // zero if no record
	Ads          int       // ads it authored
	Trashed      int       // of those, voted not_ad or boundary by a trusted origin
	Dups         int       // of those, called a duplicate by a trusted origin
	Retracted    int       // of those, withdrawn by itself
	Votes        int       // votes it cast
}

// Origins lists this node, the origins of every stored record, and those with a policy:
// this node first, then by records.
//
// A trusted origin is one whose weight alone reaches the policy's MinTrust.
func (db *DB) Origins(ctx context.Context) ([]OriginRow, error) {
	var out []OriginRow
	err := db.View(ctx, func(v View) error {
		var err error
		out, err = db.origins(ctx, v.tx)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("listing origins: %w", err)
	}
	return out, nil
}

func (db *DB) origins(ctx context.Context, q querier) ([]OriginRow, error) {
	w, err := db.weigher(ctx, q)
	if err != nil {
		return nil, err
	}
	rows := map[record.Origin]*OriginRow{}
	row := func(o record.Origin) *OriginRow {
		r := rows[o]
		if r == nil {
			r = &OriginRow{Origin: o}
			rows[o] = r
		}
		return r
	}
	if self := db.Self(); self != (record.Origin{}) {
		row(self)
	}
	if err := eachRowQ(ctx, q, `SELECT origin, name, weight, blocked, managed FROM origin_policy`, nil, func(rs *sql.Rows) error {
		var o []byte
		var name string
		var weight sql.NullFloat64
		var blocked, managed bool
		if err := rs.Scan(&o, &name, &weight, &blocked, &managed); err != nil {
			return err
		}
		r := row(record.Origin(o))
		r.Name, r.Blocked, r.Managed = name, blocked, managed
		if weight.Valid {
			r.Own = &weight.Float64
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := eachRowQ(ctx, q, `SELECT origin, COUNT(*), MAX(received) FROM records GROUP BY origin`, nil, func(rs *sql.Rows) error {
		var o []byte
		var n int
		var last int64
		if err := rs.Scan(&o, &n, &last); err != nil {
			return err
		}
		r := row(record.Origin(o))
		r.Records, r.LastReceived = n, time.UnixMilli(last)
		return nil
	}); err != nil {
		return nil, err
	}
	if err := eachRowQ(ctx, q, `SELECT origin, COUNT(*) FROM votes GROUP BY origin`, nil, func(rs *sql.Rows) error {
		var o []byte
		var n int
		if err := rs.Scan(&o, &n); err != nil {
			return err
		}
		row(record.Origin(o)).Votes = n
		return nil
	}); err != nil {
		return nil, err
	}
	trash, dups, err := db.trash(ctx, q, w)
	if err != nil {
		return nil, err
	}
	retracted := map[fingerprint.ID]bool{}
	if err := eachRowQ(ctx, q, `SELECT ad FROM retractions JOIN ads a ON a.id = ad AND a.author = origin WHERE `+retractionLive, nil,
		func(rs *sql.Rows) error {
			var ad []byte
			if err := rs.Scan(&ad); err != nil {
				return err
			}
			retracted[fingerprint.ID(ad)] = true
			return nil
		}); err != nil {
		return nil, err
	}
	if err := eachRowQ(ctx, q, `SELECT id, author FROM ads WHERE author IS NOT NULL`, nil, func(rs *sql.Rows) error {
		var ad, o []byte
		if err := rs.Scan(&ad, &o); err != nil {
			return err
		}
		id := fingerprint.ID(ad)
		r := row(record.Origin(o))
		r.Ads++
		r.Trashed += b2i(trash[id])
		r.Dups += b2i(dups[id])
		r.Retracted += b2i(retracted[id])
		return nil
	}); err != nil {
		return nil, err
	}
	self := db.Self()
	out := make([]OriginRow, 0, len(rows))
	for _, r := range rows {
		r.Self = r.Origin == self
		r.Weight = w.of(r.Origin)
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b OriginRow) int {
		switch {
		case a.Self != b.Self:
			if a.Self {
				return -1
			}
			return 1
		case a.Records != b.Records:
			return cmp.Compare(b.Records, a.Records)
		}
		return strings.Compare(a.Origin.String(), b.Origin.String())
	})
	return out, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// trash returns the ads a trusted origin voted not_ad or boundary, and those a trusted
// origin called duplicates.
func (db *DB) trash(ctx context.Context, q querier, w *weigher) (trash, dups map[fingerprint.ID]bool, err error) {
	trash, dups = map[fingerprint.ID]bool{}, map[fingerprint.ID]bool{}
	scan := func(query string, into map[fingerprint.ID]bool) error {
		return eachRowQ(ctx, q, query, nil, func(rs *sql.Rows) error {
			var ad, o []byte
			if err := rs.Scan(&ad, &o); err != nil {
				return err
			}
			if w.of(record.Origin(o)) >= w.pol.MinTrust {
				into[fingerprint.ID(ad)] = true
			}
			return nil
		})
	}
	if err := scan(`SELECT ad, origin FROM votes WHERE value < 0 AND reason IN ('not_ad', 'boundary')`, trash); err != nil {
		return nil, nil, err
	}
	return trash, dups, scan(`SELECT ad, origin FROM dups`, dups)
}

// SetOriginPolicy sets this node's opinion of an origin (from the admin page). Origins
// managed by the config file cannot be changed here, nor can this node itself.
func (db *DB) SetOriginPolicy(ctx context.Context, r OriginRule) error {
	if r.Origin == db.Self() {
		return ErrSelf
	}
	if r.Weight != nil && *r.Weight < 0 {
		return errors.New("negative weight")
	}
	var w sql.NullFloat64
	if r.Weight != nil {
		w = sql.NullFloat64{Float64: *r.Weight, Valid: true}
	}
	return db.write(ctx, func(tx *sql.Tx) error {
		if err := notManaged(ctx, tx, r.Origin); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO origin_policy (origin, name, weight, blocked, managed) VALUES (?, ?, ?, ?, 0)
			ON CONFLICT (origin) DO UPDATE SET name = excluded.name, weight = excluded.weight, blocked = excluded.blocked`,
			r.Origin[:], r.Name, w, r.Blocked)
		return err
	})
}

// ClearOriginPolicy forgets this node's opinion of an origin: it gets the defaults again.
func (db *DB) ClearOriginPolicy(ctx context.Context, o record.Origin) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		if err := notManaged(ctx, tx, o); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM origin_policy WHERE origin = ?`, o[:])
		return err
	})
}

func notManaged(ctx context.Context, tx *sql.Tx, o record.Origin) error {
	var managed bool
	err := tx.QueryRowContext(ctx, `SELECT managed FROM origin_policy WHERE origin = ?`, o[:]).Scan(&managed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	case managed:
		return ErrManaged
	}
	return nil
}

// FileRow is a file this node or another origin has a map of.
type FileRow struct {
	Key        string
	Own        bool      // this node has a map of it
	Updated    time.Time // the latest map
	DurationMs int32
	AnalyzedMs int32 // by this node
	Ads        int   // this node's detections
	Origins    int   // other origins with a map of it
	PeerAds    int   // their detections
}

// Files lists the files with a map whose key contains search (all when ""), the most
// recently updated first: limit of them (all when limit <= 0) after the first offset, and
// how many there are in all.
func (db *DB) Files(ctx context.Context, search string, offset, limit int) (rows []FileRow, total int, err error) {
	// One row per map (this node's own, or a peer's), grouped by file.
	const maps = `SELECT m.key, 1 AS own, m.updated AS t, m.duration_ms AS dur,
			m.analyzed_ms AS analyzed,
			(SELECT COUNT(*) FROM detections d WHERE d.key = m.key) AS ads, NULL AS origin, 0 AS peer_ads
		FROM file_maps m WHERE instr(m.key, :q) > 0
		UNION ALL
		SELECT m.key, 0, m.ts, m.duration_ms, 0, 0, m.origin,
			(SELECT COUNT(*) FROM peer_detections d WHERE d.key = m.key AND d.origin = m.origin)
		FROM peer_maps m WHERE instr(m.key, :q) > 0`
	args := []any{sql.Named("q", search), sql.Named("limit", cmp.Or(max(limit, 0), -1)), sql.Named("offset", max(offset, 0))}
	err = db.View(ctx, func(v View) error {
		if err := v.tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT key) FROM (`+maps+`)`, args...).Scan(&total); err != nil {
			return err
		}
		return eachRowQ(ctx, v.tx, `SELECT key, MAX(own), MAX(t),
			COALESCE(NULLIF(MAX(CASE WHEN own = 1 THEN dur END), 0), MAX(dur)),
			SUM(analyzed), SUM(ads), COUNT(DISTINCT origin), SUM(peer_ads)
			FROM (`+maps+`) GROUP BY key ORDER BY MAX(t) DESC, key LIMIT :limit OFFSET :offset`, args, func(rs *sql.Rows) error {
			var r FileRow
			var updated int64
			if err := rs.Scan(&r.Key, &r.Own, &updated, &r.DurationMs, &r.AnalyzedMs, &r.Ads, &r.Origins, &r.PeerAds); err != nil {
				return err
			}
			r.Updated = time.UnixMilli(updated)
			rows = append(rows, r)
			return nil
		})
	})
	if err != nil {
		return nil, 0, fmt.Errorf("listing files: %w", err)
	}
	return rows, total, nil
}

// OriginNames returns the names this node knows origins by: its own name for them
// (origin_policy), else the name of the configured peer that is that node.
func (db *DB) OriginNames(ctx context.Context) (map[record.Origin]string, error) {
	names := map[record.Origin]string{}
	err := db.eachRow(ctx, `SELECT node_id, name, 0 FROM peers WHERE node_id IS NOT NULL
		UNION ALL SELECT origin, name, 1 FROM origin_policy WHERE name != '' ORDER BY 3`, nil, func(rs *sql.Rows) error {
		var o []byte
		var name string
		var prio int
		if err := rs.Scan(&o, &name, &prio); err != nil {
			return err
		}
		names[record.Origin(o)] = name
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading origin names: %w", err)
	}
	return names, nil
}

// FindAds returns the ads whose id starts with the hex digits prefix (dashes ignored),
// at most limit of them.
func (db *DB) FindAds(ctx context.Context, prefix string, limit int) ([]fingerprint.ID, error) {
	lo, hi, ok := idRange(prefix)
	if !ok {
		return nil, fmt.Errorf("ad id %q: want hex digits", prefix)
	}
	var out []fingerprint.ID
	err := db.eachRow(ctx, `SELECT id FROM ads WHERE id BETWEEN ? AND ? ORDER BY id LIMIT ?`,
		[]any{lo, hi, limit}, func(rs *sql.Rows) error {
			var b []byte
			if err := rs.Scan(&b); err != nil {
				return err
			}
			out = append(out, fingerprint.ID(b))
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("finding ads: %w", err)
	}
	return out, nil
}
