package server

import (
	"encoding/json"
	"os"
	"os/user"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/index"
	idstate "github.com/iogentic/pipesmith-registry/internal/state"
	"github.com/iogentic/pipesmith-registry/internal/version"
)

const informationPath = "/service/rest/beta/system/information"

// The system information (ruling SY7, contract S34): sections of maps, the
// version, the schema numbers of the three files, the Go runtime, the start
// time and uptime, the stores with kind, availability and counts, and an
// empty update section; no path, host name, user, address or environment
// value.
func TestSystemInformation(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	r := a.send("GET", informationPath, "basic", asAdmin)
	status(t, r, 200)
	var info map[string]map[string]any
	if err := json.Unmarshal(r.body, &info); err != nil {
		t.Fatalf("not two levels of maps: %v: %s", err, r.body)
	}
	for _, section := range []string{"version", "schema", "runtime", "stores", "update"} {
		if _, ok := info[section]; !ok {
			t.Fatalf("no section %s: %s", section, r.body)
		}
	}
	if len(info) != 5 {
		t.Fatalf("%d sections: %s", len(info), r.body)
	}
	if info["version"]["version"] != version.Version || info["version"]["go"] != runtime.Version() {
		t.Fatalf("version %v", info["version"])
	}
	for file, want := range map[string]int{"state.sqlite": idstate.SchemaVersion, "index.sqlite": index.SchemaVersion,
		"catalog.sqlite": catalog.SchemaVersion} {
		if got, ok := info["schema"][file].(float64); !ok || int(got) != want {
			t.Errorf("schema of %s: %v, want %d", file, info["schema"][file], want)
		}
	}
	started, err := time.Parse(time.RFC3339, info["runtime"]["started"].(string))
	if err != nil || time.Since(started) > time.Minute || time.Since(started) < 0 {
		t.Fatalf("started %v: %v", info["runtime"]["started"], err)
	}
	if up, ok := info["runtime"]["uptimeSeconds"].(float64); !ok || up < 0 || up > 60 {
		t.Fatalf("uptime %v", info["runtime"]["uptimeSeconds"])
	}
	if len(info["update"]) != 0 {
		t.Fatalf("update %v", info["update"])
	}
	if len(info["stores"]) == 0 {
		t.Fatalf("no store: %s", r.body)
	}
	for name, v := range info["stores"] {
		store, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("store %s: %v", name, v)
		}
		for _, key := range []string{"kind", "available", "files", "bytes"} {
			if _, ok := store[key]; !ok {
				t.Errorf("store %s has no %s: %v", name, key, store)
			}
		}
		if len(store) != 4 {
			t.Errorf("store %s: %v", name, store)
		}
	}
	body := string(r.body)
	forbidden := []string{a.srv.rp.cfg.DataDir, "127.0.0.1", "localhost", a.base}
	if host, err := os.Hostname(); err == nil && host != "" {
		forbidden = append(forbidden, host)
	}
	if u, err := user.Current(); err == nil && len(u.Username) > 2 {
		forbidden = append(forbidden, u.Username)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		forbidden = append(forbidden, home)
	}
	for _, word := range forbidden {
		if word != "" && strings.Contains(body, word) {
			t.Errorf("the answer holds %q: %s", word, body)
		}
	}
	if strings.Contains(body, "/") && !strings.Contains(info["version"]["go"].(string), "/") {
		for _, v := range info["stores"] {
			if strings.Contains(v.(map[string]any)["kind"].(string), "/") {
				t.Errorf("a store kind holds a path: %v", v)
			}
		}
	}
	for who, want := range map[string]int{asReader: 403, asNobody: 403} {
		status(t, a.send("GET", informationPath, "basic", who), want)
	}
	status(t, a.send("GET", informationPath), 401)
}
