package logship

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"mime"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// The rules of the HTTP Event Collector, read on 2026-11-19 in the manual of
// Splunk Enterprise 9.4 at https://help.splunk.com/en/splunk-enterprise/:
// get-started/get-data-in/9.4/get-data-with-http-event-collector/
// format-events-for-http-event-collector (the token as Authorization: Splunk
// <token>, or HTTP Basic with the token as the password; the keys time,
// host, source, sourcetype, index, fields and event; time as epoch
// <sec>.<ms>, for example 1433188255.500; event a string, a number or an
// object; batches as JSON objects one after the other, and arrays, are both
// accepted), rest-api-reference/9.4/input-endpoints/input-endpoint-descriptions
// (POST services/collector/event; the answer {"text":"Success","code":0};
// the codes 0 to 16 with their HTTP statuses: 2 Token is required 401, 4
// Invalid token 403, 5 No data 400, 6 Invalid data format 400, 9 Server is
// busy 503, 12 Event field is required 400, 13 Event field cannot be blank
// 400; invalid-event-number, the zero based index of the first invalid
// event; "Events processed before the error are sent to indexers and all
// events after the first error are dropped") and get-data-with-http-event-
// collector/troubleshoot-http-event-collector (codes 26 and 27, 429, the
// queue at capacity). The rule of the statuses tried again is that of OTLP
// (429, 502, 503 and 504), as the design takes it for every HTTP kind. The
// choices of this server on top: one object per line with the keys time,
// host, source (the kind), sourcetype pipesmith:registry and event (the
// line as a JSON object), a line break between two objects, Content-Type
// application/json, no compression, no channel, no index, no fields.
// unverified: the Content-Type HEC expects (the pages name none; the stub
// takes application/json only and answers 415), the answer to an unknown
// key of an event and to a Content-Encoding (the stub answers 400 code 6),
// whether time may be a string in a real server (the stub takes the number
// this server sends), Retry-After on 429 and 503.

// splunkPath is the path of an event of the collector.
const splunkPath = "/services/collector/event"

// splunkSourcetype is the sourcetype of this server.
const splunkSourcetype = "pipesmith:registry"

// maxSplunkBody is the most bytes of a request the stub reads.
const maxSplunkBody = 16 << 20

// splunkCodes is the HEC code of an answer the stub gives for a status a
// test asks for.
var splunkCodes = map[int]int{400: 6, 401: 3, 403: 4, 429: 26, 500: 8, 503: 9}

// splunkStub is a receiver of the event endpoint of HEC. It reads the
// objects of a request one after the other, each key by its exact name,
// takes the events before the first one that breaks a rule as HEC does, and
// answers that one with the code of its rule and invalid-event-number.
type splunkStub struct {
	strictWeb
	auth   string          // the Authorization it takes; empty takes none
	badAt  func(n int) int // the index of an event of request n the stub treats as invalid; -1 for none
	events []splunkEvent
}

// splunkEvent is one event the stub took.
type splunkEvent struct {
	time            float64
	stamp           string // time as it came
	host, source    string
	event           map[string]any
	id              string
	request, number int // the request, from 1, and the event in it, from 0
}

func newSplunkStub(t *testing.T, auth string, https bool) *splunkStub {
	t.Helper()
	s := &splunkStub{auth: auth}
	s.start(t, s, https)
	return s
}

func (s *splunkStub) setBadAt(badAt func(n int) int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.badAt = badAt
}

// hecAnswer writes an answer of HEC.
func hecAnswer(w http.ResponseWriter, status, code int, text string, invalid int) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	body := map[string]any{"text": text, "code": code}
	if invalid >= 0 {
		body["invalid-event-number"] = invalid
	}
	data, _ := json.Marshal(body)
	_, _ = w.Write(data)
}

