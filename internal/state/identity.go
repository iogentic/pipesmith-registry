package state

import (
	"database/sql"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The writes of the identity commands (decision N3 of the phase 3 plan).

// Update reads every record and replaces the rows of the kinds named with
// what fn makes of them, in one transaction that raises the generation: a
// write of another process between the read and the write cannot be lost,
// and an error of fn changes nothing. The rows of kinds that are not named
// stay as they are, whatever fn does with their records.
func (st *Store) Update(kinds []Kind, fn func(snap *model.Snapshot) error) error {
	if err := checkKinds(kinds); err != nil {
		return err
	}
	return st.identityWrite(func(tx *sql.Tx) error {
		snap, err := loadAll(tx)
		if err != nil {
			return err
		}
		if err := fn(snap); err != nil {
			return err
		}
		return writeKinds(tx, kinds, snap)
	})
}

// RaiseGeneration raises the generation alone, for a change of the identity
// state that lies outside state.sqlite: the bind password of an LDAP
// connection in its secret file. A running server loads the state again and
// reads the file then.
func (st *Store) RaiseGeneration() error {
	return st.identityWrite(func(*sql.Tx) error { return nil })
}
