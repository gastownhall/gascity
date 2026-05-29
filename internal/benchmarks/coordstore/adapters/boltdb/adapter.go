// Package boltdb provides a bbolt-backed StoreAdapter for the coordination
// store benchmark sweep.
package boltdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/benchmarks/coordstore"
	"github.com/gastownhall/gascity/internal/benchmarks/coordstore/adapters/internal/memstore"
	bbolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"
)

var (
	bucketRecords   = []byte("records")
	bucketEphemeral = []byte("ephemeral")
	bucketDeps      = []byte("deps")
)

// Adapter stores records in bbolt and serves reads through an in-process hot
// index loaded from the database at Open time.
type Adapter struct {
	*memstore.Adapter
	db *bbolt.DB
}

// New returns an uninitialized bbolt adapter.
func New() *Adapter {
	a := &Adapter{}
	a.Adapter = memstore.New("bb", a)
	return a
}

// Open initializes the bbolt database and loads its current contents.
func (a *Adapter) Open(ctx context.Context, cfg coordstore.Config) error {
	path := filepath.Join(cfg.DataDir, "store.bolt")
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return fmt.Errorf("bbolt: open %s: %w", path, err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bucketRecords, bucketEphemeral, bucketDeps} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		db.Close() //nolint:errcheck
		return fmt.Errorf("bbolt: create buckets: %w", err)
	}
	a.db = db

	records, deps, err := a.load(ctx)
	if err != nil {
		db.Close() //nolint:errcheck
		a.db = nil
		return err
	}
	a.ReplaceState(records, deps)
	return nil
}

// Close releases the bbolt handle.
func (a *Adapter) Close() error {
	if a.db == nil {
		return nil
	}
	err := a.db.Close()
	a.db = nil
	return err
}

// SaveRecord writes a complete record atomically.
func (a *Adapter) SaveRecord(_ context.Context, r coordstore.Record) error {
	db := a.db
	if db == nil {
		return fmt.Errorf("bbolt: database is not open")
	}
	return db.Update(func(tx *bbolt.Tx) error {
		data, err := json.Marshal(r)
		if err != nil {
			return err
		}
		dst := bucketRecords
		other := bucketEphemeral
		if r.Ephemeral {
			dst = bucketEphemeral
			other = bucketRecords
		}
		if err := tx.Bucket(other).Delete([]byte(r.ID)); err != nil {
			return err
		}
		return tx.Bucket(dst).Put([]byte(r.ID), data)
	})
}

// DeleteRecord removes a record from bbolt.
func (a *Adapter) DeleteRecord(_ context.Context, id string, _ bool) error {
	db := a.db
	if db == nil {
		return fmt.Errorf("bbolt: database is not open")
	}
	return db.Update(func(tx *bbolt.Tx) error {
		if err := tx.Bucket(bucketRecords).Delete([]byte(id)); err != nil {
			return err
		}
		return tx.Bucket(bucketEphemeral).Delete([]byte(id))
	})
}

// SaveDep writes a dependency edge.
func (a *Adapter) SaveDep(_ context.Context, d coordstore.Dep) error {
	db := a.db
	if db == nil {
		return fmt.Errorf("bbolt: database is not open")
	}
	return db.Update(func(tx *bbolt.Tx) error {
		data, err := json.Marshal(d)
		if err != nil {
			return err
		}
		return tx.Bucket(bucketDeps).Put([]byte(depKey(d.FromID, d.ToID)), data)
	})
}

// DeleteDep removes a dependency edge.
func (a *Adapter) DeleteDep(_ context.Context, fromID, toID string) error {
	db := a.db
	if db == nil {
		return fmt.Errorf("bbolt: database is not open")
	}
	return db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketDeps).Delete([]byte(depKey(fromID, toID)))
	})
}

// ResetBacking wipes all buckets.
func (a *Adapter) ResetBacking(context.Context) error {
	db := a.db
	if db == nil {
		return fmt.Errorf("bbolt: database is not open")
	}
	return db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bucketRecords, bucketEphemeral, bucketDeps} {
			if err := tx.DeleteBucket(name); err != nil && !errors.Is(err, berrors.ErrBucketNotFound) {
				return err
			}
			if _, err := tx.CreateBucket(name); err != nil {
				return err
			}
		}
		return nil
	})
}

func (a *Adapter) load(context.Context) ([]coordstore.Record, []coordstore.Dep, error) {
	var records []coordstore.Record
	var deps []coordstore.Dep
	err := a.db.View(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bucketRecords, bucketEphemeral} {
			b := tx.Bucket(name)
			if b == nil {
				continue
			}
			if err := b.ForEach(func(_, value []byte) error {
				var r coordstore.Record
				if err := json.Unmarshal(value, &r); err != nil {
					return err
				}
				records = append(records, r)
				return nil
			}); err != nil {
				return err
			}
		}
		b := tx.Bucket(bucketDeps)
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, value []byte) error {
			var d coordstore.Dep
			if err := json.Unmarshal(value, &d); err != nil {
				return err
			}
			deps = append(deps, d)
			return nil
		})
	})
	if err != nil {
		return nil, nil, fmt.Errorf("bbolt: load: %w", err)
	}
	return records, deps, nil
}

func depKey(fromID, toID string) string { return fromID + "\x00" + toID }
