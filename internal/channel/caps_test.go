package channel

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/release"
)

// The caps of SHA256SUMS and of the bundle go up to the size the release
// check accepts and no further (C-9).
func TestCapsOfTheCheckedFiles(t *testing.T) {
	r := newRig(t)
	for _, key := range []string{"sums_bytes", "bundle_bytes"} {
		r.start(nil, map[string]any{"limits": map[string]any{key: 1 << 20}})
		cfg, _ := release.LoadUpdateConfig(r.root)
		if _, err := ParseSettings(cfg); err != nil {
			t.Errorf("%s of 1 MiB: %v", key, err)
		}
		r.start(nil, map[string]any{"limits": map[string]any{key: 1<<20 + 1}})
		cfg, _ = release.LoadUpdateConfig(r.root)
		if _, err := ParseSettings(cfg); !errors.Is(err, ErrSettings) || !strings.Contains(err.Error(), key) {
			t.Errorf("%s above 1 MiB: %v", key, err)
		}
	}
}

// checkedCap is the size release.ParseSums accepts.
func TestCheckedCapIsTheOneOfTheCheck(t *testing.T) {
	_, err := release.ParseSums(bytes.Repeat([]byte("x"), checkedCap+1))
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("one byte above: %v", err)
	}
	_, err = release.ParseSums(bytes.Repeat([]byte("x"), checkedCap))
	if err != nil && strings.Contains(err.Error(), "larger than") {
		t.Fatalf("at the cap: %v", err)
	}
}