func (s *splunkStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := s.begin()
	if status, code, reason := s.head(r); status != 0 {
		s.note(reason)
		hecAnswer(w, status, code, reason, -1)
		return
	}
	events, status, code, reason, invalid := s.decode(r, n)
	s.mu.Lock()
	badAt := s.badAt
	s.mu.Unlock()
	if badAt != nil && badAt(n) >= 0 && (status == 0 || badAt(n) < invalid) {
		invalid = badAt(n)
		status, code, reason = http.StatusBadRequest, 6, "Invalid data format, as the test asks"
	}
	if status == 0 {
		var retryAfter string
		if status, retryAfter = s.asked(n); status != 0 {
			s.note("the answer of the test")
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			code, ok := splunkCodes[status]
			if !ok {
				code = 8
			}
			hecAnswer(w, status, code, "the answer of the test", -1)
			return
		}
	}
	// The events before the first invalid one are taken, as HEC does.
	if invalid >= 0 {
		events = events[:min(invalid, len(events))]
	}
	s.mu.Lock()
	s.events = append(s.events, events...)
	s.mu.Unlock()
	if status != 0 {
		s.note(reason)
		hecAnswer(w, status, code, reason, invalid)
		return
	}
	hecAnswer(w, http.StatusOK, 0, "Success", -1)
}

// head checks the head of a request: the rules every kind shares, the token
// and the media type of this server.
func (s *splunkStub) head(r *http.Request) (int, int, string) {
	if status, reason := webHead(r, splunkPath); status != 0 {
		return status, 6, reason
	}
	auth := r.Header.Get("Authorization")
	switch {
	case s.auth != "" && auth == "":
		return http.StatusUnauthorized, 2, "Token is required"
	case auth != s.auth:
		return http.StatusForbidden, 4, "Invalid token, the authorization " + auth
	}
	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/json" {
		return http.StatusUnsupportedMediaType, 6, "content type " + r.Header.Get("Content-Type")
	}
	if enc := r.Header.Get("Content-Encoding"); enc != "" {
		return http.StatusBadRequest, 6, "content encoding " + enc + ": this server sends no compression"
	}
	return 0, 0, ""
}

// decode reads the events of request n. A status other than 0 refuses the
// event at invalid; the events before it are in the list.
func (s *splunkStub) decode(r *http.Request, n int) (events []splunkEvent, status, code int, reason string, invalid int) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSplunkBody+1))
	switch {
	case err != nil || len(body) > maxSplunkBody:
		return nil, http.StatusRequestEntityTooLarge, 6, "the body cannot be read", 0
	case len(bytes.TrimSpace(body)) == 0:
		return nil, http.StatusBadRequest, 5, "No data", -1
	case !utf8.Valid(body):
		return nil, http.StatusBadRequest, 6, "Invalid data format: not UTF-8", 0
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	for i := 0; ; i++ {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			return events, 0, 0, "", -1
		}
		if err != nil {
			return events, http.StatusBadRequest, 6, "Invalid data format", i
		}
		if raw[0] == '[' {
			return events, http.StatusBadRequest, 6, "a JSON array, which this server does not send", i
		}
		e, code, reason := splunkRead(raw, n, i)
		if code != 0 {
			return events, http.StatusBadRequest, code, reason, i
		}
		if i > 0 && body[dec.InputOffset()-int64(len(raw))-1] != '\n' {
			return events, http.StatusBadRequest, 6, "two objects without a line break between them", i
		}
		events = append(events, e)
	}
}

