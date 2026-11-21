package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// A state file of release 0.2.0 (schema 5) meets this binary: the open
// writes the copy state.sqlite.v5 of mode 0600 with the rows of the file
// as they were, then takes step 6. Every token row keeps its values and
// gets the empty name, the rows of the other tables stay, a name is unique
// per user, source and kind, and a second open writes nothing.
func TestSchema5FileTakesStep6(t *testing.T) {
	dir := dataDirOf(t, "v5.sqlite")
	path := Path(dir)
	released := dump(t, copied(t, path, filepath.Join(t.TempDir(), "released.sqlite")), true)
	before := keptRows(t, path, 5)
	if !strings.Contains(before["tokens"], `"AAAAAAAAAAA"`) || !strings.Contains(before["tokens"], `"BBBBBBBBBBB"`) {
		t.Fatalf("v5.sqlite lacks its two tokens:\n%s", before["tokens"])
	}

	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	sameRecords(t, got, recordsV1())

	copyPath := path + ".v5"
	info, err := os.Stat(copyPath)
	if err != nil {
		t.Fatalf("no copy of schema 5 beside the migrated file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("copy mode %o, want 600: the file holds password hashes and token digests", perm)
	}
	if kept := dump(t, copied(t, copyPath, filepath.Join(t.TempDir(), "kept.sqlite")), true); kept != released {
		t.Fatalf("the copy differs from the file of release 0.2.0\ncopy\n%s\nreleased\n%s", kept, released)
	}

	after := keptRows(t, path, 5)
	for table, rows := range before {
		if table == "tokens" {
			continue
		}
		if after[table] != rows {
			t.Fatalf("rows of %s changed\nbefore\n%s\nafter\n%s", table, rows, after[table])
		}
	}
	oldTokens, newTokens := strings.Split(before["tokens"], "\n"), strings.Split(after["tokens"], "\n")
	if len(oldTokens) != 2 || len(newTokens) != 2 {
		t.Fatalf("token rows before %d, after %d; want 2 and 2", len(oldTokens), len(newTokens))
	}
	for i := range oldTokens {
		if want := oldTokens[i] + `, text ""`; newTokens[i] != want {
			t.Fatalf("token row %d\n%s\nwant\n%s", i, newTokens[i], want)
		}
	}
	for _, line := range []string{`text "schema", int64 6`,
		"index tokens_name on tokens: CREATE UNIQUE INDEX tokens_name ON tokens (user_id, source, kind, name) WHERE name != ''"} {
		if migrated := dump(t, path, true); !strings.Contains(migrated, line) {
			t.Fatalf("the migrated file lacks %q:\n%s", line, migrated)
		}
	}

	issued := time.Date(2026, 11, 20, 22, 0, 0, 0, time.UTC)
	named := func(public, user, kind, name string) model.Token {
		return model.Token{Public: public, Kind: kind, UserID: user, Source: "default", Name: name, Created: issued}
	}
	if _, err := st.AddToken(named("CCCCCCCCCCC", "demo-admin", "user", "laptop"), 20); err != nil {
		t.Fatalf("a named token on the migrated file: %v", err)
	}
	if _, err := st.AddToken(named("DDDDDDDDDDD", "demo-admin", "user", "laptop"), 20); !errors.Is(err, model.ErrTokenName) {
		t.Fatalf("a second user token named laptop of demo-admin: %v, want ErrTokenName", err)
	}
	for _, other := range []model.Token{named("EEEEEEEEEEE", "demo-admin", "npm", "laptop"),
		named("FFFFFFFFFFF", "demo-reader", "user", "laptop")} {
		if _, err := st.AddToken(other, 20); err != nil {
			t.Fatalf("the name laptop for %s %s: %v", other.UserID, other.Kind, err)
		}
	}
	rows, err := st.Tokens("demo-admin", "default")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, row := range rows {
		names[row.Public] = row.Name
	}
	if len(names) != 4 || names["AAAAAAAAAAA"] != "" || names["BBBBBBBBBBB"] != "" || names["CCCCCCCCCCC"] != "laptop" {
		t.Fatalf("tokens of demo-admin after the migration: %v", names)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(copyPath); err != nil {
		t.Fatal(err)
	}
	migrated := dump(t, path, true)
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	if now := dump(t, path, true); now != migrated {
		t.Fatalf("the second open changed the file\n%s", now)
	}
	if _, err := os.Stat(copyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the second open wrote a copy: %v", err)
	}
}
