package logship

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// The rules of the bulk API, read on 2026-11-19. Elasticsearch,
// https://www.elastic.co/docs/api/doc/elasticsearch/operation/operation-bulk:
// POST or PUT to /_bulk or /<target>/_bulk; the body newline delimited JSON,
// an action line followed by a source line for create and index; "The final
// line of data must end with a newline character", which a carriage return
// may precede; Content-Type application/json or application/x-ndjson; no
// pretty print; data streams take only the action create; the answer 200
// with took, errors and items, one item per action in the order of the
// request, keyed by the action, each with status and, on a failure, error
// with type and reason; Authorization ApiKey <key> among the schemes. The
// page https://www.elastic.co/docs/manage-data/data-store/data-streams: a
// document of a data stream needs @timestamp. The page
// https://www.elastic.co/docs/deploy-manage/production-guidance/optimize-performance/indexing-speed:
// 429 TOO_MANY_REQUESTS for an overloaded cluster, pause and retry. OpenSearch,
// https://docs.opensearch.org/latest/api-reference/document-apis/bulk/: the
// same body, POST strongly recommended, Content-Type application/x-ndjson,
// "requires that the request body end with a newline character"; and
// https://docs.opensearch.org/latest/im-plugin/data-streams/: the action
// index is rejected for a data stream, create is taken. The choices of this
// server on top: POST to /<index>/_bulk, Content-Type application/x-ndjson,
// the action line {"create":{}}, the source the JSON object of the line with
// @timestamp, the time of the line with milliseconds, first; an item of 429
// is sent again within the attempts of the batch, every other item of 400 and
// above counts as rejected. unverified: the status and text of a refusal of
// the whole request (no final newline, a wrong action for a data stream, a
// pretty printed line: the stub answers 400 with an error object), the
// status for a wrong Content-Type (the stub answers 406), the answer to a
// missing or wrong Authorization (the stub answers 401), whether OpenSearch
// takes ApiKey, the item text es_rejected_execution_exception (the stub
// writes it as the type of an item of 429).

// bulkIndex is the index of a target without the key index.
const bulkIndex = "pipesmith-registry"

// maxBulkBody is the limit of http.max_content_length, 100mb.
const maxBulkBody = 100 << 20

// bulkStub is a receiver of the bulk API. It reads each line of a request by
// the exact names of its keys, takes the documents of a request that keeps
// the rules, and answers with one item per action; itemStatus gives an item
// another status, and a document of an item of 400 or above is not taken.
type bulkStub struct {
	strictWeb
	path       string
	auth       string                // the Authorization it takes; empty takes none
	itemStatus func(n, i int) int    // the status of item i of request n; 0 is 201
	reason     func(n, i int) string // the reason of an item error; empty is a short one
	docs       []bulkDoc
}

// bulkDoc is one document the stub took.
type bulkDoc struct {
	stamp   string
	source  map[string]any
	id      string
	request int
}

func newBulkStub(t *testing.T, index, auth string, https bool) *bulkStub {
	t.Helper()
	s := &bulkStub{path: "/" + index + "/_bulk", auth: auth}
	s.start(t, s, https)
	return s
}

func (s *bulkStub) setItems(status func(n, i int) int, reason func(n, i int) string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.itemStatus, s.reason = status, reason
}

// bulkError writes the answer of a refused request.
func bulkError(w http.ResponseWriter, status int, kind, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	data, _ := json.Marshal(map[string]any{"error": map[string]any{"type": kind, "reason": reason}, "status": status})
	_, _ = w.Write(data)
}

func (s *bulkStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := s.begin()
	docs, code, reason := s.decode(r, n)
	if code == 0 {
		var retryAfter string
		if code, retryAfter = s.asked(n); code != 0 {
			reason = "the answer of the test"
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
		}
	}
	if code != 0 {
		s.note(reason)
		bulkError(w, code, "illegal_argument_exception", reason)
		return
	}
	s.mu.Lock()
	itemStatus, itemReason := s.itemStatus, s.reason
	s.mu.Unlock()
	items := make([]any, len(docs))
	failed := false
	for i, d := range docs {
		status := 201
		if itemStatus != nil && itemStatus(n, i) != 0 {
			status = itemStatus(n, i)
		}
		item := map[string]any{"_index": strings.Split(s.path, "/")[1], "status": status}
		if status >= 400 {
			failed = true
			kind := map[int]string{429: "es_rejected_execution_exception", 409: "version_conflict_engine_exception"}[status]
			if kind == "" {
				kind = "document_parsing_exception"
			}
			text := "the item status of the test"
			if itemReason != nil && itemReason(n, i) != "" {
				text = itemReason(n, i)
			}
			item["error"] = map[string]any{"type": kind, "reason": text}
		} else {
			item["_id"], item["result"] = fmt.Sprintf("demo-%d-%d", n, i), "created"
			s.mu.Lock()
			s.docs = append(s.docs, d)
			s.mu.Unlock()
		}
		items[i] = map[string]any{"create": item}
	}
	w.Header().Set("Content-Type", "application/json")
	data, _ := json.Marshal(map[string]any{"took": 3, "errors": failed, "items": items})
	_, _ = w.Write(data)
}

