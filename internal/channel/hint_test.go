package channel

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/release/releasetest"
)

// plainText fails when text holds a byte below 0x20 or a second line.
func plainText(t *testing.T, text string) {
	t.Helper()
	for i := 0; i < len(text); i++ {
		if text[i] < 0x20 || text[i] == 0x7f {
			t.Fatalf("byte %#x at %d of %q", text[i], i, text)
		}
	}
}

func hintBundle(t *testing.T, sums []byte, hint string) []byte {
	t.Helper()
	sum := sha256.Sum256(sums)
	data, err := json.Marshal(map[string]any{
		"mediaType":            "application/vnd.dev.sigstore.bundle.v0.3+json",
		"verificationMaterial": map[string]any{"publicKey": map[string]any{"hint": hint}},
		"messageSignature": map[string]any{
			"messageDigest": map[string]any{"algorithm": "SHA2_256", "digest": sum[:]},
			"signature":     []byte{1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The key hint comes from the forge: it is shown only as a short text of
// the base64 alphabet.
func TestKeyHintIsShownOnlyWhenPlain(t *testing.T) {
	key := releasetest.NewSigner(t)
	sums := []byte(strings.Repeat("a", 64) + "  x\n")
	err := verifySignature(key.Pub, sums, hintBundle(t, sums, "x\x1b[2Jy\nstaged: releases/9.9.9"))
	if err == nil {
		t.Fatal("a one byte signature verified")
	}
	plainText(t, err.Error())
	if !strings.Contains(err.Error(), "not shown") || strings.Contains(err.Error(), "staged:") {
		t.Fatalf("message %q", err)
	}
	for hint, shown := range map[string]bool{
		"AbC+/9=":               true,
		strings.Repeat("A", 64): true,
		strings.Repeat("A", 65): false,
		"with space":            false,
		"é":                     false,
	} {
		err := verifySignature(key.Pub, sums, hintBundle(t, sums, hint))
		plainText(t, err.Error())
		if strings.Contains(err.Error(), "key hint "+hint+",") != shown {
			t.Errorf("hint %q shown %v: %v", hint, !shown, err)
		}
	}
}

// A transport error that carries text of the forge reaches the message
// without control characters: net/http quotes the header line.
func TestTransportErrorsArePlain(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			bufio.NewReader(conn).ReadString('\n')
			fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nX\x1b[2J: y\r\n\r\n")
			conn.Close()
		}
	}()
	r := newRig(t)
	r.config(map[string]any{"trusted_key": r.trusted.Pub, "data_dir": r.data,
		"channel": map[string]any{"server": "http://" + ln.Addr().String(), "owner": "acme"}})
	_, ferr := r.f.Fetch(Options{})
	if ferr == nil {
		t.Fatal("a malformed answer passed")
	}
	plainText(t, ferr.Error())
	if !strings.Contains(ferr.Error(), "malformed") {
		t.Fatalf("message %q", ferr)
	}
}
