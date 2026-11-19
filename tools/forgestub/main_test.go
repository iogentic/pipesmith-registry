package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunServesOnLoopback(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "0.1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "0.1.0", "SHA256SUMS"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	var out bytes.Buffer
	go func() { done <- run(ctx, []string{"-dir", dir, "-owner", "acme"}, &out, ready) }()
	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("run: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no address")
	}
	if !strings.HasPrefix(addr, "http://127.0.0.1:") {
		t.Fatalf("address %s", addr)
	}
	resp, err := http.Get(addr + "/api/packages/acme/generic/pipesmith-registry/v0.1.0/SHA256SUMS")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "x\n" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestRunUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"-dir", t.TempDir()}, {"-owner", "acme"}, {"-dir", t.TempDir(), "-owner", "acme", "extra"},
		{"-dir", filepath.Join(t.TempDir(), "none"), "-owner", "acme"}, {"-dir", t.TempDir(), "-owner", "acme", "-port", "70000"}} {
		if err := run(context.Background(), args, io.Discard, nil); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
}
