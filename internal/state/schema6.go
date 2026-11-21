package state

import "database/sql"

// schema6 is step 6 of state.sqlite (ruling AP2 of leg 6-api): the name
// of a token, which POST /api/tokens sets. A row of an earlier step, an npm
// login among them, has the empty name. A name that is not empty is unique
// per user, source and kind. Once listed the step never changes.
func schema6(tx *sql.Tx) error {
	for _, statement := range []string{
		"ALTER TABLE tokens ADD COLUMN name TEXT NOT NULL DEFAULT ''",
		"CREATE UNIQUE INDEX IF NOT EXISTS tokens_name ON tokens (user_id, source, kind, name) WHERE name != ''",
	} {
		if _, err := tx.ExecContext(bg, statement); err != nil {
			return err
		}
	}
	return nil
}
