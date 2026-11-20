package ldapauth

// Waiting is the number of sign ins that wait for a place of the connection
// now.
func Waiting(d *Directory) int64 { return d.places.waiting.Load() }

// LookupWaiting is the number of lookups that wait for a place of the
// connection now.
func LookupWaiting(d *Directory) int64 { return d.lookups.waiting.Load() }

// Failures is the count of incidents in a row of the connection.
func Failures(d *Directory) int { return d.breaker.failures() }