// decode applies the rules of the bulk API and of this server to request n;
// a code other than 0 refuses it.
func (s *bulkStub) decode(r *http.Request, n int) ([]bulkDoc, int, string) {
	if code, reason := webHead(r, s.path); code != 0 {
		return nil, code, reason
	}
	if r.Header.Get("Authorization") != s.auth {
		return nil, http.StatusUnauthorized, "authorization"
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch {
	case err != nil || media != "application/json" && media != "application/x-ndjson":
		return nil, http.StatusNotAcceptable, "Content-Type header [" + r.Header.Get("Content-Type") + "] is not supported"
	case media != "application/x-ndjson":
		return nil, http.StatusBadRequest, "content type " + media + ", this server sends application/x-ndjson"
	}
	if enc := r.Header.Get("Content-Encoding"); enc != "" {
		return nil, http.StatusBadRequest, "content encoding " + enc + ": this server sends no compression"
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBulkBody+1))
	switch {
	case err != nil:
		return nil, http.StatusBadRequest, "the body cannot be read"
	case len(body) > maxBulkBody:
		return nil, http.StatusRequestEntityTooLarge, "the body is above 100mb"
	case len(body) == 0:
		return nil, http.StatusBadRequest, "request body is required"
	case body[len(body)-1] != '\n':
		return nil, http.StatusBadRequest, "The bulk request must be terminated by a newline"
	case !utf8.Valid(body):
		return nil, http.StatusBadRequest, "the body is not UTF-8"
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	var docs []bulkDoc
	for i := 0; i < len(lines); i += 2 {
		action := strings.TrimSuffix(lines[i], "\r")
		if i+1 >= len(lines) {
			return nil, http.StatusBadRequest, "an action of create without its source"
		}
		doc, code, reason := bulkPair(action, strings.TrimSuffix(lines[i+1], "\r"), n)
		if code != 0 {
			return nil, code, reason
		}
		docs = append(docs, doc)
	}
	return docs, 0, ""
}

// bulkPair reads an action line and its source line.
func bulkPair(action, source string, n int) (bulkDoc, int, string) {
	fields, err := jsonFields([]byte(action))
	if err != nil || len(fields) != 1 || !json.Valid([]byte(action)) {
		return bulkDoc{}, http.StatusBadRequest, "Malformed action/metadata line, expected one object with one action"
	}
	name := slices.Collect(maps.Keys(fields))[0]
	switch name {
	case "create":
	case "index", "update", "delete":
		return bulkDoc{}, http.StatusBadRequest, "the action " + name + ", this server sends create, the one action of a data stream"
	default:
		return bulkDoc{}, http.StatusBadRequest, "Unknown key for a START_OBJECT in [" + name + "]"
	}
	meta, err := jsonFields(fields[name])
	if err != nil || len(meta) != 0 {
		return bulkDoc{}, http.StatusBadRequest, "the metadata of the action is not {}, which this server sends"
	}
	if !json.Valid([]byte(source)) {
		return bulkDoc{}, http.StatusBadRequest, "the source is not one JSON object on one line"
	}
	var doc map[string]any
	if json.Unmarshal([]byte(source), &doc) != nil || doc == nil {
		return bulkDoc{}, http.StatusBadRequest, "the source is not a JSON object"
	}
	stamp, ok := doc["@timestamp"].(string)
	if !ok {
		return bulkDoc{}, http.StatusBadRequest, "a document without @timestamp, which a data stream needs"
	}
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return bulkDoc{}, http.StatusBadRequest, "@timestamp " + stamp + " is not a date"
	}
	// The documents of this server.
	head, lineAt, ok := headOfLine(source)
	switch {
	case !strings.HasPrefix(source, `{"@timestamp":`):
		return bulkDoc{}, http.StatusBadRequest, "@timestamp is not the first key"
	case !ok:
		return bulkDoc{}, http.StatusBadRequest, "a source without time, level, logger and msg by these names"
	case !at.Equal(lineAt) || stamp != lineAt.UTC().Format("2006-01-02T15:04:05.000Z"):
		return bulkDoc{}, http.StatusBadRequest, fmt.Sprintf("@timestamp %s is not the time %v of its line", stamp, head["time"])
	}
	delete(doc, "@timestamp")
	id, _ := lineIDOf([]byte(source))
	return bulkDoc{stamp: stamp, source: doc, id: id, request: n}, 0, ""
}

