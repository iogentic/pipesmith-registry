package logship

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// The rules of OTLP/HTTP with JSON, read on 2026-11-19. The page
// https://opentelemetry.io/docs/specs/otlp/, sections JSON Protobuf Encoding,
// OTLP/HTTP Request, OTLP/HTTP Response, Failures, Retryable Response Codes,
// Bad Data and OTLP/HTTP Throttling: POST to the default path /v1/logs; the
// client and the server set Content-Type application/json and the answer
// takes the Content-Type of the request; the keys are the field names in
// lowerCamelCase and the original names are not valid keys; a receiver
// ignores a key it does not know; enums are integers, never their names;
// 64 bit integers, timeUnixNano among them, are decimal strings, and a
// number is accepted when decoding; a client that gzips sends
// Content-Encoding gzip; the server limits the body, 64 MiB recommended,
// and answers 413 above it; success is 200 with an ExportLogsServiceResponse
// whose partial_success is unset; a partial success is 200 with
// rejected_log_records, and the client must not retry it; 400 is bad data,
// never retried; 429, 502, 503 and 504 are retried and every other 4xx and
// 5xx is not; 429 and 503 may carry Retry-After as seconds or an HTTP date;
// the client limits the answer body (4 MiB recommended) and treats a longer
// one as an error that is not retried. The proto files of
// github.com/open-telemetry/opentelemetry-proto, branch main
// (opentelemetry/proto/collector/logs/v1/logs_service.proto,
// .../logs/v1/logs.proto, .../common/v1/common.proto): the field names
// resource_logs, resource, scope_logs, scope, log_records, time_unix_nano,
// severity_number, severity_text, body, string_value, partial_success,
// rejected_log_records (int64), error_message; SEVERITY_NUMBER_DEBUG 5, INFO
// 9, WARN 13, ERROR 17. The choices of this server on top: one resource with
// service.name pipesmith-registry and service.instance.id the host, one scope
// per kind named by the kind, each record with timeUnixNano as a string,
// severityNumber, severityText the level and body the line as stringValue;
// no compression. unverified: the JSON form of the Status body of a failure
// (the page names a protobuf Status only; the stub answers a JSON object),
// the status for a wrong Content-Type (the stub answers 415), the answer of a
// gateway to a missing or wrong Authorization (the stub answers 401).

// otlpPath is the path of an export of logs.
const otlpPath = "/v1/logs"

// maxOTLPBody is the bound of a request body the page recommends.
const maxOTLPBody = 64 << 20

// otlpSeverity is the severityNumber of each level name of a line.
var otlpSeverity = map[string]int64{"DEBUG": 5, "INFO": 9, "WARN": 13, "ERROR": 17}

// otlpStub is a receiver of OTLP/HTTP in JSON. It reads every key by its
// exact name, ignores the keys it does not know as the page asks, takes the
// records of a request that keeps the rules of the page and of this server,
// and answers 200 with {} or with the body of answer; anything else it
// refuses with the status of the rule and a JSON Status object.
type otlpStub struct {
	strictWeb
	auth    string             // the Authorization it takes; empty takes none
	answer  func(n int) string // the body of a 200 to request n; nil or "" is {}
	records []otlpRecordTaken  // what it took
	batches map[int][]string   // the scopes of each request it took, in their order
}

// otlpRecordTaken is one log record the stub took.
type otlpRecordTaken struct {
	scope, severity string
	number          int64
	stamp           string
	line, id        string
	request         int
}

func newOTLPStub(t *testing.T, auth string, https bool) *otlpStub {
	t.Helper()
	s := &otlpStub{auth: auth, batches: map[int][]string{}}
	s.start(t, s, https)
	return s
}

func (s *otlpStub) setAnswer(answer func(n int) string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answer = answer
}