// splunkRead reads event i of request n by the rules of HEC and of this
// server; a code other than 0 is the HEC code of the rule it breaks.
func splunkRead(raw json.RawMessage, n, i int) (splunkEvent, int, string) {
	fields, err := jsonFields(raw)
	if err != nil {
		return splunkEvent{}, 6, "Invalid data format: " + err.Error()
	}
	value, ok := fields["event"]
	switch {
	case !ok:
		return splunkEvent{}, 12, "Event field is required"
	case string(value) == `""` || string(value) == "null":
		return splunkEvent{}, 13, "Event field cannot be blank"
	}
	// The events of this server.
	keys := slices.Sorted(maps.Keys(fields))
	if !slices.Equal(keys, []string{"event", "host", "source", "sourcetype", "time"}) {
		return splunkEvent{}, 6, fmt.Sprintf("the keys %v, not those of this server", keys)
	}
	host, _ := jsonText(fields, "host")
	source, _ := jsonText(fields, "source")
	sourcetype, _ := jsonText(fields, "sourcetype")
	stamp := string(fields["time"])
	secs, err := strconv.ParseFloat(stamp, 64)
	dot := strings.IndexByte(stamp, '.')
	switch {
	case !jsonNumber(fields["time"]) || err != nil || dot < 1 || len(stamp)-dot-1 != 3:
		return splunkEvent{}, 6, "time " + stamp + " is not epoch seconds with three decimals"
	case host != stubHost || !lineKinds[source] || sourcetype != splunkSourcetype:
		return splunkEvent{}, 6, fmt.Sprintf("host %q, source %q, sourcetype %q, not those of this server", host, source, sourcetype)
	}
	var event map[string]any
	if value[0] != '{' || json.Unmarshal(value, &event) != nil || !headIn(event) {
		return splunkEvent{}, 6, "an event that is not a JSON object with time, level, logger and msg by these names"
	}
	id, _ := lineIDOf(value)
	return splunkEvent{time: secs, stamp: stamp, host: host, source: source, event: event, id: id, request: n, number: i}, 0, ""
}

func (s *splunkStub) taken() []splunkEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]splunkEvent(nil), s.events...)
}

func (s *splunkStub) took() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// splunkStamp is the time of a line as this server sends it: epoch seconds
// with three decimals.
func splunkStamp(line logs.Line) string {
	ms := line.Time.UnixMilli()
	return fmt.Sprintf("%d.%03d", ms/1000, ms%1000)
}

// The 1,000 hostile lines arrive over http and over https, the address
// without a path, as 1,000 events with the ids that were sent: each event
// the JSON object of its line, source its kind, time the time of the line in
// seconds with three decimals. The stub took two requests, one per batch of
// 500.
func TestSplunkHECThousandHostileLines(t *testing.T) {
	lines := hostileLines(t)
	want := idSet(t, lines)
	sent := map[string]logs.Line{}
	for _, l := range lines {
		id, _ := lineIDOf(l.JSON)
		sent[id] = l
	}
	for _, variant := range []string{"http", "https"} {
		t.Run(variant, func(t *testing.T) {
			stub := newSplunkStub(t, "", variant == "https")
			m := shipThousand(t, "splunk-hec", stub, lines, nil)
			got := stub.taken()
			if len(got) != len(lines) {
				t.Fatalf("%d events decoded, %d lines sent", len(got), len(lines))
			}
			ids := map[string]bool{}
			for _, e := range got {
				line, ok := sent[e.id]
				if !ok {
					t.Fatalf("an event with the id %q, which was not sent", e.id)
				}
				var object map[string]any
				if err := json.Unmarshal(line.JSON, &object); err != nil {
					t.Fatal(err)
				}
				switch {
				case !reflect.DeepEqual(e.event, object):
					t.Fatalf("%s arrived as %v, sent as %q", e.id, e.event, line.JSON)
				case e.source != line.Kind:
					t.Fatalf("%s with the source %s for a %s line", e.id, e.source, line.Kind)
				case e.stamp != splunkStamp(line) || math.Abs(e.time-float64(line.Time.UnixMilli())/1000) > 0.0005:
					t.Fatalf("%s: the time %s for a line of %v", e.id, e.stamp, line.Time)
				}
				ids[e.id] = true
			}
			if len(ids) != len(want) {
				t.Fatalf("%d ids decoded, %d sent", len(ids), len(want))
			}
			st := statsOf(t, m, "demo")
			if st.Sent != 1000 || st.Failed != 0 || st.Errors != 0 || st.Attempts != 2 || stub.count() != 2 {
				t.Fatalf("%+v in %d requests", st, stub.count())
			}
			t.Logf("%d events in %d requests", len(got), stub.count())
		})
	}
}

