package channel

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/release"
)

// The trusted key is read once, when the stage starts: a key file replaced
// during the downloads decides nothing (C-2).
func TestKeyReplacedDuringTheDownload(t *testing.T) {
	r := newRig(t)
	other := newOther(t)
	r.publish(published{version: "0.2.0", signer: other})
	r.wrap = onFile(release.SumsFile, func(w http.ResponseWriter, req *http.Request, next http.Handler) {
		if err := os.WriteFile(r.trusted.Pub, other.PEM, 0o644); err != nil {
			t.Error(err)
		}
		next.ServeHTTP(w, req)
	})
	r.start()
	r.refused(release.ErrSignature)
}

// The copy of the key lives below releases/ while the stage runs and is
// gone after it; a copy that a killed fetch left is removed.
func TestKeyCopyIsRemoved(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	left := filepath.Join(r.root, release.ReleasesDir, ".fetch-key-1234")
	if err := os.WriteFile(left, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	seen := false
	r.wrap = onFile(release.SumsFile, func(w http.ResponseWriter, req *http.Request, next http.Handler) {
		entries, _ := os.ReadDir(filepath.Join(r.root, release.ReleasesDir))
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".fetch-key-") && e.Name() != ".fetch-key-1234" {
				seen = true
			}
		}
		next.ServeHTTP(w, req)
	})
	r.start()
	r.fetched("0.2.0")
	if !seen {
		t.Error("no copy of the key below releases/ during the download")
	}
	for _, name := range r.releases() {
		if strings.HasPrefix(name, ".fetch-key-") {
			t.Fatalf("%s stayed", name)
		}
	}
}
