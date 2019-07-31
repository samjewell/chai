package genji

import (
	"math/rand"
	"time"

	"github.com/asdine/genji/engine"
	"github.com/asdine/genji/field"
	"github.com/asdine/genji/record"
	"github.com/asdine/genji/table"
	"github.com/oklog/ulid/v2"
	"github.com/pkg/errors"
)

var (
	seed    = time.Now().UnixNano()
	entropy = rand.New(rand.NewSource(seed))
	ulidTs  = ulid.Timestamp(time.Now())
)

// DB represents a collection of tables stored in the underlying engine.
// DB differs from the engine in that it provides automatic indexing
// and database administration methods.
// DB is safe for concurrent use unless the given engine isn't.
type DB struct {
	engine.Engine
}

// New initializes the DB using the given engine.
func New(ng engine.Engine) *DB {
	return &DB{
		Engine: ng,
	}
}

// Begin starts a new transaction.
// The returned transaction must be closed either by calling Rollback or Commit.
func (db DB) Begin(writable bool) (*Tx, error) {
	tx, err := db.Engine.Begin(writable)
	if err != nil {
		return nil, err
	}

	return &Tx{
		Transaction: tx,
	}, nil
}

// View starts a read only transaction, runs fn and automatically rolls it back.
func (db DB) View(fn func(tx *Tx) error) error {
	tx, err := db.Begin(false)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	return fn(tx)
}

