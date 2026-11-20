package logs

import "time"

// Request is the record of one request. It holds no header except the user
// agent and the content length.
type Request struct {
	ID            string    // 16 lower case hex digits
	Time          time.Time // when the answer ended, UTC
	Client        string    // the client of the access line without its port, IPv6 without brackets
	Subject       string    // the user id of the subject the chain resolved, else empty
	Realm         string    // the realm of that subject, else empty
	Method        string
	Path          string // the path of the access line without its query
	Query         string // the raw query, masked
	Protocol      string // HTTP/<major>.<minor> of the request
	Status        int    // the status of the answer, 0 when the handler wrote no header
	ContentLength string // the raw Content-Length header of the request, empty when absent
	RequestBytes  int64  // bytes of the request body the server read
	ResponseBytes int64  // bytes of the answer body the client received
	Duration      time.Duration
	UserAgent     string
	Repository    string // the repository of the request when it exists, else empty
	Format        string // the format of that repository
	Port          int    // the port of the listener, 0 when unknown
}
