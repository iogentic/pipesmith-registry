package logship

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SettingKey is the setting of state.sqlite that holds the targets: a JSON
// list with one object per target.
const SettingKey = "log_targets"

// The kinds of target.
const (
	kindSyslog        = "syslog"
	kindGELF          = "gelf"
	kindLoki          = "loki"
	kindOTLP          = "otlp"
	kindSplunkHEC     = "splunk-hec"
	kindElasticsearch = "elasticsearch"
	kindHTTPJSON      = "http-json"
)

// Target is one row of the setting log_targets as it is stored. A number of
// zero and a missing list of files take their default.
type Target struct {
	Name            string   `json:"name"`
	Kind            string   `json:"kind"`
	Address         string   `json:"address"`
	Files           []string `json:"files,omitempty"`                 // out of server, requests, audit, tasks; none is all four
	QueueLines      int      `json:"queue_lines,omitempty"`           // default 10,000
	QueueBytes      int64    `json:"queue_bytes,omitempty"`           // default 16 MiB
	BatchLines      int      `json:"batch_lines,omitempty"`           // default 500
	FlushIntervalMS int      `json:"flush_interval_ms,omitempty"`     // default 1,000
	TimeoutMS       int      `json:"timeout_ms,omitempty"`            // default 5,000
	CAFile          string   `json:"ca_file,omitempty"`               // the only trusted roots; none is the roots of the system
	Secret          string   `json:"secret,omitempty" audit:"secret"` // the name of the secret file below secrets/
	User            string   `json:"user,omitempty"`                  // the user of HTTP Basic; the secret is the password
	Index           string   `json:"index,omitempty"`                 // elasticsearch: the index or data stream
}

// The defaults of a target.
const (
	defaultQueueLines = 10_000
	defaultQueueBytes = 16 << 20
	defaultBatchLines = 500
	defaultFlushMS    = 1_000
	defaultTimeoutMS  = 5_000
	defaultIndex      = "pipesmith-registry"
)

// bound is the range of one number of a row.
type bound struct {
	key      string
	min, max int
}

var bounds = []bound{
	{"queue_lines", 1, 1_000_000},
	{"queue_bytes", 64 << 10, 256 << 20},
	{"batch_lines", 1, 10_000},
	{"flush_interval_ms", 100, 60_000},
	{"timeout_ms", 100, 60_000},
}

var knownKeys = map[string]bool{"name": true, "kind": true, "address": true, "files": true, "queue_lines": true,
	"queue_bytes": true, "batch_lines": true, "flush_interval_ms": true, "timeout_ms": true, "ca_file": true,
	"secret": true, "user": true, "index": true}

// The bit of each file in the mask of a target.
const (
	fileServer uint8 = 1 << iota
	fileRequests
	fileAudit
	fileTasks
	allFiles = fileServer | fileRequests | fileAudit | fileTasks
)

var fileBits = map[string]uint8{"server": fileServer, "requests": fileRequests, "audit": fileAudit, "tasks": fileTasks}

var webSchemes = []string{"http", "https"}

// kindRule is what the address of a kind takes.
type kindRule struct {
	schemes []string            // the schemes of the address
	secret  []string            // the schemes over which the kind takes a secret
	path    func(Target) string // the path added to an http or https address without one; nil adds none
}

var kindRules = map[string]kindRule{
	kindSyslog:        {schemes: []string{"udp", "tcp", "tls"}},
	kindGELF:          {schemes: []string{"udp", "tcp", "http", "https"}, secret: webSchemes, path: fixedPath("/gelf")},
	kindLoki:          {schemes: webSchemes, secret: webSchemes, path: fixedPath("/loki/api/v1/push")},
	kindOTLP:          {schemes: webSchemes, secret: webSchemes, path: fixedPath("/v1/logs")},
	kindSplunkHEC:     {schemes: webSchemes, secret: webSchemes, path: fixedPath("/services/collector/event")},
	kindElasticsearch: {schemes: webSchemes, secret: webSchemes, path: func(t Target) string { return "/" + t.index() + "/_bulk" }},
	kindHTTPJSON:      {schemes: webSchemes, secret: webSchemes},
}

func fixedPath(path string) func(Target) string { return func(Target) string { return path } }

