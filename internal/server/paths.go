package server

import (
	"math/big"
	"strings"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/pystr"
)

// target is the repository name and path of a request.
type target struct {
	repo      string
	path      string // without leading and trailing slash, "" for the root
	directory bool   // the URL ends with "/"
	bare      bool   // "/repository/<name>" without any slash after the name
}

// badPath is BadPath of the Python module: the decoded request path is not
// a repository path.
type badPath struct{ reason string }

func (e *badPath) Error() string { return e.reason }

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

// unquoteToBytes is urllib.parse.unquote_to_bytes: every "%" with two hex
// digits becomes its byte, any other "%" stays.
func unquoteToBytes(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			out = append(out, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
			continue
		}
		out = append(out, s[i])
	}
	return out
}

// decodeReplace is bytes.decode("utf-8", "replace"): every maximal invalid
// subpart becomes one U+FFFD, as Python counts them.
func decodeReplace(b []byte) string {
	var sb strings.Builder
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || size > 1 {
			sb.Write(b[i : i+size])
			i += size
			continue
		}
		sb.WriteRune(utf8.RuneError)
		i += maximalSubpart(b[i:])
	}
	return sb.String()
}

func maximalSubpart(b []byte) int {
	need, lo, hi := 0, byte(0x80), byte(0xBF)
	switch lead := b[0]; {
	case lead >= 0xC2 && lead <= 0xDF:
		need = 1
	case lead == 0xE0:
		need, lo = 2, 0xA0
	case lead >= 0xE1 && lead <= 0xEC, lead == 0xEE, lead == 0xEF:
		need = 2
	case lead == 0xED:
		need, hi = 2, 0x9F
	case lead == 0xF0:
		need, lo = 3, 0x90
	case lead >= 0xF1 && lead <= 0xF3:
		need = 3
	case lead == 0xF4:
		need, hi = 3, 0x8F
	default:
		return 1
	}
	n := 1
	for k := 0; k < need && n < len(b); k++ {
		c := b[n]
		if k == 0 && (c < lo || c > hi) || k > 0 && (c < 0x80 || c > 0xBF) {
			break
		}
		n++
	}
	return n
}

// routePath is the path the router of the Python server matches: uvicorn's
// unquote of the raw path, invalid UTF-8 replaced.
func routePath(raw string) string {
	if !strings.Contains(raw, "%") {
		return raw
	}
	return decodeReplace(unquoteToBytes(raw))
}

// accessPath is the path of the access line: the route path quoted again
// with "/" kept, and the raw query after "?".
func accessPath(raw, query string) string {
	path := quote(routePath(raw), "/")
	if query != "" {
		path += "?" + query
	}
	return path
}

// parseTarget is _target: the raw path decoded once, strictly as UTF-8.
// It returns nil for a path outside prefix or without a repository name.
func parseTarget(raw, prefix string) (*target, error) {
	decodedBytes := unquoteToBytes(raw)
	if !utf8.Valid(decodedBytes) {
		return nil, &badPath{"not UTF-8"}
	}
	decoded := string(decodedBytes)
	if strings.Contains(decoded, "\x00") {
		return nil, &badPath{"NUL byte"}
	}
	if !strings.HasPrefix(decoded, prefix) {
		return nil, nil
	}
	repo, path, slash := strings.Cut(decoded[len(prefix):], "/")
	if repo == "" {
		return nil, nil
	}
	if !slash {
		return &target{repo: repo, bare: true}, nil
	}
	directory := path == "" || strings.HasSuffix(path, "/")
	core := strings.TrimSuffix(path, "/")
	if core != "" {
		for _, segment := range strings.Split(core, "/") {
			if segment == "" || segment == "." || segment == ".." {
				return nil, &badPath{"empty, '.' or '..' segment"}
			}
		}
	}
	return &target{repo: repo, path: core, directory: directory}, nil
}

const alwaysSafe = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_.-~"

// quote is urllib.parse.quote(s, safe=safe) of a UTF-8 text.
func quote(s, safe string) string {
	const upper = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x80 && (strings.IndexByte(alwaysSafe, c) >= 0 || strings.IndexByte(safe, c) >= 0) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upper[c>>4])
		b.WriteByte(upper[c&15])
	}
	return b.String()
}

// repoURL is _url: the URL of a directory of a repository.
func repoURL(prefix, repo, path string) string {
	tail := ""
	if path != "" {
		tail = quote(path, "/") + "/"
	}
	return prefix + quote(repo, "") + "/" + tail
}

// pair is one item of a query string.
type pair struct{ name, value string }

type queryValues []pair

// parseQuery is the QueryParams of Starlette: parse_qsl with blank values
// kept, "+" as a space, invalid UTF-8 replaced.
func parseQuery(raw string) queryValues {
	var out queryValues
	if raw == "" {
		return out
	}
	unquote := func(s string) string {
		return routePath(strings.ReplaceAll(s, "+", " "))
	}
	for _, item := range strings.Split(raw, "&") {
		if item == "" {
			continue
		}
		name, value, _ := strings.Cut(item, "=")
		out = append(out, pair{unquote(name), unquote(value)})
	}
	return out
}

// get is the last value of a name, as Starlette's QueryParams.get answers.
func (q queryValues) get(name string) (string, bool) {
	for i := len(q) - 1; i >= 0; i-- {
		if q[i].name == name {
			return q[i].value, true
		}
	}
	return "", false
}

// apiError is ApiError of the Python module: an answer of the JSON API other
// than 200.
type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return e.message }

// apiPath is _api_path: a repository path from a query parameter, one
// leading and one trailing slash dropped.
func apiPath(value *string) (string, *apiError) {
	if value == nil {
		return "", nil
	}
	if strings.Contains(*value, "\x00") {
		return "", &apiError{400, "the path holds a NUL byte"}
	}
	core := strings.TrimSuffix(strings.TrimPrefix(*value, "/"), "/")
	if core != "" {
		for _, segment := range strings.Split(core, "/") {
			if segment == "" || segment == "." || segment == ".." {
				return "", &apiError{400, "the path holds an empty, '.' or '..' segment"}
			}
		}
	}
	return core, nil
}

// intParam is _int_param: int() of a query parameter within low and high
// (nil for none); a missing or empty parameter gives the default.
func intParam(q queryValues, name string, def, low int64, high *int64) (*big.Int, *apiError) {
	raw, ok := q.get(name)
	if !ok || raw == "" {
		return big.NewInt(def), nil
	}
	value, err := pystr.ParseInt(raw)
	if err != nil {
		return nil, &apiError{400, name + " is not a number"}
	}
	if value.Cmp(big.NewInt(low)) < 0 || high != nil && value.Cmp(big.NewInt(*high)) > 0 {
		bounds := "at least " + big.NewInt(low).String()
		if high != nil {
			bounds = "from " + big.NewInt(low).String() + " to " + big.NewInt(*high).String()
		}
		return nil, &apiError{400, name + " must be " + bounds}
	}
	return value, nil
}
