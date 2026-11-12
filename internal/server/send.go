package server

import (
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/mirosso25/pipesmith-registry/internal/index"
	"github.com/mirosso25/pipesmith-registry/internal/maven"
	"github.com/mirosso25/pipesmith-registry/internal/pystr"
)

// body is Body of the Python module: what _send answers with, either an
// open bytes file or bytes in memory.
type body struct {
	size        int64
	etag        string
	modifiedMS  *int64
	contentType string
	file        *os.File
	data        []byte
}

func (b *body) close() {
	if b.file != nil {
		b.file.Close()
	}
}

// strongEtag is the ETag of a stored file: its SHA1 in quotes, as version
// 3.87.1 of the old product sends it for hosted and proxy content.
func strongEtag(sha1 string) string { return `"` + sha1 + `"` }

// headerSafe is _header_safe: a value of printable ASCII, not empty.
func headerSafe(value *string) bool {
	if value == nil || *value == "" {
		return false
	}
	for i := 0; i < len(*value); i++ {
		if c := (*value)[i]; c < 0x20 || c >= 0x7F {
			return false
		}
	}
	return true
}

// fileBody is _file_body: the strong ETag only while the file has the size
// of its row and the row has a SHA1.
func fileBody(asset *index.Asset, f *os.File, size int64) *body {
	etag := fmt.Sprintf(`W/"%d-%d"`, size, asset.CreatedMS)
	if size == asset.Size && asset.SHA1 != nil && *asset.SHA1 != "" {
		etag = strongEtag(*asset.SHA1)
	}
	contentType := maven.ContentTypeFor(asset.Path)
	if headerSafe(asset.ContentType) {
		contentType = *asset.ContentType
	}
	created := asset.CreatedMS
	return &body{size: size, etag: etag, modifiedMS: &created, contentType: contentType, file: f}
}

var quotedEtag = regexp.MustCompile(`(?:W/)?"[^"]*"`)

func opaque(tag string) string {
	return strings.TrimPrefix(latin1Strip(tag), "W/")
}

// noneMatch is the weak comparison of If-None-Match against the ETag.
func noneMatch(header, etag string) bool {
	value := latin1Strip(header)
	if value == "*" {
		return true
	}
	candidates := quotedEtag.FindAllString(value, -1)
	if len(candidates) == 0 {
		candidates = strings.Split(value, ",")
	}
	mine := opaque(etag)
	for _, c := range candidates {
		if opaque(c) == mine {
			return true
		}
	}
	return false
}

func firstHeader(h http.Header, name string) (string, bool) {
	values := h.Values(name)
	if len(values) == 0 {
		return "", false
	}
	return values[0], true
}

// notModified is _not_modified: If-None-Match decides alone when it is
// sent, else If-Modified-Since against the second of the modification.
func notModified(h http.Header, etag string, modifiedMS *int64) (bool, error) {
	if value, ok := firstHeader(h, "If-None-Match"); ok {
		return noneMatch(value, etag), nil
	}
	if value, ok := firstHeader(h, "If-Modified-Since"); ok && value != "" && modifiedMS != nil {
		since, ok, err := parseHTTPDate(value)
		if err != nil {
			return false, err
		}
		return ok && floorDiv(*modifiedMS, 1000) <= since, nil
	}
	return false, nil
}

// ifRangeAllows is _if_range_allows: a Range header counts only while
// If-Range, when sent, still names this representation.
func ifRangeAllows(value *string, etag string, modifiedMS *int64) (bool, error) {
	if value == nil {
		return true, nil
	}
	v := latin1Strip(*value)
	if strings.HasPrefix(v, `"`) || strings.HasPrefix(v, "W/") {
		return !strings.HasPrefix(v, "W/") && !strings.HasPrefix(etag, "W/") && v == etag, nil
	}
	since, ok, err := parseHTTPDate(v)
	if err != nil {
		return false, err
	}
	return ok && modifiedMS != nil && floorDiv(*modifiedMS, 1000) == since, nil
}

type rangeKind int

const (
	rangeIgnored rangeKind = iota // the whole file is sent
	rangeSatisfiable
	rangeUnsatisfiable
)

