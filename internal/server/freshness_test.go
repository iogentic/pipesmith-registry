package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/proxy"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// Task 8 of the phase 5 plan: freshness (decisions 5.4, 5.5 and 5.6), as a
// table of testdata/vectors/proxy_freshness.json with the fake clock.

type freshnessRow struct {
	Checked      int64  `json:"checked"` // minutes after start_ms
	ETag         string `json:"etag"`    // a key of validators
	LastModified string `json:"last_modified"`
}

type freshnessCase struct {
	Name    string        `json:"name"`
	Repo    string        `json:"repo"`
	Path    string        `json:"path"`
	Kind    string        `json:"kind"`
	Age     int64         `json:"age"`
	Row     *freshnessRow `json:"row"`
	Minutes int64         `json:"minutes"`
	Remote  string        `json:"remote"`
	Want    struct {
		Requests int               `json:"requests"`
		Body     string            `json:"body"`
		Sent     map[string]string `json:"sent"`
		Row      *freshnessRow     `json:"row"`
	} `json:"want"`
}

type freshnessVectors struct {
	StartMS int64 `json:"start_ms"`
	Proxies []struct {
		Name           string `json:"name"`
		Format         string `json:"format"`
		ContentMaxAge  *int64 `json:"content_max_age"`
		MetadataMaxAge *int64 `json:"metadata_max_age"`
	} `json:"proxies"`
	Validators map[string]string `json:"validators"`
	Cases      []freshnessCase   `json:"cases"`
}

func oldBytes(path string) string { return "old bytes of " + path + "\n" }
func newBytes(path string) string { return "new bytes of " + path + "\n" }

// openProxyState is a catalog.sqlite of its own for the table proxy_state
// of a test.
func openProxyState(t *testing.T) *catalog.Catalog {
	t.Helper()
	c, err := catalog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestProxyFreshnessVectors(t *testing.T) {
	var v freshnessVectors
	if err := json.Unmarshal(mustRead(t, "../../testdata/vectors/proxy_freshness.json"), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) < 24 {
		t.Fatalf("%d cases, the plan asks for at least 24", len(v.Cases))
	}
	start := time.UnixMilli(v.StartMS)
	clock := testkit.NewClock(start)
	state := openProxyState(t)
	ages := map[string][2]int64{}
	var proxies []proxyDef
	for _, p := range v.Proxies {
		section := map[string]any{}
		age := [2]int64{repos.DefaultContentMaxAge, repos.DefaultMetadataMaxAge}
		if p.ContentMaxAge != nil {
			section["contentMaxAge"], age[0] = *p.ContentMaxAge, *p.ContentMaxAge
		}
		if p.MetadataMaxAge != nil {
			section["metadataMaxAge"], age[1] = *p.MetadataMaxAge, *p.MetadataMaxAge
		}
		ages[p.Name] = age
		proxies = append(proxies, proxyDef{name: p.Name, format: p.Format, attributes: map[string]any{"proxy": section,
			"httpclient": map[string]any{"autoBlock": false, "connection": map[string]any{"retries": 0}}}})
	}
	var blobs []testkit.BlobSpec
	for _, c := range v.Cases {
		b := spec(c.Repo, c.Path, []byte(oldBytes(c.Path)), "")
		b.CreatedMS = v.StartMS
		blobs = append(blobs, b)
	}
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: proxies, blobs: blobs, tune: func(o *appOptions) {
		o.proxyState = state
		o.upstreamConfig = proxy.Config{Now: clock.Now}
		o.writeOptions = blobwrite.Options{Now: clock.Now}
	}})
	at := func(minutes int64) int64 { return v.StartMS + minutes*60_000 }
	for _, c := range v.Cases {
		age := ages[c.Repo][0]
		if c.Kind != "file" {
			age = ages[c.Repo][1]
		}
		if age != c.Age {
			t.Fatalf("%s: the case names the age %d, the proxy has %d", c.Name, c.Age, age)
		}
		if c.Row != nil {
			checked := at(c.Row.Checked)
			if err := state.PutProxyRow(catalog.ProxyRow{Repo: c.Repo, Path: c.Path, CheckedMS: &checked, ETag: v.Validators[c.Row.ETag],
				LastModified: v.Validators[c.Row.LastModified]}); err != nil {
				t.Fatal(err)
			}
		}
		target := remoteBase(c.Repo) + c.Path
		switch c.Remote {
		case "304":
			u.remote.Script(target, testkit.NotModified())
		case "304 v1b":
			u.remote.Script(target, testkit.NotModified().With("ETag", v.Validators["v1b"]))
		case "200":
			u.remote.Script(target, testkit.OK(newBytes(c.Path)).With("ETag", v.Validators["v2"]).With("Last-Modified", v.Validators["modified_2"]))
		case "500":
			u.remote.Script(target, testkit.ServerError())
		case "404":
			u.remote.Script(target, testkit.NotFound())
		case "closed":
			u.remote.Script(target, testkit.Closed())
		default:
			t.Fatalf("%s: no remote answer %q", c.Name, c.Remote)
		}
		clock.Set(time.UnixMilli(at(c.Minutes)))
		r := u.get(c.Repo, c.Path)
		want := oldBytes(c.Path)
		if c.Want.Body == "new" {
			want = newBytes(c.Path)
		}
		if r.status != http.StatusOK || string(r.body) != want {
			t.Errorf("%s: %d %q, want %q", c.Name, r.status, r.body, want)
		}
		if n := u.remote.Count(target); n != c.Want.Requests {
			t.Errorf("%s: %d requests at the remote, want %d", c.Name, n, c.Want.Requests)
		}
		for _, req := range u.remote.Requests() {
			if req.Target != target {
				continue
			}
			for _, header := range []string{"If-None-Match", "If-Modified-Since"} {
				sent, wanted := req.Header.Get(header), v.Validators[c.Want.Sent[header]]
				if sent != wanted {
					t.Errorf("%s: %s %q, want %q", c.Name, header, sent, wanted)
				}
			}
		}
		got, err := state.ProxyState(c.Repo, c.Path)
		if err != nil {
			t.Fatal(err)
		}
		switch w := c.Want.Row; {
		case w == nil && got.Row != nil:
			t.Errorf("%s: a row %+v, want none", c.Name, *got.Row)
		case w != nil && got.Row == nil:
			t.Errorf("%s: no row, want %+v", c.Name, *w)
		case w != nil:
			if got.Row.CheckedMS == nil || *got.Row.CheckedMS != at(w.Checked) || got.Row.ETag != v.Validators[w.ETag] ||
				got.Row.LastModified != v.Validators[w.LastModified] || got.Row.NotFoundUntilMS != nil {
				checked := "none"
				if got.Row.CheckedMS != nil {
					checked = fmt.Sprintf("%d minutes", (*got.Row.CheckedMS-v.StartMS)/60_000)
				}
				t.Errorf("%s: row checked %s, %q, %q, not found %v; want checked %d minutes, %+v", c.Name, checked, got.Row.ETag,
					got.Row.LastModified, got.Row.NotFoundUntilMS, w.Checked, *w)
			}
		}
	}
}