func (s *bulkStub) taken() []bulkDoc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bulkDoc(nil), s.docs...)
}

func (s *bulkStub) took() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.docs)
}

// The 1,000 hostile lines arrive over http and over https, the address
// without a path, and over http with the key index, as 1,000 documents with
// the ids that were sent: each the JSON object of its line with @timestamp,
// the time of the line, added. The stub took two requests, one per batch of
// 500.
func TestElasticsearchThousandHostileLines(t *testing.T) {
	lines := hostileLines(t)
	want := idSet(t, lines)
	sent := map[string]logs.Line{}
	for _, l := range lines {
		id, _ := lineIDOf(l.JSON)
		sent[id] = l
	}
	for _, variant := range []string{"http", "https", "index"} {
		t.Run(variant, func(t *testing.T) {
			index, row := bulkIndex, map[string]any(nil)
			if variant == "index" {
				index, row = "demo-logs", map[string]any{"index": "demo-logs"}
			}
			stub := newBulkStub(t, index, "", variant == "https")
			m := shipThousand(t, "elasticsearch", stub, lines, row)
			got := stub.taken()
			if len(got) != len(lines) {
				t.Fatalf("%d documents decoded, %d lines sent", len(got), len(lines))
			}
			ids := map[string]bool{}
			for _, d := range got {
				line, ok := sent[d.id]
				if !ok {
					t.Fatalf("a document with the id %q, which was not sent", d.id)
				}
				var object map[string]any
				if err := json.Unmarshal(line.JSON, &object); err != nil {
					t.Fatal(err)
				}
				switch {
				case !reflect.DeepEqual(d.source, object):
					t.Fatalf("%s arrived as %v, sent as %q", d.id, d.source, line.JSON)
				case d.stamp != line.Time.UTC().Format("2006-01-02T15:04:05.000Z"):
					t.Fatalf("%s: @timestamp %s for a line of %v", d.id, d.stamp, line.Time)
				}
				ids[d.id] = true
			}
			if len(ids) != len(want) {
				t.Fatalf("%d ids decoded, %d sent", len(ids), len(want))
			}
			st := statsOf(t, m, "demo")
			if st.Sent != 1000 || st.Failed != 0 || st.Rejected != 0 || st.Errors != 0 || st.Attempts != 2 || stub.count() != 2 {
				t.Fatalf("%+v in %d requests", st, stub.count())
			}
			t.Logf("%d documents in %d requests", len(got), stub.count())
		})
	}
}

// The key reaches the stub as ApiKey, and with a user as HTTP Basic over
// https; neither secret is in a log line of the manager.
func TestElasticsearchSecretReachesOnlyTheStub(t *testing.T) {
	checkSecret(t, "elasticsearch", "ApiKey", "/"+bulkIndex+"/_bulk", func(t *testing.T, auth string, https bool) kindStub {
		return newBulkStub(t, bulkIndex, auth, https)
	})
}

// Each status rule of the whole request once.
func TestElasticsearchRetryRules(t *testing.T) {
	checkStatusRules(t, "elasticsearch", nil, func(t *testing.T) kindStub {
		return newBulkStub(t, bulkIndex, "", false)
	})
}

