package logship

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// The helpers of the adapter tests: 1,000 hostile lines made by the log
// files, the id of a line a stub decoded, and a certificate authority.

// hostileText is in every hostile line: quotes, a backslash, a line feed, a
// carriage return, a NUL, a tab, non ASCII letters, the line separator
// U+2028 and the characters HTML escapes.
const hostileText = "quote \" back\\slash line\nbreak\r nul\x00 tab\t \xc3\xa9\xc3\x9f\xe6\xbc\xa2 \xe2\x80\xa8 <&>"

// hostileLines makes 1,000 lines through the tap of internal/logs: server
// lines at the four levels, request, audit and task lines in turn, each
// with the hostile text, and line 500, a server line, with 100 KiB more.
// The id of a line comes right after its head: line_id in a server line,
// request_id in a request or audit line, run_id in a task line.
func hostileLines(t *testing.T) []logs.Line {
	t.Helper()
	var mu sync.Mutex
	var got []logs.Line
	var at atomic.Int64
	at.Store(demoStart.UnixNano())
	files := logs.Open(logs.Options{Dir: filepath.Join(t.TempDir(), "logs"), Settings: logs.DefaultSettings(),
		Now: func() time.Time { return time.Unix(0, at.Load()).UTC() },
		Ship: func(l logs.Line) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, l)
		}})
	log := slog.New(files.Handler())
	levels := []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError}
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("hostile-%04d", i)
		switch i % 7 {
		case 0, 1, 2, 3:
			attrs := []any{"line_id", id, "logger", "pipesmith_registry.demo", "text", hostileText}
			if i == 500 {
				attrs = append(attrs, "big", strings.Repeat("x", 100<<10))
			}
			log.Log(context.Background(), levels[i%7], "server \"line\" \xc3\xa9", attrs...)
		case 4:
			files.Request(logs.Request{ID: id, Time: time.Unix(0, at.Load()).UTC(), Client: "192.0.2.7", Subject: "demo-reader",
				Method: "GET", Path: "/repository/demo-raw/a\"b\\c\xc3\xa9", Query: "q=" + hostileText, Protocol: "HTTP/1.1",
				Status: 200, UserAgent: hostileText, Repository: "demo-raw", Format: "raw", Port: 14100})
		case 5:
			if err := files.Audit(logs.AuditEvent{RequestID: id, Subject: "demo-admin", Client: "192.0.2.8",
				Action: "repository.update", ObjectType: "repository", ObjectName: hostileText, Result: "ok",
				Fields: []string{"name", hostileText}}); err != nil {
				t.Fatal(err)
			}
		case 6:
			if err := files.Task(logs.TaskEvent{RunID: id, TaskID: "task-1", TaskType: "demo.scan", Event: "step",
				Result: hostileText, Counts: map[string]int64{"files": int64(i)}}); err != nil {
				t.Fatal(err)
			}
		}
		at.Add(int64(time.Millisecond))
	}
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1000 {
		t.Fatalf("the tap got %d lines, want 1000", len(got))
	}
	return got
}

// lineIDOf is the id of a whole JSON line of hostileLines or testLine.
func lineIDOf(text []byte) (string, error) {
	var v struct {
		LineID    string `json:"line_id"`
		RequestID string `json:"request_id"`
		RunID     string `json:"run_id"`
	}
	if err := json.Unmarshal(text, &v); err != nil {
		return "", err
	}
	for _, id := range []string{v.LineID, v.RequestID, v.RunID} {
		if id != "" {
			return id, nil
		}
	}
	return "", errors.New("a line without an id")
}

var idPattern = regexp.MustCompile(`"(?:line_id|request_id|run_id)":"([^"\\]+)"`)

// lineIDIn is the id in the first bytes of a line that was cut, as syslog
// over UDP cuts a message.
func lineIDIn(text []byte) (string, bool) {
	m := idPattern.FindSubmatch(text)
	if m == nil {
		return "", false
	}
	return string(m[1]), true
}

// idSet is the ids of lines.
func idSet(t *testing.T, lines []logs.Line) map[string]bool {
	t.Helper()
	set := map[string]bool{}
	for _, l := range lines {
		id, err := lineIDOf(l.JSON)
		if err != nil {
			t.Fatalf("%v in %q", err, l.JSON)
		}
		set[id] = true
	}
	return set
}

// noCanary fails the test when text holds one of the canaries.
func noCanary(t *testing.T, where, text string, canaries ...string) {
	t.Helper()
	for _, c := range canaries {
		if strings.Contains(text, c) {
			t.Fatalf("%s holds the canary %q", where, c)
		}
	}
}

// testCA makes a certificate authority and a server certificate signed by it
// for 127.0.0.1, ::1 and localhost, and writes the authority as PEM to a file
// for ca_file.
func testCA(t *testing.T) (caFile string, server tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "demo test authority"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "demo receiver"},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return caFile, tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: key}
}