// digits is _digits: ASCII decimal digits, at least one.
func digits(text string) bool {
	if text == "" {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return true
}

// parseRange is _parse_range for one range of bytes. The error is the
// ValueError of int() for a number of more than 4300 digits, which _send
// does not catch.
func parseRange(value string, size int64) (int64, int64, rangeKind, error) {
	unit, spec, found := strings.Cut(value, "=")
	if !found || asciiLower(latin1Strip(unit)) != "bytes" {
		return 0, 0, rangeIgnored, nil
	}
	spec = latin1Strip(spec)
	if strings.Contains(spec, ",") {
		return 0, 0, rangeIgnored, nil // several ranges: answered with the whole file
	}
	first, last, dash := strings.Cut(spec, "-")
	first, last = latin1Strip(first), latin1Strip(last)
	if !dash {
		return 0, 0, rangeIgnored, nil
	}
	bigSize := big.NewInt(size)
	if first == "" {
		if !digits(last) {
			return 0, 0, rangeIgnored, nil
		}
		count, err := pystr.ParseInt(last)
		if err != nil {
			return 0, 0, rangeIgnored, err
		}
		if count.Sign() == 0 || size == 0 {
			return 0, 0, rangeUnsatisfiable, nil
		}
		start := new(big.Int).Sub(bigSize, count)
		if start.Sign() < 0 {
			start.SetInt64(0)
		}
		return start.Int64(), size - 1, rangeSatisfiable, nil
	}
	if !digits(first) || last != "" && !digits(last) {
		return 0, 0, rangeIgnored, nil
	}
	start, err := pystr.ParseInt(first)
	if err != nil {
		return 0, 0, rangeIgnored, err
	}
	end := big.NewInt(size - 1)
	if last != "" {
		if end, err = pystr.ParseInt(last); err != nil {
			return 0, 0, rangeIgnored, err
		}
		if end.Cmp(start) < 0 {
			return 0, 0, rangeIgnored, nil
		}
	}
	if start.Cmp(bigSize) >= 0 {
		return 0, 0, rangeUnsatisfiable, nil
	}
	if end.Cmp(big.NewInt(size-1)) > 0 {
		end.SetInt64(size - 1)
	}
	return start.Int64(), end.Int64(), rangeSatisfiable, nil
}

// send is _send: the answer of a file with ETag, Last-Modified, ranges and
// the conditional requests. extra headers are set on every answer it gives,
// as the Docker read path sets its own after _send. The error is a failure
// Python does not catch; the body is closed in every case.
func send(w http.ResponseWriter, req *request, b *body, extra [][2]string) error {
	h := w.Header()
	lastModified := ""
	if b.modifiedMS != nil {
		lastModified, _ = httpDate(*b.modifiedMS)
	}
	setExtra := func() {
		for _, kv := range extra {
			h.Set(kv[0], kv[1])
		}
	}
	not, err := notModified(req.r.Header, b.etag, b.modifiedMS)
	if err != nil {
		b.close()
		return err
	}
	if not {
		b.close()
		h.Set("ETag", b.etag)
		h.Set("Accept-Ranges", "bytes")
		if lastModified != "" {
			h.Set("Last-Modified", lastModified)
		}
		setExtra()
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	status, start, length := http.StatusOK, int64(0), b.size
	contentRange := ""
	if rangeHeader, ok := firstHeader(req.r.Header, "Range"); ok {
		var ifRange *string
		if value, ok := firstHeader(req.r.Header, "If-Range"); ok {
			ifRange = &value
		}
		allowed, err := ifRangeAllows(ifRange, b.etag, b.modifiedMS)
		if err != nil {
			b.close()
			return err
		}
		if allowed {
			first, last, kind, err := parseRange(rangeHeader, b.size)
			if err != nil {
				b.close()
				return err
			}
			switch kind {
			case rangeUnsatisfiable:
				b.close()
				h.Set("Content-Range", fmt.Sprintf("bytes */%d", b.size))
				h.Set("Accept-Ranges", "bytes")
				h.Set("Content-Length", "0")
				setExtra()
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return nil
			case rangeSatisfiable:
				status, start, length = http.StatusPartialContent, first, last-first+1
				contentRange = fmt.Sprintf("bytes %d-%d/%d", first, last, b.size)
			}
		}
	}
	h.Set("Content-Type", b.contentType)
	h.Set("ETag", b.etag)
	h.Set("Accept-Ranges", "bytes")
	if lastModified != "" {
		h.Set("Last-Modified", lastModified)
	}
	if contentRange != "" {
		h.Set("Content-Range", contentRange)
	}
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	setExtra()
	if req.head {
		b.close()
		w.WriteHeader(status)
		return nil
	}
	if b.data != nil {
		w.WriteHeader(status)
		w.Write(b.data[start : start+length])
		return nil
	}
	defer b.close()
	if start > 0 {
		if _, err := b.file.Seek(start, io.SeekStart); err != nil {
			return err
		}
	}
	w.WriteHeader(status)
	copied, err := io.CopyBuffer(w, io.LimitReader(b.file, length), make([]byte, chunkBytes))
	if err == nil && copied < length {
		logAt(slogWarn, serverLogger, "%s ended %d bytes early", b.file.Name(), length-copied)
	}
	return nil
}
