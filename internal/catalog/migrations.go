package catalog

import (
	"database/sql"

	"github.com/iogentic/pipesmith-registry/internal/migrate"
)

// SchemaVersion is the version of the last step, the value of the schema row
// of a file this binary writes.
const SchemaVersion = 2

// steps are the schema steps of catalog.sqlite, numbered from 1. A further
// table or column is the next step, and SchemaVersion follows it in the same
// commit; a step that exists never changes. A later version appends its step
// here and its accessors in a file of its own.
var steps = []migrate.Step{
	{Version: 1, Apply: schema1},
	{Version: 2, Apply: schema2},
}

// schema1 holds what a scan fills: one row per store and path in blobs, the
// winning blob of a path in assets, the coordinates in components, and per
// store the rows of the last fill in scans. An empty text in namespace or
// version of components means the coordinates have none.
var schema1 = statements(
	"CREATE TABLE IF NOT EXISTS scans (store TEXT PRIMARY KEY, blobs INTEGER NOT NULL, bytes INTEGER NOT NULL,"+
		" scanned_ms INTEGER) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS blobs (store TEXT NOT NULL, repo TEXT NOT NULL, path TEXT NOT NULL,"+
		" size INTEGER NOT NULL, sha1 TEXT, content_type TEXT, created_ms INTEGER NOT NULL, uploader TEXT,"+
		" uploader_ip TEXT, md5 TEXT, sha256 TEXT, sha512 TEXT, PRIMARY KEY (store, repo, path)) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS components (id INTEGER PRIMARY KEY, repo TEXT NOT NULL, format TEXT NOT NULL,"+
		" namespace TEXT NOT NULL, name TEXT NOT NULL, version TEXT NOT NULL, prerelease INTEGER NOT NULL,"+
		" UNIQUE (repo, format, namespace, name, version))",
	"CREATE TABLE IF NOT EXISTS assets (repo TEXT NOT NULL, path TEXT NOT NULL, store TEXT NOT NULL,"+
		" format TEXT NOT NULL, component INTEGER REFERENCES components (id), extension TEXT, classifier TEXT,"+
		" last_modified_ms INTEGER, last_downloaded_ms INTEGER, PRIMARY KEY (repo, path),"+
		" FOREIGN KEY (store, repo, path) REFERENCES blobs (store, repo, path)) WITHOUT ROWID",
	"CREATE INDEX IF NOT EXISTS assets_component ON assets (component) WHERE component IS NOT NULL",
)

// schema2 is the table of the side data the import of phase 2 brings
// (decision I10 of the phase 2 plan), empty until that import: one row per
// imported asset by repository and path, and one row with an empty path per
// imported blob without an asset, found by its blob reference.
var schema2 = statements(
	"CREATE TABLE IF NOT EXISTS imported_assets (id INTEGER PRIMARY KEY, repo TEXT NOT NULL, path TEXT NOT NULL,"+
		" blob_ref TEXT NOT NULL, old_id TEXT, last_updated_ms INTEGER, last_downloaded_ms INTEGER,"+
		" checksums TEXT NOT NULL, kind TEXT, namespace TEXT, name TEXT, version TEXT, attributes TEXT,"+
		" component_attributes TEXT)",
	"CREATE UNIQUE INDEX IF NOT EXISTS imported_assets_path ON imported_assets (repo, path) WHERE path <> ''",
	"CREATE INDEX IF NOT EXISTS imported_assets_blob ON imported_assets (blob_ref)",
)

func statements(list ...string) func(tx *sql.Tx) error {
	return func(tx *sql.Tx) error {
		for _, statement := range list {
			if _, err := tx.ExecContext(bg, statement); err != nil {
				return err
			}
		}
		return nil
	}
}