// The token reaches the stub as Splunk, and with a user as HTTP Basic over
// https; neither secret is in a log line of the manager.
func TestSplunkHECSecretReachesOnlyTheStub(t *testing.T) {
	checkSecret(t, "splunk-hec", "Splunk", splunkPath, func(t *testing.T, auth string, https bool) kindStub {
		return newSplunkStub(t, auth, https)
	})
}

// Each status rule once.
func TestSplunkHECRetryRules(t *testing.T) {
	checkStatusRules(t, "splunk-hec", nil, func(t *testing.T) kindStub {
		return newSplunkStub(t, "", false)
	})
}

// A 400 with invalid-event-number k: HEC took the events before k, which
// count as sent; the event k and those after it are dropped at once, in one
// attempt. A number outside the batch, or none, drops the whole batch.
func TestSplunkHECInvalidEventNumber(t *testing.T) {
	cases := []struct {
		name               string
		badAt              int
		body               string // a 400 answer of the test in place of the stub's own
		sent, failed, took int64
	}{
		{"first", 0, "", 0, 4, 0},
		{"third", 2, "", 2, 2, 2},
		{"last", 3, "", 3, 1, 3},
		{"outside", -1, `{"text":"Invalid data format","code":6,"invalid-event-number":9}`, 0, 4, 0},
		{"none", -1, `{"text":"Incorrect index","code":7}`, 0, 4, 0},
		{"not JSON", -1, `bad request`, 0, 4, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stub := newSplunkStub(t, "", false)
			if c.body == "" {
				stub.setBadAt(func(int) int { return c.badAt })
			} else {
				body := c.body
				stub.start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					stub.begin()
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, body)
				}), false)
			}
			var set setting
			set.put(t, map[string]any{"name": "demo", "kind": "splunk-hec", "address": stub.url(), "batch_lines": 4})
			var log testLog
			m := startManager(t, newFakeClock(), &set, &log)
			statsOf(t, m, "demo")
			shipAll(t, m, testLines("requests", 0, 4))
			waitFor(t, func() bool { st := statsOf(t, m, "demo"); return st.Sent+st.Failed == 4 }, "the batch counted")
			st := statsOf(t, m, "demo")
			if st.Sent != c.sent || st.Failed != c.failed || st.Attempts != 1 || st.Errors != 1 || stub.count() != 1 ||
				int64(stub.took()) != c.took {
				t.Fatalf("%+v after %d requests, %d events taken", st, stub.count(), stub.took())
			}
			for i, e := range stub.taken() {
				if e.id != fmt.Sprintf("requests-%04d", i) {
					t.Fatalf("event %d is %s", i, e.id)
				}
			}
		})
	}
}

// splunkText is one event of line written by hand, with the keys in the
// order this server writes them.
func splunkText(line logs.Line) string {
	return `{"time":` + splunkStamp(line) + `,"host":"demo-host","source":"` + line.Kind +
		`","sourcetype":"pipesmith:registry","event":` + string(line.JSON) + `}`
}

