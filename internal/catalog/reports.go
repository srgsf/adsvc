package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Report is an ad a user saw in a file and wants to mark later. It is private to the
// user (and the admins), local to this node, and never exported: URL is the source URL
// as the client sent it.
type Report struct {
	ID      int64     `json:"id"`
	User    string    `json:"user"`
	FileKey string    `json:"fileKey,omitempty"` // the file key, when the request named one
	Norm    string    `json:"-"`                 // filekey.NormalizeURL of URL
	URL     string    `json:"url"`
	Sel     string    `json:"sel,omitempty"` // the rest of the selector: "id=..." or "ih=...&idx=..."
	PosMs   int32     `json:"posMs"`
	Note    string    `json:"note,omitempty"`
	Created time.Time `json:"created"`
}

// reportMergeMs is how close a new report may be to one of the same user in the same file
// to count as the same ad (a second press, or a report from a few seconds later).
const reportMergeMs = 10_000

// AddReport stores r (its ID and Created are set here) and returns its id. A report of
// the same user, in the same file, within 10 s of r is updated instead of adding another.
func (db *DB) AddReport(ctx context.Context, r Report) (int64, error) {
	var id int64
	err := db.write(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT id FROM reports
			WHERE user = ? AND (norm = ? OR (file_key != '' AND file_key = ?)) AND abs(pos_ms - ?) <= ?
			ORDER BY abs(pos_ms - ?) LIMIT 1`,
			r.User, r.Norm, r.FileKey, r.PosMs, reportMergeMs, r.PosMs).Scan(&id)
		switch {
		case err == nil:
			_, err = tx.ExecContext(ctx, `UPDATE reports SET file_key = ?, norm = ?, url = ?, sel = ?, pos_ms = ?,
				note = CASE WHEN ? != '' THEN ? ELSE note END, created = ? WHERE id = ?`,
				r.FileKey, r.Norm, r.URL, r.Sel, r.PosMs, r.Note, r.Note, nowMs(), id)
			return err
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO reports (user, file_key, norm, url, sel, pos_ms, note, created)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, r.User, r.FileKey, r.Norm, r.URL, r.Sel, r.PosMs, r.Note, nowMs())
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("storing a report: %w", err)
	}
	return id, nil
}

// Reports lists user's reports, or everyone's when user is "", the newest first.
func (db *DB) Reports(ctx context.Context, user string) ([]Report, error) {
	var out []Report
	err := db.eachRow(ctx, `SELECT id, user, file_key, norm, url, sel, pos_ms, note, created FROM reports
		WHERE ? = '' OR user = ? ORDER BY created DESC, id DESC`, []any{user, user},
		func(rows *sql.Rows) error {
			var r Report
			var created int64
			if err := rows.Scan(&r.ID, &r.User, &r.FileKey, &r.Norm, &r.URL, &r.Sel, &r.PosMs, &r.Note, &created); err != nil {
				return err
			}
			r.Created = time.UnixMilli(created)
			out = append(out, r)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("listing reports: %w", err)
	}
	return out, nil
}

// Report returns one report, if it exists and belongs to user (any user when "").
func (db *DB) Report(ctx context.Context, id int64, user string) (Report, bool, error) {
	var r Report
	var created int64
	err := db.r.QueryRowContext(ctx, `SELECT id, user, file_key, norm, url, sel, pos_ms, note, created FROM reports
		WHERE id = ? AND (? = '' OR user = ?)`, id, user, user).
		Scan(&r.ID, &r.User, &r.FileKey, &r.Norm, &r.URL, &r.Sel, &r.PosMs, &r.Note, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Report{}, false, nil
	}
	if err != nil {
		return Report{}, false, fmt.Errorf("reading report %d: %w", id, err)
	}
	r.Created = time.UnixMilli(created)
	return r, true, nil
}

// DeleteReport deletes a report of user (of anyone when user is ""), and reports whether
// there was one.
func (db *DB) DeleteReport(ctx context.Context, id int64, user string) (bool, error) {
	var n int64
	err := db.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM reports WHERE id = ? AND (? = '' OR user = ?)`, id, user, user)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return false, fmt.Errorf("deleting report %d: %w", id, err)
	}
	return n > 0, nil
}

// DeleteReportsIn deletes user's reports of a file (known by any of files, file keys or
// normalised URLs) with a position in [fromMs, toMs], and returns how many there were.
func (db *DB) DeleteReportsIn(ctx context.Context, user string, files []string, fromMs, toMs int32) (int, error) {
	files = nonEmpty(files)
	if len(files) == 0 {
		return 0, nil
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(files)), ",")
	args := []any{user, fromMs, toMs}
	for range 2 {
		for _, f := range files {
			args = append(args, f)
		}
	}
	var n int64
	err := db.write(ctx, func(tx *sql.Tx) error {
		//nolint:gosec // G202: only placeholders are concatenated
		res, err := tx.ExecContext(ctx, `DELETE FROM reports WHERE user = ? AND pos_ms BETWEEN ? AND ?
			AND (file_key IN (`+in+`) OR norm IN (`+in+`))`, args...)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("deleting reports: %w", err)
	}
	return int(n), nil
}

// HasReports reports whether anyone reported an ad in a file known by any of files (file
// keys or normalised URLs).
func (db *DB) HasReports(ctx context.Context, files []string) (bool, error) {
	files = nonEmpty(files)
	if len(files) == 0 {
		return false, nil
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(files)), ",")
	args := make([]any, 0, 2*len(files))
	for range 2 {
		for _, f := range files {
			args = append(args, f)
		}
	}
	var n int
	//nolint:gosec // G202: only placeholders are concatenated
	err := db.r.QueryRowContext(ctx, `SELECT count(*) FROM reports WHERE file_key IN (`+in+`) OR norm IN (`+in+`)`, args...).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("reading reports: %w", err)
	}
	return n > 0, nil
}

func nonEmpty(ss []string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
