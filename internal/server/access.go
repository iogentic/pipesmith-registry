package server

import (
	"fmt"
	"net/http"
)

// recorder writes the access line when the answer starts, as uvicorn does.
type recorder struct {
	http.ResponseWriter
	s      *Server
	r      *http.Request
	path   string // the path of the access line
	client string // the client of the access line, "<host>:<port>"
	wrote  bool
}

func (w *recorder) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.s.accessLine(w.r, w.client, w.path, status)
	w.ResponseWriter.WriteHeader(status)
}

func (w *recorder) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (s *Server) writeLog(line string) {
	if s.access == nil {
		return
	}
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	fmt.Fprint(s.access, line)
}

// accessLine is the line of uvicorn's access logger:
// INFO:     <client>:<port> - "<METHOD> <path> HTTP/<version>" <status> <phrase>
func (s *Server) accessLine(r *http.Request, client, path string, status int) {
	if s.access == nil {
		return
	}
	s.writeLog(fmt.Sprintf("INFO:     %s - \"%s %s HTTP/%d.%d\" %d %s\n", client, r.Method, path, r.ProtoMajor, r.ProtoMinor,
		status, statusPhrase(status)))
}

// statusPhrase is http.HTTPStatus(status).phrase of Python 3.12, "" for a
// status it does not name.
func statusPhrase(status int) string {
	return phrases[status]
}

var phrases = map[int]string{
	100: "Continue",
	101: "Switching Protocols",
	102: "Processing",
	103: "Early Hints",
	200: "OK",
	201: "Created",
	202: "Accepted",
	203: "Non-Authoritative Information",
	204: "No Content",
	205: "Reset Content",
	206: "Partial Content",
	207: "Multi-Status",
	208: "Already Reported",
	226: "IM Used",
	300: "Multiple Choices",
	301: "Moved Permanently",
	302: "Found",
	303: "See Other",
	304: "Not Modified",
	305: "Use Proxy",
	307: "Temporary Redirect",
	308: "Permanent Redirect",
	400: "Bad Request",
	401: "Unauthorized",
	402: "Payment Required",
	403: "Forbidden",
	404: "Not Found",
	405: "Method Not Allowed",
	406: "Not Acceptable",
	407: "Proxy Authentication Required",
	408: "Request Timeout",
	409: "Conflict",
	410: "Gone",
	411: "Length Required",
	412: "Precondition Failed",
	413: "Request Entity Too Large",
	414: "Request-URI Too Long",
	415: "Unsupported Media Type",
	416: "Requested Range Not Satisfiable",
	417: "Expectation Failed",
	418: "I'm a Teapot",
	421: "Misdirected Request",
	422: "Unprocessable Entity",
	423: "Locked",
	424: "Failed Dependency",
	425: "Too Early",
	426: "Upgrade Required",
	428: "Precondition Required",
	429: "Too Many Requests",
	431: "Request Header Fields Too Large",
	451: "Unavailable For Legal Reasons",
	500: "Internal Server Error",
	501: "Not Implemented",
	502: "Bad Gateway",
	503: "Service Unavailable",
	504: "Gateway Timeout",
	505: "HTTP Version Not Supported",
	506: "Variant Also Negotiates",
	507: "Insufficient Storage",
	508: "Loop Detected",
	510: "Not Extended",
	511: "Network Authentication Required",
}
