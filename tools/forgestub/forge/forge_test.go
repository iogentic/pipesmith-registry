package forge

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func folders(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "SHA256SUMS"), []byte("sums of "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func get(t *testing.T, url string, header ...string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(header) == 2 {
		req.Header.Set(header[0], header[1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func TestListPages(t *testing.T) {
	var names []string
	for i := range 75 {
		names = append(names, fmt.Sprintf("0.%d.0", i))
	}
	srv := httptest.NewServer(New(Options{Dir: folders(t, names...), Owner: "acme"}))
	defer srv.Close()
	seen := map[string]bool{}
	for page := 1; page <= 3; page++ {
		resp, body := get(t, fmt.Sprintf("%s/api/v1/packages/acme?type=generic&q=%s&page=%d&limit=50", srv.URL, Package, page))
		if resp.StatusCode != 200 || resp.Header.Get("X-Total-Count") != "75" {
			t.Fatalf("page %d: %d total %q", page, resp.StatusCode, resp.Header.Get("X-Total-Count"))
		}
		var entries []map[string]any
		if err := json.Unmarshal(body, &entries); err != nil {
			t.Fatal(err)
		}
		next := strings.Contains(resp.Header.Get("Link"), `rel="next"`)
		switch page {
		case 1:
			if len(entries) != 50 || !next || !strings.Contains(resp.Header.Get("Link"), "page=2") {
				t.Fatalf("page 1: %d entries, link %q", len(entries), resp.Header.Get("Link"))
			}
		case 2:
			if len(entries) != 25 || next || !strings.Contains(resp.Header.Get("Link"), `rel="prev"`) {
				t.Fatalf("page 2: %d entries, link %q", len(entries), resp.Header.Get("Link"))
			}
		case 3:
			if len(entries) != 0 || string(body) != "[]\n" {
				t.Fatalf("page 3: %s", body)
			}
		}
		for _, e := range entries {
			for _, key := range []string{"id", "owner", "type", "name", "version", "html_url", "created_at"} {
				if _, ok := e[key]; !ok {
					t.Fatalf("entry without %s: %v", key, e)
				}
			}
			owner, _ := e["owner"].(map[string]any)
			if owner["login"] != "acme" || e["type"] != "generic" || e["name"] != Package {
				t.Fatalf("entry %v", e)
			}
			seen[e["version"].(string)] = true
		}
	}
	if len(seen) != 75 || !seen["v0.74.0"] {
		t.Fatalf("%d versions seen", len(seen))
	}
	resp, body := get(t, srv.URL+"/api/v1/packages/acme")
	var entries []Entry
	json.Unmarshal(body, &entries)
	if len(entries) != DefaultPageSize || resp.Header.Get("X-Total-Count") != "75" {
		t.Fatalf("default page size: %d", len(entries))
	}
}

func TestFiltersAndExtra(t *testing.T) {
	extra := Entry{ID: 99, Type: "pypi", Name: "other", Version: "v9.9.9"}
	srv := httptest.NewServer(New(Options{Dir: folders(t, "0.1.0"), Owner: "acme", Extra: []Entry{extra}}))
	defer srv.Close()
	for query, want := range map[string]int{"type=generic": 2, "type=pypi": 1, "q=nothing": 1, "q=" + Package: 2} {
		_, body := get(t, srv.URL+"/api/v1/packages/acme?"+query)
		var entries []Entry
		json.Unmarshal(body, &entries)
		if len(entries) != want {
			t.Errorf("%s: %d entries, want %d", query, len(entries), want)
		}
	}
}

func TestDownload(t *testing.T) {
	dir := folders(t, "0.1.0")
	srv := httptest.NewServer(New(Options{Dir: dir, Owner: "acme"}))
	defer srv.Close()
	base := srv.URL + "/api/packages/acme/generic/" + Package
	resp, body := get(t, base+"/v0.1.0/SHA256SUMS")
	if resp.StatusCode != 200 || string(body) != "sums of 0.1.0\n" || resp.ContentLength != int64(len(body)) {
		t.Fatalf("download: %d %q", resp.StatusCode, body)
	}
	for _, path := range []string{base + "/v0.1.0/cosign.pub", base + "/0.1.0/SHA256SUMS", base + "/v0.2.0/SHA256SUMS",
		base + "/v0.1.0/..%2f..%2fx", srv.URL + "/api/packages/other/generic/" + Package + "/v0.1.0/SHA256SUMS",
		srv.URL + "/api/packages/acme/generic/other/v0.1.0/SHA256SUMS", srv.URL + "/api/v1/packages/other"} {
		if resp, _ := get(t, path); resp.StatusCode != 404 {
			t.Errorf("%s: %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestToken(t *testing.T) {
	stub := New(Options{Dir: folders(t, "0.1.0"), Owner: "acme", Token: "s3cret"})
	srv := httptest.NewServer(stub)
	defer srv.Close()
	url := srv.URL + "/api/packages/acme/generic/" + Package + "/v0.1.0/SHA256SUMS"
	if resp, _ := get(t, url); resp.StatusCode != 404 {
		t.Fatalf("without the token: %d", resp.StatusCode)
	}
	if resp, _ := get(t, url, "Authorization", "token wrong"); resp.StatusCode != 404 {
		t.Fatalf("a wrong token: %d", resp.StatusCode)
	}
	if resp, _ := get(t, url, "Authorization", "token s3cret"); resp.StatusCode != 200 {
		t.Fatalf("the token: %d", resp.StatusCode)
	}
	reqs := stub.Requests()
	if len(reqs) != 3 || reqs[2].Authorization != "token s3cret" || reqs[2].Path != "/api/packages/acme/generic/"+Package+"/v0.1.0/SHA256SUMS" {
		t.Fatalf("requests %+v", reqs)
	}
}
