package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// Step 3 of state.sqlite (decision E3 of the execution plan): the routing
// rules of phase 5 and the rule each repository names, in tables of their
// own (decision P5-1 of the phase 5 session), written and read only here,
// so that the loader of the repositories stays as it is.
//
// Step 3 is in the numbered list of migrations.go since the merge of
// step 2 of the security stream.

// schema3 creates the routing rules, with their matchers as a JSON list, and
// the link of a repository to the name of its rule. A link may name a rule
// that does not exist; the routing package blocks such a repository.
func schema3(tx *sql.Tx) error {
	for _, statement := range []string{
		"CREATE TABLE IF NOT EXISTS routing_rules (name TEXT PRIMARY KEY, description TEXT NOT NULL, mode TEXT NOT NULL," +
			" matchers TEXT NOT NULL) WITHOUT ROWID",
		"CREATE TABLE IF NOT EXISTS routing_links (repository TEXT PRIMARY KEY, rule TEXT NOT NULL) WITHOUT ROWID",
	} {
		if _, err := tx.ExecContext(bg, statement); err != nil {
			return err
		}
	}
	return nil
}

// Routing reads the routing rules, ordered by name, and the links, in one
// read transaction. The lists
// and the map are never nil.
func (st *Store) Routing() (model.Routing, error) {
	tx, err := st.readers.BeginTx(bg, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.Routing{Rules: []model.RoutingRule{}, Links: map[string]string{}}, err
	}
	defer func() { _ = tx.Rollback() }() // a read transaction has nothing to keep
	return loadRouting(tx)
}

// loadRouting reads the rules, ordered by name, and the links in tx.
func loadRouting(tx *sql.Tx) (model.Routing, error) {
	out := model.Routing{Rules: []model.RoutingRule{}, Links: map[string]string{}}
	err := each(tx, "SELECT name, description, mode, matchers FROM routing_rules ORDER BY name", func(rows *sql.Rows) error {
		var r model.RoutingRule
		var matchers string
		if err := rows.Scan(&r.Name, &r.Description, &r.Mode, &matchers); err != nil {
			return err
		}
		r.Matchers = []string{}
		if err := json.Unmarshal([]byte(matchers), &r.Matchers); err != nil {
			return fmt.Errorf("matchers of routing rule %q: %w", r.Name, err)
		}
		if r.Matchers == nil {
			r.Matchers = []string{}
		}
		out.Rules = append(out.Rules, r)
		return nil
	})
	if err != nil {
		return model.Routing{Rules: []model.RoutingRule{}, Links: map[string]string{}}, err
	}
	err = each(tx, "SELECT repository, rule FROM routing_links ORDER BY repository", func(rows *sql.Rows) error {
		var repository, rule string
		if err := rows.Scan(&repository, &rule); err != nil {
			return err
		}
		out.Links[repository] = rule
		return nil
	})
	if err != nil {
		return model.Routing{Rules: []model.RoutingRule{}, Links: map[string]string{}}, err
	}
	return out, nil
}

// WriteRouting replaces every routing rule and every link with those of r,
// in one transaction. A rule needs a name that no other rule of r has; a
// link needs a repository and the name of a rule.
func (st *Store) WriteRouting(r model.Routing) error {
	if err := checkRouting(r); err != nil {
		return err
	}
	return st.write(func(tx *sql.Tx) error { return writeRouting(tx, r) })
}

// UpdateRouting reads the rules and the links, lets fn change them, and
// writes them as WriteRouting does, in one transaction that raises the
// generation (ruling SY3 of leg 6-system): a running server sees the
// generation and takes the rules at its next request. An error of fn or a
// result WriteRouting refuses changes nothing.
func (st *Store) UpdateRouting(fn func(r *model.Routing) error) error {
	return st.identityWrite(func(tx *sql.Tx) error {
		r, err := loadRouting(tx)
		if err != nil {
			return err
		}
		if err := fn(&r); err != nil {
			return err
		}
		if err := checkRouting(r); err != nil {
			return err
		}
		return writeRouting(tx, r)
	})
}

// checkRouting refuses a rule without a name, a name given twice and a
// link that lacks a name.
func checkRouting(r model.Routing) error {
	names := map[string]bool{}
	for _, rule := range r.Rules {
		switch {
		case rule.Name == "":
			return errors.New("a routing rule has no name")
		case names[rule.Name]:
			return fmt.Errorf("routing rule %q is given twice", rule.Name)
		}
		names[rule.Name] = true
	}
	for repository, rule := range r.Links {
		if repository == "" || rule == "" {
			return fmt.Errorf("the link of repository %q to routing rule %q lacks a name", repository, rule)
		}
	}
	return nil
}

// writeRouting replaces every rule and every link with those of r in tx.
func writeRouting(tx *sql.Tx, r model.Routing) error {
	if err := exec(tx, "DELETE FROM routing_links"); err != nil {
		return err
	}
	if err := exec(tx, "DELETE FROM routing_rules"); err != nil {
		return err
	}
	for _, rule := range r.Rules {
		matchers := rule.Matchers
		if matchers == nil {
			matchers = []string{}
		}
		text, err := encode(matchers)
		if err != nil {
			return err
		}
		if err := exec(tx, "INSERT INTO routing_rules VALUES (?, ?, ?, ?)", rule.Name, rule.Description, rule.Mode,
			text); err != nil {
			return fmt.Errorf("routing rule %q: %w", rule.Name, err)
		}
	}
	for _, repository := range slices.Sorted(maps.Keys(r.Links)) {
		if err := exec(tx, "INSERT INTO routing_links VALUES (?, ?)", repository, r.Links[repository]); err != nil {
			return fmt.Errorf("routing link of repository %q: %w", repository, err)
		}
	}
	return nil
}