func (s *otlpStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := s.begin()
	records, scopes, code, reason := s.decode(r, n)
	var retryAfter string
	if code == 0 {
		if code, retryAfter = s.asked(n); code != 0 {
			reason = "the answer of the test"
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if code != 0 {
		s.note(reason)
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(code)
		status, _ := json.Marshal(map[string]any{"code": 3, "message": reason})
		_, _ = w.Write(status)
		return
	}
	s.mu.Lock()
	s.records = append(s.records, records...)
	s.batches[n] = scopes
	answer := s.answer
	s.mu.Unlock()
	body := "{}"
	if answer != nil && answer(n) != "" {
		body = answer(n)
	}
	_, _ = io.WriteString(w, body)
}

// decode applies the rules of OTLP/HTTP and of this server to request n; a
// code other than 0 refuses it.
func (s *otlpStub) decode(r *http.Request, n int) ([]otlpRecordTaken, []string, int, string) {
	if code, reason := webHead(r, otlpPath); code != 0 {
		return nil, nil, code, reason
	}
	if r.Header.Get("Authorization") != s.auth {
		return nil, nil, http.StatusUnauthorized, "authorization"
	}
	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/json" {
		return nil, nil, http.StatusUnsupportedMediaType, "content type " + r.Header.Get("Content-Type")
	}
	if enc := r.Header.Get("Content-Encoding"); enc != "" {
		return nil, nil, http.StatusBadRequest, "content encoding " + enc + ": this server sends no compression"
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxOTLPBody+1))
	switch {
	case err != nil:
		return nil, nil, http.StatusBadRequest, "the body cannot be read"
	case len(body) > maxOTLPBody:
		return nil, nil, http.StatusRequestEntityTooLarge, "the body is above 64 MiB"
	case !utf8.Valid(body) || !json.Valid(body):
		return nil, nil, http.StatusBadRequest, "the body is not JSON"
	}
	top, err := jsonFields(body)
	if err != nil {
		return nil, nil, http.StatusBadRequest, err.Error()
	}
	resources, err := jsonList(top, "resourceLogs")
	if err != nil {
		return nil, nil, http.StatusBadRequest, err.Error()
	}
	// The requests of this server.
	if len(resources) != 1 {
		return nil, nil, http.StatusBadRequest, fmt.Sprintf("%d resources, this server sends one", len(resources))
	}
	res, err := jsonFields(resources[0])
	if err != nil {
		return nil, nil, http.StatusBadRequest, "resourceLogs: " + err.Error()
	}
	if code, reason := checkOTLPResource(res); code != 0 {
		return nil, nil, code, reason
	}
	scopes, err := jsonList(res, "scopeLogs")
	if err != nil || len(scopes) == 0 {
		return nil, nil, http.StatusBadRequest, "no scopeLogs"
	}
	var out []otlpRecordTaken
	var names []string
	for _, raw := range scopes {
		records, name, code, reason := readOTLPScope(raw, n, names)
		if code != 0 {
			return nil, nil, code, reason
		}
		names = append(names, name)
		out = append(out, records...)
	}
	return out, names, 0, ""
}

// jsonList is the value of key in fields as a list; an absent key is an
// empty list, as a field left out is in protobuf.
func jsonList(fields map[string]json.RawMessage, key string) ([]json.RawMessage, error) {
	raw, ok := fields[key]
	if !ok {
		return nil, nil
	}
	var list []json.RawMessage
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &list) != nil {
		return nil, refusal(key + " is not a list")
	}
	return list, nil
}

// otlpAttributes reads a list of KeyValue whose values are each a
// stringValue.
func otlpAttributes(raw json.RawMessage) (map[string]string, error) {
	var list []json.RawMessage
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &list) != nil {
		return nil, refusal("attributes is not a list of KeyValue")
	}
	out := map[string]string{}
	for _, item := range list {
		kv, err := jsonFields(item)
		if err != nil {
			return nil, err
		}
		key, ok := jsonText(kv, "key")
		if !ok {
			return nil, refusal("a KeyValue without key")
		}
		value, ok := otlpString(kv["value"])
		if !ok {
			return nil, refusal("the value of " + key + " is not a stringValue")
		}
		if _, twice := out[key]; twice {
			return nil, refusal("the attribute " + key + " twice")
		}
		out[key] = value
	}
	return out, nil
}

// otlpString is an AnyValue that holds exactly one stringValue.
func otlpString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	fields, err := jsonFields(raw)
	if err != nil || len(fields) != 1 {
		return "", false
	}
	return jsonText(fields, "stringValue")
}

// checkOTLPResource checks the resource of this server.
func checkOTLPResource(res map[string]json.RawMessage) (int, string) {
	resource, err := jsonFields(res["resource"])
	if err != nil {
		return http.StatusBadRequest, "no resource"
	}
	attrs, err := otlpAttributes(resource["attributes"])
	if err != nil {
		return http.StatusBadRequest, "resource: " + err.Error()
	}
	want := map[string]string{"service.name": "pipesmith-registry", "service.instance.id": stubHost}
	if !maps.Equal(attrs, want) {
		return http.StatusBadRequest, fmt.Sprintf("the resource %v, not that of this server", attrs)
	}
	return 0, ""
}