// The stub refuses what HEC and this server do not allow, one bad request
// each, sent to it directly, each with the status and the code of its rule
// and a reason that names it; it takes the good requests and, of a request
// with a bad event, the events before it.
func TestSplunkHECStubRefuses(t *testing.T) {
	stub := newSplunkStub(t, "Splunk demo-token", false)
	one, two := testLine("server", 1), testLine("audit", 2)
	good := splunkText(one)
	other := func(from, to string) string { return strings.Replace(good, from, to, 1) }
	headers := map[string]string{"Content-Type": "application/json", "User-Agent": "pipesmith-registry/" + testVersion,
		"Authorization": "Splunk demo-token"}
	cases := []directRequest{
		{name: "good", body: good, want: 200},
		{name: "two events", body: good + "\n" + splunkText(two), want: 200},
		{name: "method", method: "PUT", body: good, want: 405, reason: "method PUT"},
		{name: "path", path: "/services/collector/raw", body: good, want: 404, reason: "path"},
		{name: "content type", header: map[string]string{"Content-Type": "text/plain"}, body: good, want: 415, reason: "content type"},
		{name: "gzip", header: map[string]string{"Content-Encoding": "gzip"}, body: compress(t, "GZIP", good), want: 400, reason: "no compression"},
		{name: "user agent", header: map[string]string{"User-Agent": "demo-client/1.0"}, body: good, want: 400, reason: "user agent"},
		{name: "no token", header: map[string]string{"Authorization": ""}, body: good, want: 401, reason: "Token is required"},
		{name: "other token", header: map[string]string{"Authorization": "Splunk other-token"}, body: good, want: 403, reason: "Invalid token"},
		{name: "the token as Bearer", header: map[string]string{"Authorization": "Bearer demo-token"}, body: good, want: 403, reason: "Invalid token"},
		{name: "no data", body: " \n", want: 400, reason: "No data"},
		{name: "not JSON", body: strings.TrimSuffix(good, "}"), want: 400, reason: "Invalid data format"},
		{name: "no event", body: `{"time":1,"host":"demo-host"}`, want: 400, reason: "Event field is required"},
		{name: "a blank event", body: other(`"event":`+string(one.JSON), `"event":""`), want: 400, reason: "cannot be blank"},
		{name: "an array", body: "[" + good + "]", want: 400, reason: "JSON array"},
		{name: "no line break", body: good + splunkText(two), want: 400, reason: "without a line break"},
		{name: "a key in another case", body: other(`"sourcetype"`, `"Sourcetype"`), want: 400, reason: "not those of this server"},
		{name: "an index", body: other(`"host"`, `"index":"main","host"`), want: 400, reason: "not those of this server"},
		{name: "time with two decimals", body: other(splunkStamp(one), splunkStamp(one)[:len(splunkStamp(one))-1]), want: 400, reason: "three decimals"},
		{name: "time as a string", body: other(splunkStamp(one), `"`+splunkStamp(one)+`"`), want: 400, reason: "three decimals"},
		{name: "another sourcetype", body: other(splunkSourcetype, "_json"), want: 400, reason: "not those of this server"},
		{name: "a source that is no kind", body: other(`"source":"server"`, `"source":"demo"`), want: 400, reason: "not those of this server"},
		{name: "the event as a string", body: other(`"event":`+string(one.JSON), `"event":"plain text"`), want: 400, reason: "not a JSON object"},
		{name: "the head of a line in another case", body: other(`"msg"`, `"Msg"`), want: 400, reason: "not a JSON object"},
		{name: "a bad second event", body: splunkText(two) + "\n" + `{"host":"demo-host"}`, want: 400, reason: "Event field is required"},
	}
	wants := sendDirect(t, stub.url(), splunkPath, headers, cases)
	checkRefusals(t, stub.refusals(), wants)
	var ids []string
	for _, e := range stub.taken() {
		ids = append(ids, e.id)
	}
	// good, two events, the first event before the missing line break, the
	// first event before the bad second
	if !slices.Equal(ids, []string{"server-0001", "server-0001", "audit-0002", "server-0001", "audit-0002"}) {
		t.Fatalf("the stub took %v, want the good events and the event before each bad one", ids)
	}
}

// A refused request is answered with the body of HEC: text, code and, for
// an event, invalid-event-number.
func TestSplunkHECStubAnswersAsHEC(t *testing.T) {
	stub := newSplunkStub(t, "", false)
	req, err := http.NewRequest(http.MethodPost, stub.url()+splunkPath, strings.NewReader(splunkText(testLine("tasks", 1))+"\n{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "pipesmith-registry/"+testVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var answer map[string]any
	if resp.StatusCode != 400 || json.Unmarshal(body, &answer) != nil || answer["code"] != float64(12) ||
		answer["invalid-event-number"] != float64(1) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}
