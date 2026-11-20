package channel

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/release"
	"github.com/iogentic/pipesmith-registry/tools/forgestub/forge"
)

// trickle answers the file with its full Content-Length and sends one byte
// per second, at most eight bytes.
func trickle(body []byte) func(w http.ResponseWriter, req *http.Request, _ http.Handler) {
	return func(w http.ResponseWriter, req *http.Request, _ http.Handler) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 8 && i < len(body); i++ {
			w.Write(body[i : i+1])
			w.(http.Flusher).Flush()
			select {
			case <-req.Context().Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

// manyVersions lists n versions 1.<i>.0 above the running 0.1.0, none of
// them complete.
func manyVersions(n int) []forge.Entry {
	out := make([]forge.Entry, n)
	for i := range out {
		out[i] = forge.Entry{Type: "generic", Name: Package, Version: fmt.Sprintf("v1.%d.0", i)}
	}
	return out
}

// A forge that sends one byte per second ends the stage at its deadline.
func TestStageDeadlineSlowForge(t *testing.T) {
	r := newRig(t)
	files := r.publish(published{version: "0.2.0"})
	r.wrap = onFile(platformBinary(), trickle(files[platformBinary()]))
	r.start(nil, map[string]any{"timeouts": map[string]any{"stage_seconds": 1}})
	start := time.Now()
	err := r.refused(ErrDeadline)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("took %v", took)
	}
	if !strings.Contains(err.Error(), "timeouts.stage_seconds") || !strings.Contains(err.Error(), platformBinary()) {
		t.Fatalf("message %v", err)
	}
}

// A forge that lists a thousand versions and answers each slowly ends the
// stage at its deadline.
func TestStageDeadlineManyVersions(t *testing.T) {
	r := newRig(t)
	r.extra = manyVersions(1000)
	r.wrap = onFile(release.PublicKeyFile, func(w http.ResponseWriter, req *http.Request, next http.Handler) {
		select {
		case <-req.Context().Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
		next.ServeHTTP(w, req)
	})
	r.start(nil, map[string]any{"timeouts": map[string]any{"stage_seconds": 1}, "limits": map[string]any{"candidates": 1000}})
	start := time.Now()
	r.refused(ErrDeadline)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("took %v", took)
	}
}

// The stage checks at most limits.candidates versions for completeness.
func TestCandidateCap(t *testing.T) {
	r := newRig(t)
	r.extra = manyVersions(1000)
	r.start()
	err := r.refused(ErrList)
	if !strings.Contains(err.Error(), "limits.candidates") {
		t.Fatalf("message %v", err)
	}
	if n := len(r.requested()); n != 10 {
		t.Fatalf("%d requests of a file, want 10", n)
	}
}

func TestStageSettings(t *testing.T) {
	r := newRig(t)
	r.start()
	cfg, err := release.LoadUpdateConfig(r.root)
	if err != nil {
		t.Fatal(err)
	}
	set, err := ParseSettings(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if set.Stage != time.Hour || set.Candidates != 10 {
		t.Fatalf("defaults %v %d", set.Stage, set.Candidates)
	}
	r.start(nil, map[string]any{"timeouts": map[string]any{"stage_seconds": 60}, "limits": map[string]any{"candidates": 1000}})
	cfg, _ = release.LoadUpdateConfig(r.root)
	if set, err = ParseSettings(cfg); err != nil || set.Stage != time.Minute || set.Candidates != 1000 {
		t.Fatalf("%+v %v", set, err)
	}
	for name, more := range map[string]map[string]any{
		"no candidate":     {"limits": map[string]any{"candidates": 0}},
		"many candidates":  {"limits": map[string]any{"candidates": 1001}},
		"stage over a day": {"timeouts": map[string]any{"stage_seconds": 86401}},
	} {
		r.start(nil, more)
		cfg, _ := release.LoadUpdateConfig(r.root)
		if _, err := ParseSettings(cfg); !errors.Is(err, ErrSettings) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