// readOTLPScope reads one ScopeLogs of request n; before holds the scopes of the
// request before it.
func readOTLPScope(raw json.RawMessage, n int, before []string) ([]otlpRecordTaken, string, int, string) {
	fields, err := jsonFields(raw)
	if err != nil {
		return nil, "", http.StatusBadRequest, "scopeLogs: " + err.Error()
	}
	scope, err := jsonFields(fields["scope"])
	if err != nil {
		return nil, "", http.StatusBadRequest, "a scopeLogs without scope"
	}
	name, ok := jsonText(scope, "name")
	switch {
	case !ok || !lineKinds[name]:
		return nil, "", http.StatusBadRequest, fmt.Sprintf("the scope %q is not a kind of line", name)
	case slices.Contains(before, name):
		return nil, "", http.StatusBadRequest, "a second scope of the kind " + name
	}
	records, err := jsonList(fields, "logRecords")
	if err != nil || len(records) == 0 {
		return nil, "", http.StatusBadRequest, "the scope " + name + " has no logRecords"
	}
	var out []otlpRecordTaken
	for _, rec := range records {
		taken, code, reason := readOTLPRecord(rec, name, n)
		if code != 0 {
			return nil, "", code, reason
		}
		out = append(out, taken)
	}
	return out, name, 0, ""
}

// readOTLPRecord reads one LogRecord of the scope of kind.
func readOTLPRecord(raw json.RawMessage, kind string, n int) (otlpRecordTaken, int, string) {
	rec, err := jsonFields(raw)
	if err != nil {
		return otlpRecordTaken{}, http.StatusBadRequest, "logRecords: " + err.Error()
	}
	// The rules of the page: a 64 bit integer as a string or a number, an
	// enum as an integer.
	var stamp string
	if raw, ok := rec["timeUnixNano"]; ok {
		text, isText := textOf(raw)
		if !isText {
			text = string(raw)
		}
		if _, err := strconv.ParseUint(text, 10, 64); err != nil {
			return otlpRecordTaken{}, http.StatusBadRequest, "timeUnixNano is not a 64 bit integer"
		}
		if !isText {
			return otlpRecordTaken{}, http.StatusBadRequest, "timeUnixNano as a number, which this server does not send"
		}
		stamp = text
	}
	var number int64
	if raw, ok := rec["severityNumber"]; ok {
		if !jsonNumber(raw) || json.Unmarshal(raw, &number) != nil || number < 0 || number > 24 {
			return otlpRecordTaken{}, http.StatusBadRequest, "severityNumber is not an integer of the enum"
		}
	}
	// The records of this server.
	severity, _ := jsonText(rec, "severityText")
	line, ok := otlpString(rec["body"])
	switch {
	case stamp == "":
		return otlpRecordTaken{}, http.StatusBadRequest, "a record without timeUnixNano"
	case otlpSeverity[severity] == 0 || number != otlpSeverity[severity]:
		return otlpRecordTaken{}, http.StatusBadRequest, fmt.Sprintf("severityNumber %d with severityText %q", number, severity)
	case !ok:
		return otlpRecordTaken{}, http.StatusBadRequest, "a body that is not a stringValue"
	}
	head, at, ok := headOfLine(line)
	switch {
	case !ok:
		return otlpRecordTaken{}, http.StatusBadRequest, "a line that is not a JSON object with time, level, logger and msg by these names"
	case head["level"] != severity:
		return otlpRecordTaken{}, http.StatusBadRequest, fmt.Sprintf("a line of the level %v with severityText %s", head["level"], severity)
	case strconv.FormatInt(at.UnixNano(), 10) != stamp:
		return otlpRecordTaken{}, http.StatusBadRequest, "timeUnixNano " + stamp + " is not the time of its line"
	}
	id, _ := lineIDOf([]byte(line))
	return otlpRecordTaken{scope: kind, severity: severity, number: number, stamp: stamp, line: line, id: id, request: n},
		0, ""
}

func (s *otlpStub) taken() []otlpRecordTaken {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]otlpRecordTaken(nil), s.records...)
}

func (s *otlpStub) took() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

