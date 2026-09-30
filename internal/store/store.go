// Package store opens what adsvc keeps in its data directory: the catalogue
// (catalogue.db), the ad index over it (tracking.csr), the per-file ad maps, and this
// node's identity (identity.key: its signing key, which never enters the catalogue).
package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/srgsf/adsvc/internal/admap"
	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/library"
	"github.com/srgsf/adsvc/internal/record"
)

// Store is an open data directory.
type Store struct {
	DB   *catalog.DB
	Lib  *library.Library
	Maps *admap.Store
}

// Open opens the data directory dir, creating it if needed; "" keeps everything in
// memory, with a new identity.
func Open(ctx context.Context, dir string) (*Store, error) {
	dbPath, csrPath := "", ""
	id := record.NewIdentity()
	if dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, err
		}
		dbPath, csrPath = filepath.Join(dir, "catalogue.db"), filepath.Join(dir, "tracking.csr")
		var err error
		if id, err = record.LoadIdentity(filepath.Join(dir, "identity.key")); err != nil {
			return nil, err
		}
	}
	db, err := catalog.Open(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	if err := db.SetIdentity(ctx, id); err != nil {
		db.Close()
		return nil, err
	}
	lib, err := library.Open(ctx, db, csrPath)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db, Lib: lib, Maps: admap.New(db)}, nil
}

// Close releases the index and closes the catalogue.
func (s *Store) Close() error { return errors.Join(s.Lib.Close(), s.DB.Close()) }

// Copy is an in-memory store holding a copy of the ads of from (and no file maps): what a
// benchmark run needs so that nothing it stores is seen by another run.
func Copy(ctx context.Context, from *catalog.DB) (*Store, error) {
	db, err := catalog.Open(ctx, "")
	if err != nil {
		return nil, err
	}
	ads, err := from.Ads(ctx, fingerprint.Version)
	if err == nil {
		byID := make(map[fingerprint.ID]catalog.Ad, len(ads))
		for _, a := range ads {
			byID[a.ID] = a
		}
		err = from.EachPoints(ctx, fingerprint.Version, func(id fingerprint.ID, pts []byte) error {
			_, err := db.PutAd(ctx, byID[id], pts)
			return err
		})
	}
	var lib *library.Library
	if err == nil {
		lib, err = library.Open(ctx, db, "")
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db, Lib: lib, Maps: admap.New(db)}, nil
}