var (
	namePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	secretPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	indexPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,254}$`)
)

// ParseTarget reads and checks one row of the setting. It is the one
// validation of the command log-target and of the manager. An error names
// the rule the row breaks, never its address.
func ParseTarget(row []byte) (Target, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(row, &fields); err != nil || fields == nil {
		return Target{}, errors.New("the row is not a JSON object")
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !knownKeys[key] {
			return Target{}, fmt.Errorf("unknown key %q", key)
		}
	}
	r := fieldReader{fields: fields}
	var t Target
	r.text("name", &t.Name)
	r.text("kind", &t.Kind)
	r.text("address", &t.Address)
	r.text("ca_file", &t.CAFile)
	r.text("secret", &t.Secret)
	r.text("user", &t.User)
	r.text("index", &t.Index)
	r.files(&t.Files)
	for _, b := range bounds {
		n, present := r.number(b.key)
		if present && (n < b.min || n > b.max) && r.err == nil {
			r.err = fmt.Errorf("%s is %d, outside %d to %d", b.key, n, b.min, b.max)
		}
		switch b.key {
		case "queue_lines":
			t.QueueLines = n
		case "queue_bytes":
			t.QueueBytes = int64(n)
		case "batch_lines":
			t.BatchLines = n
		case "flush_interval_ms":
			t.FlushIntervalMS = n
		case "timeout_ms":
			t.TimeoutMS = n
		}
	}
	if r.err != nil {
		return t, r.err
	}
	return t, t.check()
}

// check applies the rules of the kind to the fields of a row.
func (t Target) check() error {
	switch {
	case t.Name == "":
		return errors.New("name is missing")
	case !namePattern.MatchString(t.Name):
		return fmt.Errorf("name %q is not 1 to 32 of a-z, 0-9, _ and -, starting with a letter or a digit", t.Name)
	case t.Kind == "":
		return errors.New("kind is missing")
	case t.Address == "":
		return errors.New("address is missing")
	}
	rule, known := kindRules[t.Kind]
	if !known {
		return fmt.Errorf("unknown kind %q", t.Kind)
	}
	u, err := t.checkAddress(rule)
	if err != nil {
		return err
	}
	if t.CAFile != "" {
		if !filepath.IsAbs(t.CAFile) {
			return errors.New("ca_file is not an absolute path")
		}
		if u.Scheme != "https" && u.Scheme != "tls" {
			return errors.New("ca_file needs https or tls")
		}
	}
	if t.Secret != "" {
		switch {
		case !secretPattern.MatchString(t.Secret):
			return errors.New("secret is not the name of a file below secrets/")
		case !slices.Contains(rule.secret, u.Scheme):
			return fmt.Errorf("kind %s takes no secret over %s", t.Kind, u.Scheme)
		case u.Scheme != "https" && u.Scheme != "tls" && !loopback(u.Hostname()):
			return errors.New("a secret only over https or tls, or to a loopback address")
		}
	}
	if t.User != "" {
		switch {
		case t.Secret == "":
			return errors.New("user needs a secret, the password")
		case u.Scheme != "http" && u.Scheme != "https":
			return errors.New("user is for http and https only")
		case len(t.User) > 256 || strings.ContainsFunc(t.User, func(r rune) bool { return r == ':' || r < 0x20 || r == 0x7f }):
			return errors.New("user holds a colon or a control character, or is longer than 256 bytes")
		}
	}
	if t.Index != "" {
		if t.Kind != kindElasticsearch {
			return errors.New("index is for kind elasticsearch only")
		}
		if !indexPattern.MatchString(t.Index) {
			return errors.New("index is not 1 to 255 of a-z, 0-9, ., _ and -, starting with a letter or a digit")
		}
	}
	return nil
}

// checkAddress parses the address and applies the rules of its scheme.
func (t Target) checkAddress(rule kindRule) (*url.URL, error) {
	u, err := url.Parse(t.Address)
	switch {
	case err != nil:
		return nil, errors.New("address is not a URL")
	case u.Scheme == "":
		return nil, errors.New("address has no scheme")
	case !slices.Contains(rule.schemes, u.Scheme):
		return nil, fmt.Errorf("kind %s takes no scheme %q", t.Kind, u.Scheme)
	case u.Opaque != "" || u.Hostname() == "":
		return nil, errors.New("address has no host")
	case u.User != nil:
		return nil, errors.New("address holds user info: give the user as user and the password as the secret")
	case u.Fragment != "":
		return nil, errors.New("address holds a fragment")
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("port %s is outside 1 to 65535", port)
		}
	}
	if u.Scheme == "udp" || u.Scheme == "tcp" || u.Scheme == "tls" {
		if u.Port() == "" {
			return nil, fmt.Errorf("an address of scheme %s needs a port", u.Scheme)
		}
		if u.Path != "" || u.RawQuery != "" || u.ForceQuery {
			return nil, fmt.Errorf("an address of scheme %s takes no path or query", u.Scheme)
		}
	}
	return u, nil
}

// loopback reports whether host is a literal loopback address.
func loopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// fieldReader reads the fields of one row and keeps the first error.
type fieldReader struct {
	fields map[string]json.RawMessage
	err    error
}

// present reports whether key holds a value other than null.
func (r *fieldReader) present(key string) bool {
	raw, ok := r.fields[key]
	return ok && r.err == nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func (r *fieldReader) text(key string, into *string) {
	if r.present(key) && json.Unmarshal(r.fields[key], into) != nil {
		r.err = fmt.Errorf("%s is not a string", key)
	}
}

func (r *fieldReader) number(key string) (int, bool) {
	if !r.present(key) {
		return 0, false
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(r.fields[key]))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		r.err = fmt.Errorf("%s is not a number", key)
		return 0, false
	}
	text, isNumber := v.(json.Number)
	if !isNumber {
		r.err = fmt.Errorf("%s is not a number", key)
		return 0, false
	}
	n, err := strconv.Atoi(string(text))
	if err != nil {
		r.err = fmt.Errorf("%s is not a whole number", key)
		return 0, false
	}
	return n, true
}

func (r *fieldReader) files(into *[]string) {
	if !r.present("files") {
		return
	}
	var files []string
	if err := json.Unmarshal(r.fields["files"], &files); err != nil {
		r.err = errors.New("files is not a list of names")
		return
	}
	if len(files) == 0 {
		r.err = errors.New("files is empty")
		return
	}
	var seen uint8
	for _, name := range files {
		bit := fileBits[name]
		switch {
		case bit == 0:
			r.err = fmt.Errorf("unknown file %q", name)
			return
		case seen&bit != 0:
			r.err = fmt.Errorf("files names the file %q twice", name)
			return
		}
		seen |= bit
	}
	*into = files
}

// ParseTargets reads the value of the setting. A row it refuses, and a
// second row of a name, is left out and named in the errors; the other rows
// are the targets. A missing value holds no targets.
func ParseTargets(value []byte) ([]Target, []error) {
	if len(bytes.TrimSpace(value)) == 0 {
		return nil, nil
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(value, &rows); err != nil {
		return nil, []error{fmt.Errorf("the setting %s is not a JSON list", SettingKey)}
	}
	var targets []Target
	var errs []error
	names := map[string]int{}
	for i, row := range rows {
		t, err := ParseTarget(row)
		if first, taken := names[t.Name]; err == nil && taken {
			err = fmt.Errorf("the name %s is taken by row %d", t.Name, first)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("row %d: %w", i+1, err))
			continue
		}
		names[t.Name] = i + 1
		targets = append(targets, t)
	}
	return targets, errs
}

// endpoint is the address with the path of the protocol of the kind when an
// http or https address has none.
func (t Target) endpoint() *url.URL {
	u, err := url.Parse(t.Address)
	if err != nil {
		return &url.URL{}
	}
	if rule := kindRules[t.Kind]; rule.path != nil && (u.Scheme == "http" || u.Scheme == "https") && (u.Path == "" || u.Path == "/") {
		u.Path, u.RawPath = rule.path(t), ""
	}
	return u
}

func (t Target) queueLines() int { return orDefault(t.QueueLines, defaultQueueLines) }
func (t Target) batchLines() int { return orDefault(t.BatchLines, defaultBatchLines) }

func (t Target) queueBytes() int64 {
	if t.QueueBytes == 0 {
		return defaultQueueBytes
	}
	return t.QueueBytes
}

func (t Target) flushInterval() time.Duration {
	return time.Duration(orDefault(t.FlushIntervalMS, defaultFlushMS)) * time.Millisecond
}

func (t Target) timeout() time.Duration {
	return time.Duration(orDefault(t.TimeoutMS, defaultTimeoutMS)) * time.Millisecond
}

func (t Target) index() string {
	if t.Index == "" {
		return defaultIndex
	}
	return t.Index
}

// fileMask is the bits of the files the target takes.
func (t Target) fileMask() uint8 {
	if len(t.Files) == 0 {
		return allFiles
	}
	var mask uint8
	for _, name := range t.Files {
		mask |= fileBits[name]
	}
	return mask
}

func orDefault(n, def int) int {
	if n == 0 {
		return def
	}
	return n
}
