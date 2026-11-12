package maven

// Lower is str.lower of Python 3.12, for the read path of the server, which
// lowers SHA1 texts of the index and of npm documents.
func Lower(s string) string {
	return pyLower(s)
}