// With errors true the items are read: of a batch of 4 with the items 201,
// 429, 400 and 409, the item of 429 is sent again alone after 500 ms and
// taken, the items of 400 and 409 count as rejected, and no attempt counts
// as an error. An item of 429 in all 3 attempts fails; an answer whose items
// do not match the batch fails the batch as answer.
func TestElasticsearchItemErrors(t *testing.T) {
	t.Run("mixed", func(t *testing.T) {
		stub := newBulkStub(t, bulkIndex, "", false)
		stub.setItems(func(n, i int) int {
			if n == 1 {
				return []int{0, 429, 400, 409}[i]
			}
			return 0
		}, nil)
		m, clk := startBulk(t, stub, 4)
		shipAll(t, m, testLines("server", 0, 4))
		clk.waitPending(t, 500*time.Millisecond)
		clk.Advance(500 * time.Millisecond)
		waitFor(t, func() bool { return statsOf(t, m, "demo").Sent == 2 }, "the item of 429 sent again")
		st := statsOf(t, m, "demo")
		ids := []string{}
		for _, d := range stub.taken() {
			ids = append(ids, fmt.Sprintf("%s in %d", d.id, d.request))
		}
		if st.Rejected != 2 || st.Failed != 0 || st.Attempts != 2 || st.Errors != 0 || stub.count() != 2 ||
			!slices.Equal(ids, []string{"server-0000 in 1", "server-0001 in 2"}) {
			t.Fatalf("%+v, taken %v", st, ids)
		}
	})
	t.Run("429 three times", func(t *testing.T) {
		stub := newBulkStub(t, bulkIndex, "", false)
		stub.setItems(func(n, i int) int {
			if i == 0 {
				return 429
			}
			return 0
		}, nil)
		m, clk := startBulk(t, stub, 2)
		shipAll(t, m, testLines("tasks", 0, 2))
		clk.waitPending(t, 500*time.Millisecond)
		clk.Advance(500 * time.Millisecond)
		clk.waitPending(t, 2*time.Second)
		clk.Advance(2 * time.Second)
		waitFor(t, func() bool { return statsOf(t, m, "demo").Failed == 1 }, "the item given up")
		if st := statsOf(t, m, "demo"); st.Sent != 1 || st.Rejected != 0 || st.Attempts != 3 || stub.count() != 3 {
			t.Fatalf("%+v after %d requests", st, stub.count())
		}
	})
	for _, c := range []struct{ name, body string }{
		{"fewer items", `{"took":1,"errors":true,"items":[{"create":{"status":201}}]}`},
		{"not JSON", `{"took":1,"errors":true,"items":[`},
		{"an item of two actions", `{"took":1,"errors":true,"items":[{"create":{"status":201},"index":{"status":201}},{"create":{"status":201}}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			stub := newBulkStub(t, bulkIndex, "", false)
			body := c.body
			stub.start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				stub.begin()
				_, _ = io.WriteString(w, body)
			}), false)
			m, _ := startBulk(t, stub, 2)
			shipAll(t, m, testLines("audit", 0, 2))
			waitFor(t, func() bool { return statsOf(t, m, "demo").Failed == 2 }, "the batch given up")
			if st := statsOf(t, m, "demo"); st.Sent != 0 || st.Attempts != 1 || st.Errors != 1 {
				t.Fatalf("%+v", st)
			}
		})
	}
}

// The answer of 500 items that each carry an error of 300 bytes, about 200
// KiB, is read to its end: the bound is 64 KiB plus 1 KiB per line of the
// batch (decision P7-23), and all 500 items count as rejected.
func TestElasticsearchAnswerOf500Items(t *testing.T) {
	stub := newBulkStub(t, bulkIndex, "", false)
	stub.setItems(func(int, int) int { return 400 }, func(int, int) string { return strings.Repeat("r", 300) })
	m, _ := startBulk(t, stub, 500)
	shipAll(t, m, testLines("requests", 0, 500))
	waitFor(t, func() bool { st := statsOf(t, m, "demo"); return st.Rejected+st.Failed == 500 }, "the batch counted")
	if st := statsOf(t, m, "demo"); st.Rejected != 500 || st.Failed != 0 || st.Attempts != 1 || st.Errors != 0 {
		t.Fatalf("%+v", st)
	}
}

// startBulk starts a manager on a fake clock with a target of the stub whose
// batches hold batch lines.
func startBulk(t *testing.T, stub *bulkStub, batch int) (*Manager, *fakeClock) {
	t.Helper()
	var set setting
	set.put(t, map[string]any{"name": "demo", "kind": "elasticsearch", "address": stub.url(), "batch_lines": batch})
	clk := newFakeClock()
	var log testLog
	m := startManager(t, clk, &set, &log)
	statsOf(t, m, "demo")
	return m, clk
}

// bulkText is the body of one document of line written by hand.
func bulkText(line logs.Line) string {
	return `{"create":{}}` + "\n" + `{"@timestamp":"` + line.Time.UTC().Format("2006-01-02T15:04:05.000Z") + `",` +
		string(line.JSON[1:]) + "\n"
}

// The stub refuses what the bulk API and this server do not allow, one bad
// request each, sent to it directly, each with the status of its rule and a
// reason that names it; it takes the good requests.
func TestElasticsearchStubRefuses(t *testing.T) {
	stub := newBulkStub(t, bulkIndex, "ApiKey demo-token", false)
	one, two := testLine("server", 1), testLine("tasks", 2)
	good := bulkText(one)
	other := func(from, to string) string { return strings.Replace(good, from, to, 1) }
	path := "/" + bulkIndex + "/_bulk"
	headers := map[string]string{"Content-Type": "application/x-ndjson", "User-Agent": "pipesmith-registry/" + testVersion,
		"Authorization": "ApiKey demo-token"}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(strings.Split(good, "\n")[1]), "", "  "); err != nil {
		t.Fatal(err)
	}
	cases := []directRequest{
		{name: "good", body: good, want: 200},
		{name: "two documents with carriage returns", body: strings.ReplaceAll(good+bulkText(two), "\n", "\r\n"), want: 200},
		{name: "method", method: "PUT", body: good, want: 405, reason: "method PUT"},
		{name: "path", path: "/_bulk", body: good, want: 404, reason: "path"},
		{name: "another index", path: "/demo-other/_bulk", body: good, want: 404, reason: "path"},
		{name: "content type", header: map[string]string{"Content-Type": "text/plain"}, body: good, want: 406, reason: "not supported"},
		{name: "content type json", header: map[string]string{"Content-Type": "application/json"}, body: good, want: 400, reason: "sends application/x-ndjson"},
		{name: "gzip", header: map[string]string{"Content-Encoding": "gzip"}, body: compress(t, "GZIP", good), want: 400, reason: "no compression"},
		{name: "user agent", header: map[string]string{"User-Agent": "demo-client/1.0"}, body: good, want: 400, reason: "user agent"},
		{name: "no key", header: map[string]string{"Authorization": ""}, body: good, want: 401, reason: "authorization"},
		{name: "the key as Bearer", header: map[string]string{"Authorization": "Bearer demo-token"}, body: good, want: 401, reason: "authorization"},
		{name: "no final newline", body: strings.TrimSuffix(good, "\n"), want: 400, reason: "terminated by a newline"},
		{name: "empty", body: "", want: 400, reason: "body is required"},
		{name: "no source", body: `{"create":{}}` + "\n", want: 400, reason: "without its source"},
		{name: "the action index", body: other(`{"create":{}}`, `{"index":{}}`), want: 400, reason: "the action index"},
		{name: "an unknown action", body: other(`{"create":{}}`, `{"demo":{}}`), want: 400, reason: "Unknown key"},
		{name: "an action in another case", body: other(`{"create":{}}`, `{"Create":{}}`), want: 400, reason: "Unknown key"},
		{name: "metadata", body: other(`{"create":{}}`, `{"create":{"_index":"demo-logs"}}`), want: 400, reason: "metadata"},
		{name: "a pretty printed source", body: `{"create":{}}` + "\n" + pretty.String() + "\n", want: 400, reason: "one JSON object on one line"},
		{name: "no @timestamp", body: other(`"@timestamp"`, `"timestamp"`), want: 400, reason: "without @timestamp"},
		{name: "@timestamp not a date", body: other(`"@timestamp":"`, `"@timestamp":"soon`), want: 400, reason: "not a date"},
		{name: "@timestamp of another time", body: other(`"@timestamp":"2026-11-19T10:00:00.001Z"`, `"@timestamp":"2026-11-19T10:00:00.002Z"`),
			want: 400, reason: "not the time"},
		{name: "@timestamp not first", body: `{"create":{}}` + "\n" + strings.TrimSuffix(string(one.JSON), "}") + `,"@timestamp":"2026-11-19T10:00:00.001Z"}` + "\n",
			want: 400, reason: "not the first key"},
		{name: "the head of a line in another case", body: other(`"msg"`, `"Msg"`), want: 400, reason: "without time, level, logger and msg"},
	}
	wants := sendDirect(t, stub.url(), path, headers, cases)
	checkRefusals(t, stub.refusals(), wants)
	var ids []string
	for _, d := range stub.taken() {
		ids = append(ids, d.id)
	}
	if !slices.Equal(ids, []string{"server-0001", "server-0001", "tasks-0002"}) {
		t.Fatalf("the stub took %v, want the good documents", ids)
	}
}
