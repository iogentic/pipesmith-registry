package logship

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// Every refusal of the validation, one row each; the error names the rule
// and never the address.
func TestParseTargetRefuses(t *testing.T) {
	cases := []struct{ row, want string }{
		{`"a string"`, "not a JSON object"},
		{`{"name":"a","kind":"http-json","address":"http://192.0.2.1","colour":"red"}`, `unknown key "colour"`},
		{`{"kind":"http-json","address":"http://192.0.2.1"}`, "name is missing"},
		{`{"name":"Upper","kind":"http-json","address":"http://192.0.2.1"}`, "name"},
		{`{"name":"a","address":"http://192.0.2.1"}`, "kind is missing"},
		{`{"name":"a","kind":"carrier-pigeon","address":"http://192.0.2.1"}`, `unknown kind "carrier-pigeon"`},
		{`{"name":"a","kind":"http-json"}`, "address is missing"},
		{`{"name":"a","kind":"http-json","address":"http:///path-only"}`, "address has no host"},
		{`{"name":"a","kind":"syslog","address":"http://192.0.2.1"}`, `kind syslog takes no scheme "http"`},
		{`{"name":"a","kind":"syslog","address":"udp://192.0.2.1"}`, "needs a port"},
		{`{"name":"a","kind":"syslog","address":"tcp://192.0.2.1:6514/x"}`, "no path"},
		{`{"name":"a","kind":"http-json","address":"http://demo-reader:pw@192.0.2.1"}`, "user info"},
		{`{"name":"a","kind":"http-json","address":"http://192.0.2.1:70000"}`, "port"},
		{`{"name":"a","kind":"http-json","address":"http://192.0.2.1","files":["server","debug"]}`, `unknown file "debug"`},
		{`{"name":"a","kind":"http-json","address":"http://192.0.2.1","files":[]}`, "files is empty"},
		{`{"name":"a","kind":"http-json","address":"http://192.0.2.1","files":["audit","audit"]}`, `file "audit" twice`},
		{`{"name":"a","kind":"http-json","address":"http://192.0.2.1","queue_lines":0}`, "queue_lines"},
		{`{"name":"a","kind":"http-json","address":"http://192.0.2.1","queue_lines":1000001}`, "queue_lines"},
		{`{"name":"a","kind":"http-json","address":"http://192.0.2.1","queue_bytes":65535}`, "queue_bytes"},
		{`{"name":"a","kind":"http-json","address":"http://192.0.2.1","queue_bytes":268435457}`, "queue_bytes"},
		{`{"name":"a","kind":"http-json","address":"http://192.0.2.1","batch_lines":0}`, "batch_lines"},
		{`{"name":"a","kind":"http-json","address":"http://192.0.2.1","batch_lines":10001}`, "batch_lines"},
		{`{"name":"a","kind":"http-json","address":"http://192.0.2.1","flush_interval_ms":99}`, "flush_interval_ms"},
		{`{"name":"a","kind":"http-json","address":"http://192.0.2.1","flush_interval_ms":60001}`, "flush_interval_ms"},
		{`{"name":"a","kind":"http-json","address":"http://192.0.2.1","timeout_ms":99}`, "timeout_ms"},
		{`{"name":"a","kind":"http-json","address":"http://192.0.2.1","timeout_ms":60001}`, "timeout_ms"},
		{`{"name":"a","kind":"http-json","address":"http://192.0.2.1","timeout_ms":1500.5}`, "timeout_ms is not a whole number"},
		{`{"name":"a","kind":"http-json","address":"https://192.0.2.1","ca_file":"certs/ca.pem"}`, "ca_file"},
		{`{"name":"a","kind":"http-json","address":"http://192.0.2.1","ca_file":"/etc/demo/ca.pem"}`, "ca_file needs https or tls"},
		{`{"name":"a","kind":"syslog","address":"tls://192.0.2.1:6514","secret":"log-target-a"}`, "takes no secret"},
		{`{"name":"a","kind":"http-json","address":"http://192.0.2.1","secret":"log-target-a"}`, "secret only over https"},
		{`{"name":"a","kind":"http-json","address":"https://192.0.2.1","secret":"../log-target-a"}`, "secret"},
		{`{"name":"a","kind":"http-json","address":"https://192.0.2.1","user":"demo-admin"}`, "user needs a secret"},
		{`{"name":"a","kind":"http-json","address":"https://192.0.2.1","user":"demo:admin","secret":"log-target-a"}`, "user"},
		{`{"name":"a","kind":"http-json","address":"https://192.0.2.1","index":"demo"}`, "index"},
		{`{"name":"a","kind":"elasticsearch","address":"https://192.0.2.1","index":"Demo Logs"}`, "index"},
	}
	for _, c := range cases {
		_, err := ParseTarget([]byte(c.row))
		if err == nil {
			t.Errorf("accepted %s", c.row)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q, want %q in it", c.row, err, c.want)
		}
		if strings.Contains(err.Error(), "192.0.2.1") || strings.Contains(err.Error(), "pw@") {
			t.Errorf("%s: the error %q names the address", c.row, err)
		}
	}
}

