package state

// SetProcessUID replaces the user a secret file must belong to and returns
// the function that restores it.
func SetProcessUID(uid func() int) (restore func()) {
	old := processUID
	processUID = uid
	return func() { processUID = old }
}
