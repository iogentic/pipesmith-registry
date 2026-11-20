package logship

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// SendTest sends one server line with the message "log-target test" through
// the adapter of the target, with its secret, and names the class of a
// failure.
func TestSendTestSendsOneLine(t *testing.T) {
	stub := newWebStub(t, nil)
	target := Target{Name: "demo", Kind: kindHTTPJSON, Address: stub.url(), Secret: "log-target-demo"}
	if err := SendTest(target, "demo-token-7", "0.2.0"); err != nil {
		t.Fatalf("SendTest: %v", err)
	}
	got := stub.requests()
	if len(got) != 1 || got[0].header.Get("Authorization") != "Bearer demo-token-7" {
		t.Fatalf("requests %+v", got)
	}
	lines := stub.lines()
	if len(lines) != 1 {
		t.Fatalf("lines %q", lines)
	}
	var head map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &head); err != nil {
		t.Fatal(err)
	}
	if head["msg"] != "log-target test" || head["level"] != "INFO" || head["logger"] != "pipesmith_registry.log_target" {
		t.Fatalf("line %q", lines[0])
	}
}

func TestSendTestNamesTheFailure(t *testing.T) {
	stub := newWebStub(t, statuses(http.StatusBadRequest))
	err := SendTest(Target{Name: "demo", Kind: kindHTTPJSON, Address: stub.url()}, "", "0.2.0")
	if err == nil || err.Error() != "status 400" {
		t.Fatalf("error %v, want status 400", err)
	}
	silent := newSilentStub(t)
	start := time.Now()
	err = SendTest(Target{Name: "demo", Kind: kindHTTPJSON, Address: "http://" + silent.addr(), TimeoutMS: 300}, "", "0.2.0")
	if err == nil || err.Error() != "timeout" || time.Since(start) > 3*time.Second {
		t.Fatalf("error %v after %v, want timeout within the timeout of the target", err, time.Since(start))
	}
}