// A row with only name, kind and address takes the defaults of every
// number, all four files, and the path of the protocol of its kind.
func TestParseTargetDefaults(t *testing.T) {
	cases := []struct{ kind, address, endpoint string }{
		{"http-json", "http://192.0.2.1:8080", "http://192.0.2.1:8080"},
		{"http-json", "https://logs.example.com/ingest?source=demo", "https://logs.example.com/ingest?source=demo"},
		{"loki", "http://192.0.2.1:3100", "http://192.0.2.1:3100/loki/api/v1/push"},
		{"otlp", "http://192.0.2.1:4318/", "http://192.0.2.1:4318/v1/logs"},
		{"splunk-hec", "https://192.0.2.1:8088", "https://192.0.2.1:8088/services/collector/event"},
		{"elasticsearch", "https://192.0.2.1:9200", "https://192.0.2.1:9200/pipesmith-registry/_bulk"},
		{"elasticsearch", "https://192.0.2.1:9200/_bulk", "https://192.0.2.1:9200/_bulk"},
		{"gelf", "http://192.0.2.1:12201", "http://192.0.2.1:12201/gelf"},
		{"gelf", "udp://192.0.2.1:12201", "udp://192.0.2.1:12201"},
		{"syslog", "tls://192.0.2.1:6514", "tls://192.0.2.1:6514"},
	}
	for _, c := range cases {
		row := `{"name":"demo","kind":"` + c.kind + `","address":"` + c.address + `"}`
		target, err := ParseTarget([]byte(row))
		if err != nil {
			t.Fatalf("%s: %v", row, err)
		}
		if got := target.endpoint().String(); got != c.endpoint {
			t.Errorf("%s: endpoint %s, want %s", row, got, c.endpoint)
		}
		if target.queueLines() != 10000 || target.queueBytes() != 16<<20 || target.batchLines() != 500 ||
			target.flushInterval() != time.Second || target.timeout() != 5*time.Second || target.fileMask() != allFiles {
			t.Errorf("%s: defaults %+v", row, target)
		}
	}
}

// ParseTargets leaves out each row it refuses, a second row of a name among
// them, and names the row of each refusal.
func TestParseTargetsSkipsBadRows(t *testing.T) {
	targets, errs := ParseTargets([]byte(`[
		{"name":"a","kind":"http-json","address":"http://192.0.2.1"},
		{"name":"b","kind":"http-json"},
		{"name":"a","kind":"http-json","address":"http://192.0.2.2"},
		{"name":"c","kind":"loki","address":"http://192.0.2.3","files":["requests"]}]`))
	var names []string
	for _, target := range targets {
		names = append(names, target.Name)
	}
	if !slices.Equal(names, []string{"a", "c"}) || len(errs) != 2 {
		t.Fatalf("targets %v, errors %v", names, errs)
	}
	if !strings.Contains(errs[0].Error(), "row 2") || !strings.Contains(errs[1].Error(), "row 3") ||
		!strings.Contains(errs[1].Error(), "name a is taken by row 1") {
		t.Fatalf("errors %v", errs)
	}
	if _, errs := ParseTargets([]byte(`{"name":"a"}`)); len(errs) != 1 {
		t.Fatalf("a value that is not a list: %v", errs)
	}
	if targets, errs := ParseTargets(nil); len(targets) != 0 || len(errs) != 0 {
		t.Fatalf("no setting: %v %v", targets, errs)
	}
}
