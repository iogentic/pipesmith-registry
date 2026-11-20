package state

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The cleanup policies of step 5. A policy keeps every criterion as stored,
// known to this server or not; admin.Runnable decides whether it may run
// (A9). Every change goes through Update, so the generation rises.

func writeCleanupPolicies(tx *sql.Tx, snap *model.Snapshot) error {
	if err := exec(tx, "DELETE FROM cleanup_policies"); err != nil {
		return err
	}
	for _, p := range snap.CleanupPolicies {
		if p.Name == "" {
			return errors.New("a cleanup policy has no name")
		}
		if err := checkOrigin(fmt.Sprintf("cleanup policy %q", p.Name), p.Origin); err != nil {
			return err
		}
		criteria := p.Criteria
		if criteria == nil {
			criteria = map[string]any{}
		}
		text, err := encode(criteria)
		if err != nil {
			return fmt.Errorf("criteria of cleanup policy %q: %w", p.Name, err)
		}
		if err := exec(tx, "INSERT INTO cleanup_policies VALUES (?, ?, ?, ?, ?)", p.Name, p.Format, p.Notes, text,
			p.Origin); err != nil {
			return fmt.Errorf("cleanup policy %q: %w", p.Name, err)
		}
	}
	return nil
}

func loadCleanupPolicies(tx *sql.Tx, snap *model.Snapshot) error {
	snap.CleanupPolicies = []model.CleanupPolicy{}
	return each(tx, "SELECT name, format, notes, criteria, origin FROM cleanup_policies ORDER BY name",
		func(rows *sql.Rows) error {
			var p model.CleanupPolicy
			var criteria string
			if err := rows.Scan(&p.Name, &p.Format, &p.Notes, &criteria, &p.Origin); err != nil {
				return err
			}
			p.Criteria = map[string]any{}
			if err := decode(criteria, &p.Criteria); err != nil {
				return fmt.Errorf("criteria of cleanup policy %q: %w", p.Name, err)
			}
			if p.Criteria == nil {
				p.Criteria = map[string]any{}
			}
			snap.CleanupPolicies = append(snap.CleanupPolicies, p)
			return nil
		})
}

// CleanupPolicies are the cleanup policies by name. The list is never nil.
func (st *Store) CleanupPolicies() ([]model.CleanupPolicy, error) {
	snap, err := st.readOne(loadCleanupPolicies)
	if err != nil {
		return nil, err
	}
	return snap.CleanupPolicies, nil
}

// PutCleanupPolicy adds p, or replaces the policy of its name, and raises
// the generation.
func (st *Store) PutCleanupPolicy(p model.CleanupPolicy) error {
	_, err := st.change(KindCleanupPolicies, func(snap *model.Snapshot) (bool, error) {
		snap.CleanupPolicies = slices.DeleteFunc(snap.CleanupPolicies, func(x model.CleanupPolicy) bool { return x.Name == p.Name })
		snap.CleanupPolicies = append(snap.CleanupPolicies, p)
		slices.SortFunc(snap.CleanupPolicies, func(a, b model.CleanupPolicy) int { return strings.Compare(a.Name, b.Name) })
		return true, nil
	})
	return err
}

// RemoveCleanupPolicy removes the policy of the name and raises the
// generation. false, and no write, when there is none.
func (st *Store) RemoveCleanupPolicy(name string) (bool, error) {
	return st.change(KindCleanupPolicies, func(snap *model.Snapshot) (bool, error) {
		n := len(snap.CleanupPolicies)
		snap.CleanupPolicies = slices.DeleteFunc(snap.CleanupPolicies, func(x model.CleanupPolicy) bool { return x.Name == name })
		return len(snap.CleanupPolicies) != n, nil
	})
}
