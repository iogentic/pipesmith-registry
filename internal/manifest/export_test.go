package manifest

// Internal helpers that the external tests check against Python vectors.
var (
	Unescape  = unescape
	ParseLine = parseLine
	Digest    = digest
	OpenTable = openTable
)

// Exec runs a statement on the connection of the lookup.
func (l *Lookup) Exec(query string) error {
	_, err := l.db.Exec(query)
	return err
}
