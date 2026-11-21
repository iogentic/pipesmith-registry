package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/tools/githubstub/gh"
)

func TestRunServesTheReleases(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"0.1.0", "0.10.0", "0.2.0"} {
		if err := os.MkdirAll(filepath.Join(dir, v), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, v, "SHA256SUMS"), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "0.10.0", "notes.md"), []byte("# notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	var out bytes.Buffer
	go func() { done <- run(ctx, []string{"-dir", dir, "-repo", "acme/tool"}, &out, ready) }()
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
	resp, err := http.Get(addr + "/repos/acme/tool/releases?per_page=2")
	if err != nil {
		t.Fatal(err)
	}
	var list []gh.Release
	err = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if err != nil || len(list) != 2 || list[0].TagName != "v0.10.0" || list[0].Body != "# notes\n" || list[1].TagName != "v0.2.0" ||
		!strings.Contains(resp.Header.Get("Link"), `rel="next"`) || len(list[0].Assets) != 1 {
		t.Fatalf("list %+v, Link %q, %v", list, resp.Header.Get("Link"), err)
	}
	req, _ := http.NewRequest("GET", list[0].Assets[0].URL, nil)
	req.Header.Set("Accept", "application/octet-stream")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "0.10.0\n" {
		t.Fatalf("asset %d %q", resp.StatusCode, body)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestRunUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"-dir", t.TempDir()}, {"-repo", "a/b"}, {"-dir", t.TempDir(), "-repo", "ab"},
		{"-dir", t.TempDir(), "-repo", "a/b/c"}, {"-dir", filepath.Join(t.TempDir(), "none"), "-repo", "a/b"},
		{"-dir", t.TempDir(), "-repo", "a/b", "-port", "70000"}, {"-dir", t.TempDir(), "-repo", "a/b", "extra"}} {
		if err := run(context.Background(), args, io.Discard, nil); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
}
