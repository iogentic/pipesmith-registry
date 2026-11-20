package state

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The capabilities of step 5, the properties as a JSON object of strings.
// A property whose key holds a secret word is no part of a record (A11).
// Every change goes through Update, so the generation rises.

func writeCapabilities(tx *sql.Tx, snap *model.Snapshot) error {
	if err := exec(tx, "DELETE FROM capabilities"); err != nil {
		return err
	}
	for _, c := range snap.Capabilities {
		if c.ID == "" {
			return errors.New("a capability has no id")
		}
		if err := checkOrigin(fmt.Sprintf("capability %q", c.ID), c.Origin); err != nil {
			return err
		}
		properties, err := textMap(c.Properties)
		if err != nil {
			return fmt.Errorf("properties of capability %q: %w", c.ID, err)
		}
		if err := exec(tx, "INSERT INTO capabilities VALUES (?, ?, ?, ?, ?, ?)", c.ID, c.TypeID, flag(c.Enabled), c.Notes,
			properties, c.Origin); err != nil {
			return fmt.Errorf("capability %q: %w", c.ID, err)
		}
	}
	return nil
}

func loadCapabilities(tx *sql.Tx, snap *model.Snapshot) error {
	snap.Capabilities = []model.Capability{}
	return each(tx, "SELECT id, type_id, enabled, notes, properties, origin FROM capabilities ORDER BY id",
		func(rows *sql.Rows) error {
			var c model.Capability
			var enabled int
			var properties string
			if err := rows.Scan(&c.ID, &c.TypeID, &enabled, &c.Notes, &properties, &c.Origin); err != nil {
				return err
			}
			c.Enabled = enabled != 0
			var err error
			if c.Properties, err = stringMap(properties); err != nil {
				return fmt.Errorf("properties of capability %q: %w", c.ID, err)
			}
			snap.Capabilities = append(snap.Capabilities, c)
			return nil
		})
}

// Capabilities are the capabilities by id. The list is never nil.
func (st *Store) Capabilities() ([]model.Capability, error) {
	snap, err := st.readOne(loadCapabilities)
	if err != nil {
		return nil, err
	}
	return snap.Capabilities, nil
}

// PutCapability adds c, or replaces the capability of its id, and raises
// the generation.
func (st *Store) PutCapability(c model.Capability) error {
	_, err := st.change(KindCapabilities, func(snap *model.Snapshot) (bool, error) {
		snap.Capabilities = slices.DeleteFunc(snap.Capabilities, func(x model.Capability) bool { return x.ID == c.ID })
		snap.Capabilities = append(snap.Capabilities, c)
		slices.SortFunc(snap.Capabilities, func(a, b model.Capability) int { return strings.Compare(a.ID, b.ID) })
		return true, nil
	})
	return err
}

// RemoveCapability removes the capability of the id and raises the
// generation. false, and no write, when there is none.
func (st *Store) RemoveCapability(id string) (bool, error) {
	return st.change(KindCapabilities, func(snap *model.Snapshot) (bool, error) {
		n := len(snap.Capabilities)
		snap.Capabilities = slices.DeleteFunc(snap.Capabilities, func(x model.Capability) bool { return x.ID == id })
		return len(snap.Capabilities) != n, nil
	})
}