// Update starts a read-write transaction, runs fn and automatically commits it.
func (db DB) Update(fn func(tx *Tx) error) error {
	tx, err := db.Begin(true)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	err = fn(tx)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// ViewTable starts a read only transaction, fetches the selected table, calls fn with that table
// and automatically rolls back the transaction.
func (db DB) ViewTable(tableName string, fn func(*Table) error) error {
	return db.View(func(tx *Tx) error {
		tb, err := tx.Table(tableName)
		if err != nil {
			return err
		}

		return fn(tb)
	})
}

// UpdateTable starts a read/write transaction, fetches the selected table, calls fn with that table
// and automatically commits the transaction.
// If fn returns an error, the transaction is rolled back.
func (db DB) UpdateTable(tableName string, fn func(*Table) error) error {
	return db.Update(func(tx *Tx) error {
		tb, err := tx.Table(tableName)
		if err != nil {
			return err
		}

		return fn(tb)
	})
}

// Tx represents a database transaction. It provides methods for managing the
// collection of tables and the transaction itself.
// Tx is either read-only or read/write. Read-only can be used to read tables
// and read/write can be used to read, create, delete and modify tables.
type Tx struct {
	engine.Transaction
}

// CreateTableIfNotExists calls CreateTable and returns no error if it already exists.
func (tx Tx) CreateTableIfNotExists(name string) error {
	err := tx.CreateTable(name)
	if err == nil || err == engine.ErrTableAlreadyExists {
		return nil
	}
	return err
}

// CreateIndexIfNotExists calls CreateIndex and returns no error if it already exists.
func (tx Tx) CreateIndexIfNotExists(table string, field string) error {
	err := tx.CreateIndex(table, field)
	if err == nil || err == engine.ErrIndexAlreadyExists {
		return nil
	}
	return err
}

// Table returns a table by name. The table instance is only valid for the lifetime of the transaction.
func (tx Tx) Table(name string) (*Table, error) {
	tb, err := tx.Transaction.Table(name, record.NewCodec())
	if err != nil {
		return nil, err
	}

	return &Table{
		Table: tb,
		tx:    tx.Transaction,
		name:  name,
	}, nil
}

// A Table represents a collection of records.
type Table struct {
	tx    engine.Transaction
	store engine.Store
	name  string
}

// Insert the record into the table.
// Indexes are automatically updated.
func (t Table) Insert(r record.Record) ([]byte, error) {
	v, err := record.Encode(r)
	if err != nil {
		return nil, errors.Wrap(err, "failed to encode record")
	}

	var recordID []byte
	if pker, ok := r.(table.Pker); ok {
		recordID, err = pker.Pk()
		if err != nil {
			return nil, errors.Wrap(err, "failed to generate recordID from Pk method")
		}
	} else {
		id, err := ulid.New(ulidTs, entropy)
		if err == nil {
			recordID, err = id.MarshalText()
		}
		if err != nil {
			return nil, errors.Wrap(err, "failed to generate recordID")
		}
	}

	err = t.store.Put(recordID, v)
	if err != nil {
		return nil, err
	}

	indexes, err := t.tx.Indexes(t.name)
	if err != nil {
		return nil, err
	}

	for fieldName, idx := range indexes {
		f, err := r.Field(fieldName)
		if err != nil {
			return nil, err
		}

		err = idx.Set(f.Data, recordID)
		if err != nil {
			return nil, err
		}
	}

	return recordID, nil
}

// Delete a record by recordID.
// Indexes are automatically updated.
func (t Table) Delete(recordID []byte) error {
	err := t.Table.Delete(recordID)
	if err != nil {
		return err
	}

	indexes, err := t.tx.Indexes(t.name)
	if err != nil {
		return err
	}

	for _, idx := range indexes {
		err = idx.Delete(recordID)
		if err != nil {
			return err
		}
	}

	return nil
}

// Replace a record by recordID.
// An error is returned if the recordID doesn't exist.
// Indexes are automatically updated.
func (t Table) Replace(recordID []byte, r record.Record) error {
	err := t.Table.Replace(recordID, r)
	if err != nil {
		return err
	}

	indexes, err := t.tx.Indexes(t.name)
	if err != nil {
		return err
	}

	for fieldName, idx := range indexes {
		f, err := r.Field(fieldName)
		if err != nil {
			return err
		}

		err = idx.Set(f.Data, recordID)
		if err != nil {
			return err
		}
	}

	return nil
}

// AddField changes the table structure by adding a field to all the records.
// If the field data is empty, it is filled with the zero value of the field type.
// If a record already has the field, no change is performed on that record.
func (t Table) AddField(f field.Field) error {
	return t.Table.Iterate(func(recordID []byte, r record.Record) error {
		var fb record.FieldBuffer
		err := fb.ScanRecord(r)
		if err != nil {
			return err
		}

		if _, err = fb.Field(f.Name); err == nil {
			// if the field already exists, skip
			return nil
		}

		if f.Data == nil {
			f.Data = field.ZeroValue(f.Type).Data
		}
		fb.Add(f)
		return t.Table.Replace(recordID, fb)
	})
}

// DeleteField changes the table structure by deleting a field from all the records.
func (t Table) DeleteField(name string) error {
	return t.Table.Iterate(func(recordID []byte, r record.Record) error {
		var fb record.FieldBuffer
		err := fb.ScanRecord(r)
		if err != nil {
			return err
		}

		err = fb.Delete(name)
		if err != nil {
			// if the field doesn't exist, skip
			return nil
		}

		return t.Table.Replace(recordID, fb)
	})
}

// RenameField changes the table structure by renaming the selected field on all the records.
func (t Table) RenameField(oldName, newName string) error {
	return t.Table.Iterate(func(recordID []byte, r record.Record) error {
		var fb record.FieldBuffer
		err := fb.ScanRecord(r)
		if err != nil {
			return err
		}

		f, err := fb.Field(oldName)
		if err != nil {
			// if the field doesn't exist, skip
			return nil
		}

		f.Name = newName
		fb.Replace(oldName, f)
		return t.Table.Replace(recordID, fb)
	})
}

// ReIndex drops the selected index, creates a new one and runs over all the records
// to fill the newly created index.
func (t Table) ReIndex(fieldName string) error {
	err := t.tx.DropIndex(t.name, fieldName)
	if err != nil {
		return err
	}

	err = t.tx.CreateIndex(t.name, fieldName)
	if err != nil {
		return err
	}

	idx, err := t.tx.Index(t.name, fieldName)
	if err != nil {
		return err
	}

	return t.Iterate(func(recordID []byte, r record.Record) error {
		f, err := r.Field(fieldName)
		if err != nil {
			return err
		}

		return idx.Set(f.Data, recordID)
	})
}

// SelectTable returns the current table. Implements the query.TableSelector interface.
func (t Table) SelectTable(*Tx) (*Table, error) {
	return &t, nil
}

// TableName returns the name of the table.
func (t Table) TableName() string {
	return t.name
}

// A TableNamer is a type that returns the name of a table.
type TableNamer interface {
	TableName() string
}

type indexer interface {
	Indexes() []string
}

// InitTable ensures the table exists. If tn implements this interface
//   type indexer interface {
//	  Indexes() []string
//   }
// it ensures all these indexes exist and creates them if not, but it won't re-index the table.
func InitTable(tx *Tx, tn TableNamer) error {
	name := tn.TableName()

	err := tx.CreateTableIfNotExists(name)
	if err != nil {
		return err
	}

	idxr, ok := tn.(indexer)
	if ok {
		for _, idx := range idxr.Indexes() {
			err = tx.CreateIndexIfNotExists(name, idx)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