// The 1,000 hostile lines arrive over http and over https, the address
// without a path, as 1,000 records with the ids that were sent: each body
// the line byte for byte, in the scope of its kind, with timeUnixNano the
// time of the line and the severity of its level. The stub took two
// exports, one per batch of 500, each with one resource and one scope per
// kind.
func TestOTLPThousandHostileLines(t *testing.T) {
	lines := hostileLines(t)
	want := idSet(t, lines)
	sent := map[string]logs.Line{}
	for _, l := range lines {
		id, _ := lineIDOf(l.JSON)
		sent[id] = l
	}
	for _, variant := range []string{"http", "https"} {
		t.Run(variant, func(t *testing.T) {
			stub := newOTLPStub(t, "", variant == "https")
			m := shipThousand(t, "otlp", stub, lines, nil)
			got := stub.taken()
			if len(got) != len(lines) {
				t.Fatalf("%d records decoded, %d lines sent", len(got), len(lines))
			}
			ids := map[string]bool{}
			for _, e := range got {
				line, ok := sent[e.id]
				switch {
				case !ok:
					t.Fatalf("a record with the id %q, which was not sent", e.id)
				case e.line != string(line.JSON):
					t.Fatalf("%s arrived as %q, sent as %q", e.id, e.line, line.JSON)
				case e.scope != line.Kind || e.severity != levelNames[line.Level]:
					t.Fatalf("%s in the scope %s at %s for a %s line at %v", e.id, e.scope, e.severity, line.Kind, line.Level)
				case e.stamp != strconv.FormatInt(line.Time.UnixNano(), 10):
					t.Fatalf("%s: timeUnixNano %s for a line of %v", e.id, e.stamp, line.Time)
				}
				ids[e.id] = true
			}
			if len(ids) != len(want) {
				t.Fatalf("%d ids decoded, %d sent", len(ids), len(want))
			}
			st := statsOf(t, m, "demo")
			if st.Sent != 1000 || st.Failed != 0 || st.Rejected != 0 || st.Errors != 0 || st.Attempts != 2 || stub.count() != 2 {
				t.Fatalf("%+v in %d requests", st, stub.count())
			}
			stub.mu.Lock()
			scopes := fmt.Sprint(stub.batches)
			stub.mu.Unlock()
			t.Logf("%d records in %d requests, scopes %s", len(got), stub.count(), scopes)
		})
	}
}

// The token reaches the stub as Bearer, and with a user as HTTP Basic over
// https; neither secret is in a log line of the manager.
func TestOTLPSecretReachesOnlyTheStub(t *testing.T) {
	checkSecret(t, "otlp", "Bearer", otlpPath, func(t *testing.T, auth string, https bool) kindStub {
		return newOTLPStub(t, auth, https)
	})
}

// Each status rule once.
func TestOTLPRetryRules(t *testing.T) {
	checkStatusRules(t, "otlp", nil, func(t *testing.T) kindStub {
		return newOTLPStub(t, "", false)
	})
}

// A 200 with partialSuccess counts its rejectedLogRecords as rejected and the
// rest as sent, in one attempt, as a string and as a number; a
// partialSuccess with 0 and a message is a full success; a count above the
// batch rejects the batch and no more. An answer body above the bound of
// 64 KiB, and a 200 whose body is not JSON, fail the batch at once.
func TestOTLPPartialSuccess(t *testing.T) {
	cases := []struct {
		name, body               string
		sent, rejected, failed   int64
		attempts, errors, linesN int
	}{
		{"string", `{"partialSuccess":{"rejectedLogRecords":"2","errorMessage":"demo: 2 records refused"}}`, 1, 2, 0, 1, 0, 3},
		{"number", `{"partialSuccess":{"rejectedLogRecords":1}}`, 2, 1, 0, 1, 0, 3},
		{"warning", `{"partialSuccess":{"rejectedLogRecords":"0","errorMessage":"demo: a warning"}}`, 3, 0, 0, 1, 0, 3},
		{"empty", `{"partialSuccess":{}}`, 3, 0, 0, 1, 0, 3},
		{"unknown keys", `{"partialSuccess":{"rejectedLogRecords":"1","demoField":true},"demo":1}`, 2, 1, 0, 1, 0, 3},
		{"snake case", `{"partial_success":{"rejected_log_records":"2"}}`, 3, 0, 0, 1, 0, 3},
		{"above the batch", `{"partialSuccess":{"rejectedLogRecords":"9"}}`, 0, 3, 0, 1, 0, 3},
		{"too large", `{"partialSuccess":{"rejectedLogRecords":"1","errorMessage":"` + strings.Repeat("x", 70<<10) + `"}}`,
			0, 0, 3, 1, 1, 3},
		{"not JSON", `partial`, 0, 0, 3, 1, 1, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stub := newOTLPStub(t, "", false)
			stub.setAnswer(func(int) string { return c.body })
			var set setting
			set.put(t, map[string]any{"name": "demo", "kind": "otlp", "address": stub.url(), "batch_lines": c.linesN})
			var log testLog
			m := startManager(t, newFakeClock(), &set, &log)
			statsOf(t, m, "demo")
			shipAll(t, m, testLines("audit", 0, c.linesN))
			waitFor(t, func() bool {
				st := statsOf(t, m, "demo")
				return st.Sent+st.Rejected+st.Failed == int64(c.linesN)
			}, "the batch counted")
			st := statsOf(t, m, "demo")
			if st.Sent != c.sent || st.Rejected != c.rejected || st.Failed != c.failed || st.Attempts != int64(c.attempts) ||
				st.Errors != int64(c.errors) || stub.count() != 1 {
				t.Fatalf("%+v after %d requests", st, stub.count())
			}
			m.Close()
			if c.failed > 0 {
				if warns := log.records(t, "log target failing"); len(warns) != 1 || warns[0]["error"] != "answer" {
					t.Fatalf("warnings %v", warns)
				}
			}
		})
	}
}

