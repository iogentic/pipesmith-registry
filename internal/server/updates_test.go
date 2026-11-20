package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

// installLayout is an invented install root in a temp dir: releases/0.2.0/
// with a binary, the link current, trusted.pub and update.json that names
// it. It returns the root and the fingerprint of the key in hex and base64.
func installLayout(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	rel := filepath.Join(root, "releases", "0.2.0")
	if err := os.MkdirAll(rel, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rel, "pipesmith-registry"), []byte("binary"), 0o755); err != nil { // #nosec G306 -- an invented binary
		t.Fatal(err)
	}
	if err := os.Symlink("releases/0.2.0", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "trusted.pub"), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644)
	writeFile(t, filepath.Join(root, "update.json"), []byte(`{"trusted_key": "trusted.pub", "data_dir": "/var/lib/pipesmith-registry"}`), 0o644)
	sum := sha256.Sum256(der)
	return root, hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString(sum[:])
}

func writeFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // the umask of the test leaves group and others out
		t.Fatal(err)
	}
}

// modeRule stands in for the owner and mode rule of internal/release in
// these tests: it refuses a key that group or others can write, with the
// words of that rule.
func modeRule(root, key string) error {
	if key == "" {
		return nil
	}
	info, err := os.Stat(key)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("owner or mode: %s has mode %v; neither group nor others may write it; chmod go-w %s", key, info.Mode(), key)
	}
	return nil
}

// startUpdates starts a server with security on whose install root is root
// ("" for a server outside any install root) and whose trust rule is rule.
func startUpdates(t *testing.T, root string, rule func(root, key string) error) *secureApp {
	t.Helper()
	sec := newFakeSecurity()
	base := t.TempDir()
	stores := []config.StoreConfig{blobStore(t, base, "store-a", spec("demo-raw", "docs/readme.txt", []byte("readme"), "text/plain"))}
	opts := appOptions{defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}, security: sec,
		installRoot: func() (string, string) { return root, filepath.Join(base, "bin", "pipesmith-registry") }, trustRule: rule}
	return &secureApp{app: startApp(t, base, prepare(t, base, stores, opts), opts), sec: sec}
}

func updatesOf(t *testing.T, a *secureApp, who string) map[string]any {
	t.Helper()
	r := a.send("GET", "/api/updates", "basic", who)
	status(t, r, 200)
	if r.header.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type %q", r.header.Get("Content-Type"))
	}
	return r.object(t)
}

func answerField(t *testing.T, m map[string]any, path string) any {
	t.Helper()
	var v any = m
	for _, part := range strings.Split(path, ".") {
		obj, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("%s: %v is no object", path, v)
		}
		v = obj[part]
	}
	return v
}

func TestUpdatesNeedsTheStatusPermission(t *testing.T) {
	root, _, _ := installLayout(t)
	a := startUpdates(t, root, modeRule)
	status(t, a.send("GET", "/api/updates"), 401)
	r := a.send("GET", "/api/updates", "basic", asNobody)
	status(t, r, 403)
	if strings.Contains(string(r.body), root) {
		t.Fatalf("the refusal names the root: %q", r.body)
	}
	a.sec.addBasic("atlas:pw", subject("atlas", "", "nexus:atlas:read"))
	if got := updatesOf(t, a, "atlas:pw"); got["installed"] != true {
		t.Fatalf("with nexus:atlas:read: %v", got)
	}
	status(t, a.send("POST", "/api/updates", "basic", asAdmin), 405)
	status(t, a.send("PUT", "/api/updates", "basic", asAdmin), 405)
}

func TestUpdatesShowsAGoodKey(t *testing.T) {
	root, hexSum, b64Sum := installLayout(t)
	got := updatesOf(t, startUpdates(t, root, modeRule), asAdmin)
	want := map[string]any{
		"installed":    true,
		"root":         root,
		"config.path":  filepath.Join(root, "update.json"),
		"config.found": true,
		"key.path":     filepath.Join(root, "trusted.pub"),
		"key.found":    true,
		"key.state":    "good",
		"key.sha256":   hexSum,
		"key.hint":     b64Sum,
		"check.ran":    true,
		"check.passed": true,
		"release.name": "0.2.0",
		"release.from": "current",
	}
	for path, value := range want {
		if v := answerField(t, got, path); v != value {
			t.Errorf("%s: %v, want %v", path, v, value)
		}
	}
	if answerField(t, got, "key.problem") != "" {
		t.Errorf("a good key with a problem: %v", answerField(t, got, "key.problem"))
	}
}

func TestUpdatesShowsAMissingKey(t *testing.T) {
	root, _, _ := installLayout(t)
	if err := os.Remove(filepath.Join(root, "trusted.pub")); err != nil {
		t.Fatal(err)
	}
	got := updatesOf(t, startUpdates(t, root, modeRule), asAdmin)
	if answerField(t, got, "key.state") != "missing" || answerField(t, got, "key.found") != false || answerField(t, got, "key.sha256") != "" {
		t.Fatalf("a missing key: %v", got["key"])
	}
	if p, _ := answerField(t, got, "key.problem").(string); !strings.Contains(p, filepath.Join(root, "trusted.pub")) {
		t.Fatalf("the problem does not name the file: %q", p)
	}

	// update.json that names no key, and no update.json at all
	writeFile(t, filepath.Join(root, "update.json"), []byte(`{"data_dir": "/var/lib/pipesmith-registry"}`), 0o644)
	got = updatesOf(t, startUpdates(t, root, modeRule), asAdmin)
	if answerField(t, got, "key.state") != "missing" || answerField(t, got, "key.path") != "" || !strings.Contains(answerField(t, got, "key.problem").(string), "trusted_key") {
		t.Fatalf("no trusted_key: %v", got["key"])
	}
	if err := os.Remove(filepath.Join(root, "update.json")); err != nil {
		t.Fatal(err)
	}
	got = updatesOf(t, startUpdates(t, root, modeRule), asAdmin)
	if answerField(t, got, "config.found") != false || answerField(t, got, "key.state") != "missing" {
		t.Fatalf("no update.json: %v", got)
	}
}

