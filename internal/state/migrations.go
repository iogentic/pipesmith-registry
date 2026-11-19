package state

import (
	"database/sql"

	"github.com/iogentic/pipesmith-registry/internal/migrate"
)

// SchemaVersion is the version of the last step, the value of the schema row
// of a file this binary writes.
const SchemaVersion = 2

// steps are the schema steps of state.sqlite, numbered from 1. Schema 1 is
// the one of releases 0.1.0 and 0.1.1 and never changes: a further table or
// column is the next step, and SchemaVersion follows it in the same commit.
// The accessors of a step live in a file of their own.
var steps = []migrate.Step{
	{Version: 1, Apply: schema1},
	{Version: 2, Apply: schema2},
}

// schema1 creates the tables of schema 1 beside the meta table of the runner.
func schema1(tx *sql.Tx) error {
	for _, statement := range []string{
		"CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY, first_name TEXT NOT NULL, last_name TEXT NOT NULL," +
			" email TEXT NOT NULL, status TEXT NOT NULL, source TEXT NOT NULL, password_hash TEXT NOT NULL) WITHOUT ROWID",
		"CREATE TABLE IF NOT EXISTS user_roles (user_id TEXT NOT NULL, source TEXT NOT NULL, position INTEGER NOT NULL," +
			" role_id TEXT NOT NULL, PRIMARY KEY (user_id, source, position)) WITHOUT ROWID",
		"CREATE TABLE IF NOT EXISTS roles (id TEXT PRIMARY KEY, source TEXT NOT NULL, name TEXT NOT NULL," +
			" description TEXT NOT NULL, read_only INTEGER NOT NULL) WITHOUT ROWID",
		"CREATE TABLE IF NOT EXISTS role_privileges (role_id TEXT NOT NULL REFERENCES roles (id) ON DELETE CASCADE," +
			" position INTEGER NOT NULL, privilege_id TEXT NOT NULL, PRIMARY KEY (role_id, position)) WITHOUT ROWID",
		"CREATE TABLE IF NOT EXISTS role_roles (role_id TEXT NOT NULL REFERENCES roles (id) ON DELETE CASCADE," +
			" position INTEGER NOT NULL, member_id TEXT NOT NULL, PRIMARY KEY (role_id, position)) WITHOUT ROWID",
		"CREATE TABLE IF NOT EXISTS privileges (name TEXT PRIMARY KEY, type TEXT NOT NULL, description TEXT NOT NULL," +
			" read_only INTEGER NOT NULL, properties TEXT NOT NULL) WITHOUT ROWID",
		"CREATE TABLE IF NOT EXISTS ldap_servers (id TEXT PRIMARY KEY, name TEXT NOT NULL, position INTEGER NOT NULL," +
			" record TEXT NOT NULL) WITHOUT ROWID",
		"CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID",
		"CREATE TABLE IF NOT EXISTS blob_stores (name TEXT PRIMARY KEY, type TEXT NOT NULL, path TEXT NOT NULL," +
			" soft_quota TEXT) WITHOUT ROWID",
		"CREATE TABLE IF NOT EXISTS repositories (name TEXT PRIMARY KEY, format TEXT NOT NULL, type TEXT NOT NULL," +
			" online INTEGER NOT NULL, attributes TEXT NOT NULL) WITHOUT ROWID",
	} {
		if _, err := tx.ExecContext(bg, statement); err != nil {
			return err
		}
	}
	return nil
}

// schema2 adds the tokens this server issues and the generation row
// (decision E3 of the execution plan). A token row holds its user, kind and
// public half, the SHA-256 of its secret half, and the times of its issue
// and of its last use in milliseconds since 1970 UTC; the rowid keeps the
// order of issue. The generation is the row generation of the meta table,
// which every identity write raises (tokens.go).
func schema2(tx *sql.Tx) error {
	for _, statement := range []string{
		"CREATE TABLE IF NOT EXISTS tokens (public TEXT NOT NULL UNIQUE, kind TEXT NOT NULL, user_id TEXT NOT NULL," +
			" source TEXT NOT NULL, digest BLOB NOT NULL, created INTEGER NOT NULL, last_used INTEGER)",
		"CREATE INDEX IF NOT EXISTS tokens_owner ON tokens (user_id, source, kind)",
		"INSERT INTO meta VALUES ('" + generationKey + "', 0) ON CONFLICT (key) DO NOTHING",
	} {
		if _, err := tx.ExecContext(bg, statement); err != nil {
			return err
		}
	}
	return nil
}