// otlpText is an export of one record of line written by hand, with record
// as the fields of the record.
func otlpText(resource, scope, record string) string {
	return `{"resourceLogs":[{"resource":{"attributes":` + resource + `},"scopeLogs":[{"scope":{"name":"` + scope +
		`"},"logRecords":[{` + record + `}]}]}]}`
}

// otlpRecordText is the fields of the record of line written by hand.
func otlpRecordText(line logs.Line) string {
	quoted, _ := json.Marshal(string(line.JSON))
	return `"timeUnixNano":"` + strconv.FormatInt(line.Time.UnixNano(), 10) + `","severityNumber":9,"severityText":"INFO",` +
		`"body":{"stringValue":` + string(quoted) + `}`
}

// The stub refuses what OTLP/HTTP and this server do not allow, one bad
// request each, sent to it directly, each with the status of its rule and a
// reason that names it; it takes the good requests, one with keys it does
// not know among them.
func TestOTLPStubRefuses(t *testing.T) {
	stub := newOTLPStub(t, "Bearer demo-token", false)
	line := testLine("server", 1)
	resource := `[{"key":"service.name","value":{"stringValue":"pipesmith-registry"}},` +
		`{"key":"service.instance.id","value":{"stringValue":"demo-host"}}]`
	record := otlpRecordText(line)
	good := otlpText(resource, "server", record)
	other := func(from, to string) string { return strings.Replace(good, from, to, 1) }
	stamp := strconv.FormatInt(line.Time.UnixNano(), 10)
	headers := map[string]string{"Content-Type": "application/json", "User-Agent": "pipesmith-registry/" + testVersion,
		"Authorization": "Bearer demo-token"}
	cases := []directRequest{
		{name: "good", body: good, want: 200},
		{name: "unknown keys", body: other(`"severityText"`, `"demoField":[1,{"a":2}],"severityText"`), want: 200},
		{name: "a content type with a charset", header: map[string]string{"Content-Type": "application/json; charset=utf-8"},
			body: good, want: 200},
		{name: "method", method: "PUT", body: good, want: 405, reason: "method PUT"},
		{name: "path", path: "/v1/traces", body: good, want: 404, reason: "path"},
		{name: "content type", header: map[string]string{"Content-Type": "application/x-protobuf"}, body: good, want: 415, reason: "content type"},
		{name: "no content type", header: map[string]string{"Content-Type": ""}, body: good, want: 415, reason: "content type"},
		{name: "gzip", header: map[string]string{"Content-Encoding": "gzip"}, body: compress(t, "GZIP", good), want: 400, reason: "no compression"},
		{name: "user agent", header: map[string]string{"User-Agent": "demo-client/1.0"}, body: good, want: 400, reason: "user agent"},
		{name: "no secret", header: map[string]string{"Authorization": ""}, body: good, want: 401, reason: "authorization"},
		{name: "other secret", header: map[string]string{"Authorization": "Bearer other-token"}, body: good, want: 401, reason: "authorization"},
		{name: "not JSON", body: strings.TrimSuffix(good, "}"), want: 400, reason: "not JSON"},
		{name: "resourceLogs not a list", body: `{"resourceLogs":{}}`, want: 400, reason: "not a list"},
		{name: "snake case keys", body: other(`"resourceLogs"`, `"resource_logs"`), want: 400, reason: "0 resources"},
		{name: "a key in another case", body: other(`"logRecords"`, `"LogRecords"`), want: 400, reason: "no logRecords"},
		{name: "timeUnixNano as a number", body: other(`"`+stamp+`"`, stamp), want: 400, reason: "as a number"},
		{name: "timeUnixNano not an integer", body: other(`"`+stamp+`"`, `"soon"`), want: 400, reason: "64 bit integer"},
		{name: "severityNumber as its name", body: other(`"severityNumber":9`, `"severityNumber":"SEVERITY_NUMBER_INFO"`),
			want: 400, reason: "integer of the enum"},
		{name: "another resource", body: other(`"demo-host"`, `"other-host"`), want: 400, reason: "not that of this server"},
		{name: "two resources", body: `{"resourceLogs":[{},{}]}`, want: 400, reason: "2 resources"},
		{name: "a scope that is no kind", body: otlpText(resource, "demo", record), want: 400, reason: `"demo"`},
		{name: "two scopes of one kind", body: other(`}]}]}]}`, `}]},{"scope":{"name":"server"},"logRecords":[{`+record+`}]}]}]}`),
			want: 400, reason: "a second scope"},
		{name: "no timeUnixNano", body: other(`"timeUnixNano":"`+stamp+`",`, ""), want: 400, reason: "without timeUnixNano"},
		{name: "severity of another level", body: other(`"severityNumber":9`, `"severityNumber":13`), want: 400, reason: "severityNumber 13"},
		{name: "a body of another type", body: other(`{"stringValue":`, `{"intValue":"1","stringValue":`), want: 400, reason: "stringValue"},
		{name: "a line of another level", body: other(`"severityNumber":9,"severityText":"INFO"`, `"severityNumber":13,"severityText":"WARN"`),
			want: 400, reason: "level"},
		{name: "a time of another line", body: other(`"`+stamp+`"`, `"1"`), want: 400, reason: "not the time of its line"},
		{name: "the head of a line in another case", body: other(`\"msg\"`, `\"Msg\"`), want: 400, reason: "not a JSON object"},
	}
	wants := sendDirect(t, stub.url(), otlpPath, headers, cases)
	checkRefusals(t, stub.refusals(), wants)
	if got := stub.taken(); len(got) != 3 || got[0].id != "server-0001" || got[0].line != string(line.JSON) {
		t.Fatalf("the stub took %+v, want the three good records", got)
	}
}