func TestUpdatesRefusesAKeyOthersCanWrite(t *testing.T) {
	root, hexSum, _ := installLayout(t)
	key := filepath.Join(root, "trusted.pub")
	writeFile(t, key, mustRead(t, key), 0o666)
	got := updatesOf(t, startUpdates(t, root, modeRule), asAdmin)
	if answerField(t, got, "key.state") != "refused" || answerField(t, got, "check.passed") != false {
		t.Fatalf("a key others can write: %v %v", got["key"], got["check"])
	}
	if m, _ := answerField(t, got, "check.message").(string); !strings.Contains(m, "chmod go-w "+key) {
		t.Fatalf("the check does not name the fix: %q", m)
	}
	// the fingerprint is still shown, so the admin can compare it
	if answerField(t, got, "key.sha256") != hexSum {
		t.Fatalf("fingerprint %v, want %s", answerField(t, got, "key.sha256"), hexSum)
	}

	// a file that is no key is refused as well
	writeFile(t, key, []byte("not a key\n"), 0o644)
	got = updatesOf(t, startUpdates(t, root, modeRule), asAdmin)
	if answerField(t, got, "key.state") != "refused" || answerField(t, got, "key.sha256") != "" || answerField(t, got, "key.problem") == "" {
		t.Fatalf("a file that is no key: %v", got["key"])
	}
}

func TestUpdatesWithoutTheRuleSaysSo(t *testing.T) {
	root, hexSum, _ := installLayout(t)
	got := updatesOf(t, startUpdates(t, root, nil), asAdmin)
	if answerField(t, got, "key.state") != "unchecked" || answerField(t, got, "check.ran") != false || answerField(t, got, "key.sha256") != hexSum {
		t.Fatalf("without the rule: %v %v", got["key"], got["check"])
	}
}

func TestUpdatesOutsideAnInstallRoot(t *testing.T) {
	got := updatesOf(t, startUpdates(t, "", modeRule), asAdmin)
	if got["installed"] != false || !strings.Contains(got["message"].(string), "not installed through an install root") {
		t.Fatalf("outside an install root: %v", got)
	}
	if _, ok := got["key"]; ok {
		t.Fatalf("outside an install root the answer holds a key: %v", got)
	}
}

func TestUpdatesRuleErrorStaysAnAnswer(t *testing.T) {
	root, _, _ := installLayout(t)
	got := updatesOf(t, startUpdates(t, root, func(string, string) error { return errors.New("owner or mode: lstat: denied") }), asAdmin)
	if answerField(t, got, "key.state") != "refused" || answerField(t, got, "check.message") != "owner or mode: lstat: denied" {
		t.Fatalf("%v %v", got["key"], got["check"])
	}
}

func TestInstallRootOf(t *testing.T) {
	root, _, _ := installLayout(t)
	bin := filepath.Join(root, "releases", "0.2.0", "pipesmith-registry")
	for _, c := range []struct {
		exe, want string
	}{
		{bin, root},
		{filepath.Join(root, "current", "pipesmith-registry"), root}, // the path of ExecStart, through the link
		{filepath.Join(root, "releases", "0.2.0", "other"), ""},
		{filepath.Join(t.TempDir(), "pipesmith-registry"), ""},
	} {
		if got, _ := installRootOf(c.exe); got != realPath(t, c.want) {
			t.Errorf("%s: %q, want %q", c.exe, got, c.want)
		}
	}
	// a root without the link current is no install root
	if err := os.Remove(filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	if got, _ := installRootOf(bin); got != "" {
		t.Errorf("without current: %q", got)
	}
}

func realPath(t *testing.T, p string) string {
	t.Helper()
	if p == "" {
		return ""
	}
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The release comes from the last healthy switch of releases/history.jsonl
// when the file holds one; a refusal after it does not count.
func TestUpdatesShowsTheLastSwitch(t *testing.T) {
	root, _, _ := installLayout(t)
	lines := `{"id":"1","time":"2026-11-20T10:00:00Z","action":"update","result":"ok","from":"0.1.3","to":"0.2.0","reason":"releases/0.2.0 answers healthy"}
{"id":"2","time":"2026-11-20T11:00:00Z","action":"update","result":"refused","from":"0.2.0","to":"0.2.1","reason":"signature"}
`
	writeFile(t, filepath.Join(root, "releases", "history.jsonl"), []byte(lines), 0o644)
	got := updatesOf(t, startUpdates(t, root, modeRule), asAdmin)
	for path, value := range map[string]any{"release.name": "0.2.0", "release.from": "history"} {
		if v := answerField(t, got, path); v != value {
			t.Errorf("%s: %v, want %v", path, v, value)
		}
	}
	if note, _ := answerField(t, got, "release.note").(string); !strings.Contains(note, "2026-11-20T10:00:00Z") {
		t.Errorf("note %q does not name the time of the switch", note)
	}
}
