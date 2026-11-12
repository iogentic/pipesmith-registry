package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mirosso25/pipesmith-registry/internal/config"
	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
	"github.com/mirosso25/pipesmith-registry/internal/stores"
	"github.com/mirosso25/pipesmith-registry/internal/testkit"
)

// The vectors come from tools/vectors/server_formats.py: the answers of the
// reference application to requests on invented stores, for the merged
// Maven metadata of groups, the npm read path, the folder listing and a
// directory store with a blob manifest.

type formatVectors struct {
	Python    string           `json:"python"`
	Scenarios []formatScenario `json:"scenarios"`
}

type formatScenario struct {
	Name   string `json:"name"`
	Host   string `json:"host"`
	Stores []struct {
		Name  string `json:"name"`
		Kind  string `json:"kind"`
		Blobs []struct {
			Repo        string  `json:"repo"`
			Path        string  `json:"path"`
			Content     []byte  `json:"content"`
			CreatedMS   int64   `json:"created_ms"`
			ContentType *string `json:"content_type"`
		} `json:"blobs"`
		Files []struct {
			Path    string `json:"path"`
			Content []byte `json:"content"`
			MtimeMS int64  `json:"mtime_ms"`
		} `json:"files"`
		Manifest []byte `json:"manifest"`
	} `json:"stores"`
	Definitions json.RawMessage `json:"definitions"`
	Requests    []struct {
		Method  string      `json:"method"`
		Target  string      `json:"target"`
		Headers [][2]string `json:"headers"`
		Answer  struct {
			Status  int         `json:"status"`
			Headers [][2]string `json:"headers"`
			Body    []byte      `json:"body"`
		} `json:"answer"`
	} `json:"requests"`
}

func loadFormatVectors(t *testing.T) *formatVectors {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/server_formats.json")
	if err != nil {
		t.Fatal(err)
	}
	var v formatVectors
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Scenarios) == 0 {
		t.Fatal("no scenarios")
	}
	return &v
}

// formatApp builds the stores and definitions of a scenario, scans them and
// starts a server on them.
func formatApp(t *testing.T, sc *formatScenario) *app {
	t.Helper()
	base := t.TempDir()
	var cfgs []config.StoreConfig
	for _, st := range sc.Stores {
		switch st.Kind {
		case stores.KindBlobStore:
			var blobs []testkit.BlobSpec
			for _, b := range st.Blobs {
				spec := testkit.NewBlobSpec(b.Repo, b.Path)
				spec.Content, spec.CreatedMS, spec.ContentType = b.Content, b.CreatedMS, b.ContentType
				blobs = append(blobs, spec)
			}
			cfgs = append(cfgs, blobStore(t, base, st.Name, blobs...))
		case stores.KindDirectory:
			root := filepath.Join(base, st.Name)
			for _, f := range st.Files {
				target := filepath.Join(root, filepath.FromSlash(f.Path))
				if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, f.Content, 0o600); err != nil {
					t.Fatal(err)
				}
				when := time.UnixMilli(f.MtimeMS)
				if err := os.Chtimes(target, when, when); err != nil {
					t.Fatal(err)
				}
			}
			manifest := filepath.Join(base, st.Name+"-manifest.tsv")
			if err := os.WriteFile(manifest, st.Manifest, 0o600); err != nil {
				t.Fatal(err)
			}
			cfgs = append(cfgs, config.StoreConfig{Name: st.Name, Kind: stores.KindDirectory, Root: root, Manifest: &manifest})
		default:
			t.Fatalf("store kind %q", st.Kind)
		}
	}
	decoded, err := pyjson.Decode(sc.Definitions)
	if err != nil {
		t.Fatal(err)
	}
	var defs []*pyjson.Object
	for _, d := range decoded.([]any) {
		defs = append(defs, d.(*pyjson.Object))
	}
	return startApp(t, base, prepare(t, base, cfgs, appOptions{defs: defs}), appOptions{})
}

// sameHeaders compares the headers of an answer with those of the reference:
// every header the reference sends with its values, and none besides Date
// and the Connection header the test client asks for.
func sameHeaders(want [][2]string, got *response) string {
	expected := map[string][]string{}
	for _, kv := range want {
		name := strings.ToLower(kv[0])
		expected[name] = append(expected[name], kv[1])
	}
	var diffs []string
	for name, values := range expected {
		if have := got.header.Values(name); !slices.Equal(have, values) {
			diffs = append(diffs, name+": "+strings.Join(have, " | ")+", want "+strings.Join(values, " | "))
		}
	}
	for name := range got.header {
		lower := strings.ToLower(name)
		if _, ok := expected[lower]; !ok && lower != "date" && lower != "connection" {
			diffs = append(diffs, "extra "+name+": "+strings.Join(got.header.Values(name), " | "))
		}
	}
	slices.Sort(diffs)
	return strings.Join(diffs, "; ")
}

func TestFormatVectors(t *testing.T) {
	v := loadFormatVectors(t)
	for i := range v.Scenarios {
		sc := &v.Scenarios[i]
		t.Run(sc.Name, func(t *testing.T) {
			a := formatApp(t, sc)
			for _, rq := range sc.Requests {
				var headers []string
				hasHost := false
				for _, kv := range rq.Headers {
					headers = append(headers, kv[0], kv[1])
					hasHost = hasHost || strings.EqualFold(kv[0], "Host")
				}
				if !hasHost {
					headers = append(headers, "Host", sc.Host)
				}
				got := raw(t, a.addr(), rq.Method, rq.Target, headers...)
				want := rq.Answer
				if got.status != want.Status || !bytes.Equal(got.body, want.Body) {
					t.Errorf("%s %s %v: got %d %q\nwant %d %q", rq.Method, rq.Target, rq.Headers, got.status, got.body,
						want.Status, want.Body)
					continue
				}
				if diff := sameHeaders(want.Headers, got); diff != "" {
					t.Errorf("%s %s %v: headers %s", rq.Method, rq.Target, rq.Headers, diff)
				}
			}
		})
	}
}
