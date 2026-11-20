package state

import (
	"database/sql"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// columnsOf are the columns of a table of the file at path as
// "name type null|not null key", in their order.
func columnsOf(t *testing.T, path, table string) []string {
	t.Helper()
	name, err := dsn(path, url.Values{"mode": {"ro"}})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", name)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT name, type, \"notnull\", pk FROM pragma_table_info(?) ORDER BY cid", table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column, kind string
		var notNull, pk int
		if err := rows.Scan(&column, &kind, &notNull, &pk); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, strings.TrimSpace(strings.Join([]string{column, kind,
			map[int]string{0: "null", 1: "not null"}[notNull], map[int]string{0: "", 1: "key"}[pk]}, " ")))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return columns
}

// Step 5 is the last step of the numbered list (decision E3). A fresh file
// at schema 5 has task_runs and the tables of the cleanup policies, the
// tasks and the capabilities, each with the column origin (rulings S1 to
// S3).
func TestStep5IsInTheList(t *testing.T) {
	if SchemaVersion != 5 || len(steps) != 5 || steps[4].Version != 5 {
		t.Fatalf("SchemaVersion %d with %d steps", SchemaVersion, len(steps))
	}
	dir := filepath.Join(t.TempDir(), "data")
	st, err := Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	path := Path(dir)
	if schema := dump(t, path, false); !strings.Contains(schema, `text "schema", int64 5`) {
		t.Fatalf("a fresh file is not at schema 5:\n%s", schema)
	}
	for table, want := range map[string][]string{
		"task_runs": {"id INTEGER null key", "task_id TEXT not null", "started_ms INTEGER not null", "ended_ms INTEGER null",
			"result TEXT null", "message TEXT not null"},
		"cleanup_policies": {"name TEXT not null key", "format TEXT not null", "notes TEXT not null",
			"criteria TEXT not null", "origin TEXT not null"},
		"tasks": {"id TEXT not null key", "name TEXT not null", "type_id TEXT not null", "enabled INTEGER not null",
			"schedule TEXT not null", "properties TEXT not null", "imported_state TEXT null", "origin TEXT not null"},
		"capabilities": {"id TEXT not null key", "type_id TEXT not null", "enabled INTEGER not null", "notes TEXT not null",
			"properties TEXT not null", "origin TEXT not null"},
	} {
		if got := columnsOf(t, path, table); !slices.Equal(got, want) {
			t.Errorf("columns of %s\n%q\nwant\n%q", table, got, want)
		}
	}
}

// The origin of a row is import or local; the file refuses any other value.
func TestOriginIsImportOrLocal(t *testing.T) {
	st, err := Create(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, origin := range []string{"import", "local"} {
		if err := st.write(func(tx *sql.Tx) error {
			return exec(tx, "INSERT INTO capabilities VALUES (?, 'demo.type', 0, '', '{}', ?)", "c-"+origin, origin)
		}); err != nil {
			t.Fatalf("origin %s: %v", origin, err)
		}
	}
	for _, origin := range []string{"bootstrap", "", "Import"} {
		if err := st.write(func(tx *sql.Tx) error {
			return exec(tx, "INSERT INTO capabilities VALUES (?, 'demo.type', 0, '', '{}', ?)", "x-"+origin, origin)
		}); err == nil {
			t.Errorf("origin %q was stored", origin)
		}
	}
}
