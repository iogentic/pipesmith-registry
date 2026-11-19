package ldapauth

// Waiting is the number of sign ins that wait for a place of the connection
// now.
func Waiting(d *Directory) int64 { return d.places.waiting.Load() }