// The answer to a refused request is a JSON object with the reason, under
// Content-Type application/json, as the stub of a JSON request answers.
func TestOTLPStubAnswersJSON(t *testing.T) {
	stub := newOTLPStub(t, "", false)
	resp, err := http.Post(stub.url()+otlpPath, "application/json", strings.NewReader("{"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 || resp.Header.Get("Content-Type") != "application/json" || !json.Valid(body) {
		t.Fatalf("%d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
}

// A request above the bound of the client, 64 MiB as the page recommends,
// is not made: the batch fails at once as request and the stub sees nothing.
// The test lowers the bound to 4 KiB.
func TestOTLPRequestAboveTheBoundIsNotMade(t *testing.T) {
	before := otlpMaxRequest
	otlpMaxRequest = 4 << 10
	t.Cleanup(func() { otlpMaxRequest = before })
	stub := newOTLPStub(t, "", false)
	var set setting
	set.put(t, map[string]any{"name": "demo", "kind": "otlp", "address": stub.url(), "batch_lines": 2})
	var log testLog
	m := startManager(t, newFakeClock(), &set, &log)
	statsOf(t, m, "demo")
	shipAll(t, m, []logs.Line{paddedLine("server", 1, 1<<10), paddedLine("server", 2, 4<<10)})
	waitFor(t, func() bool { return statsOf(t, m, "demo").Failed == 2 }, "the batch given up")
	if st := statsOf(t, m, "demo"); st.Attempts != 1 || st.Sent != 0 || stub.count() != 0 {
		t.Fatalf("%+v after %d requests", st, stub.count())
	}
	m.Close()
	if warns := log.records(t, "log target failing"); len(warns) != 1 || warns[0]["error"] != "request" {
		t.Fatalf("warnings %v", warns)
	}
}
