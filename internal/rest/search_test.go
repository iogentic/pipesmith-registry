package rest

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mirosso25/pipesmith-registry/internal/testkit"
)

const jar = "com/example/demo-lib/1.0.0/demo-lib-1.0.0.jar"

// Assets

func TestAssetIDRoundTrip(t *testing.T) {
	for _, c := range [][2]string{{"demo-maven", jar}, {"demo-docker", "v2/-/blobs/sha256:" + strings.Repeat("ab", 32)},
		{"r", "a b/ü/c:d"}} {
		id := AssetID(c[0], c[1])
		if strings.ContainsAny(id, "+/=") {
			t.Fatalf("%q is not URL safe", id)
		}
		repo, path, ok := DecodeAssetID(id)
		if !ok || repo != c[0] || path != c[1] {
			t.Fatalf("%q: %q %q %v", id, repo, path, ok)
		}
	}
	for _, id := range []string{"", "!!", "bm9jb2xvbg", AssetID("", "x"), AssetID("r", ""), "ZGVtby1tYXZlbjo=="} {
		if _, _, ok := DecodeAssetID(id); ok {
			t.Errorf("%q decoded", id)
		}
	}
}

func TestAssetByID(t *testing.T) {
	f := newFixture(t)
	a, err := Asset(f.admin(), AssetID("demo-maven", jar))
	m := body(t, a, err, 200).(map[string]any)
	want := map[string]any{
		"id":             AssetID("demo-maven", jar),
		"path":           jar,
		"downloadUrl":    "http://localhost:14650/repository/demo-maven/" + jar,
		"repository":     "demo-maven",
		"format":         "maven2",
		"checksum":       map[string]any{"sha1": testkit.SHA1Hex([]byte("jar 1.0.0"))},
		"contentType":    "application/octet-stream",
		"lastModified":   nil,
		"lastDownloaded": nil,
		"uploader":       nil,
		"uploaderIp":     nil,
		"fileSize":       json.Number("9"),
		"blobCreated":    time.UnixMilli(older).UTC().Format("2006-01-02T15:04:05.000Z"),
		"blobStoreName":  "store-a",
	}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("got  %#v\nwant %#v", m, want)
	}
}

func TestAssetOfAGroupNamesTheMember(t *testing.T) {
	f := newFixture(t)
	path := "com/example/remote/3.0/remote-3.0.jar"
	a, err := Asset(f.admin(), AssetID("demo-group", path))
	m := body(t, a, err, 200).(map[string]any)
	if m["repository"] != "demo-proxy" || m["blobStoreName"] != "store-b" || m["downloadUrl"] != "http://localhost:14650/repository/demo-group/"+path {
		t.Fatalf("%#v", m)
	}
	// the first member that holds the path wins, as in the read path
	a, err = Asset(f.admin(), AssetID("demo-group", jar))
	if m := body(t, a, err, 200).(map[string]any); m["repository"] != "demo-maven" {
		t.Fatalf("%#v", m)
	}
}

func TestAssetWinsAcrossStores(t *testing.T) {
	f := newFixture(t)
	a, err := Asset(f.admin(), AssetID("demo-raw", "docs/readme.txt"))
	m := body(t, a, err, 200).(map[string]any)
	if m["blobStoreName"] != "store-b" || m["fileSize"] != json.Number("10") {
		t.Fatalf("%#v", m)
	}
}

func TestAssetRefusals(t *testing.T) {
	f := newFixture(t)
	a, err := Asset(f.admin(), "not an id")
	message(t, body(t, a, err, 422))
	for _, id := range []string{AssetID("demo-maven", "missing.jar"), AssetID("missing", jar), AssetID("demo-maven", "com/example")} {
		a, err := Asset(f.admin(), id)
		message(t, body(t, a, err, 404))
	}
}

// Search

type component struct {
	repository, group, name, version string
	assets                           []string
}

func search(t *testing.T, env *Env, status int, pairs ...string) (comps []component, token any) {
	t.Helper()
	a, err := Search(env, query(pairs...))
	v := body(t, a, err, status)
	if status != 200 {
		return nil, v
	}
	page := v.(map[string]any)
	if got := keys(page); !reflect.DeepEqual(got, []string{"continuationToken", "items"}) {
		t.Fatalf("keys %v", got)
	}
	for _, item := range list(t, page["items"]) {
		if got := keys(item); !reflect.DeepEqual(got, []string{"assets", "format", "group", "id", "name", "repository", "version"}) {
			t.Fatalf("component keys %v", got)
		}
		c := component{repository: item["repository"].(string), name: item["name"].(string)}
		if g, ok := item["group"].(string); ok {
			c.group = g
		}
		if v, ok := item["version"].(string); ok {
			c.version = v
		}
		for _, asset := range list(t, item["assets"]) {
			if len(asset) != 14 {
				t.Fatalf("asset keys %v", keys(asset))
			}
			if asset["repository"] != c.repository {
				t.Fatalf("asset of %s in %v", c.repository, asset["repository"])
			}
			c.assets = append(c.assets, asset["path"].(string))
		}
		comps = append(comps, c)
	}
	return comps, page["continuationToken"]
}

func short(comps []component) []string {
	out := []string{}
	for _, c := range comps {
		out = append(out, c.repository+"|"+c.group+"|"+c.name+"|"+c.version)
	}
	return out
}

var allComponents = []string{
	"demo-docker||app|1.0",
	"demo-maven|com.example|demo-lib|1.0.0",
	"demo-maven|com.example|demo-lib|1.1.0",
	"demo-maven|org.sample|tool|2.0",
	"demo-npm||widget|1.2.3",
	"demo-npm|demo|gadget|0.1.0",
	"demo-proxy|com.example|demo-lib|1.0.0",
	"demo-proxy|com.example|remote|3.0",
	"demo-raw|/|top.txt|",
	"demo-raw|/docs|docs/readme.txt|",
}

func TestSearchEverything(t *testing.T) {
	f := newFixture(t)
	comps, token := search(t, f.admin(), 200)
	if got := short(comps); !reflect.DeepEqual(got, allComponents) {
		t.Fatalf("got  %v\nwant %v", got, allComponents)
	}
	if token != nil {
		t.Fatalf("token %v", token)
	}
	if got := comps[1].assets; !reflect.DeepEqual(got, []string{
		"com/example/demo-lib/1.0.0/demo-lib-1.0.0-sources.jar",
		"com/example/demo-lib/1.0.0/demo-lib-1.0.0.jar",
		"com/example/demo-lib/1.0.0/demo-lib-1.0.0.jar.sha1",
		"com/example/demo-lib/1.0.0/demo-lib-1.0.0.pom"}) {
		t.Fatalf("assets %v", got)
	}
}

func TestSearchFilters(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		pairs []string
		want  []string
	}{
		{[]string{"name", "demo-lib"}, []string{allComponents[1], allComponents[2], allComponents[6]}},
		{[]string{"maven.artifactId", "demo-lib"}, []string{allComponents[1], allComponents[2], allComponents[6]}},
		{[]string{"group", "com.example"}, []string{allComponents[1], allComponents[2], allComponents[6], allComponents[7]}},
		{[]string{"maven.groupId", "org.sample"}, []string{allComponents[3]}},
		{[]string{"version", "1.0.0"}, []string{allComponents[1], allComponents[6]}},
		{[]string{"maven.baseVersion", "1.1.0"}, []string{allComponents[2]}},
		{[]string{"name", "demo-*", "version", "1.1*"}, []string{allComponents[2]}},
		{[]string{"q", "demo-lib"}, []string{allComponents[1], allComponents[2], allComponents[6]}},
		{[]string{"q", "DEMO-LIB"}, []string{allComponents[1], allComponents[2], allComponents[6]}},
		{[]string{"q", "demo-* 1.1"}, []string{allComponents[2]}},
		{[]string{"q", "readme"}, []string{allComponents[9]}},
		{[]string{"q", "nothing-like-this"}, nil},
		{[]string{"repository", "demo-group"}, []string{allComponents[1], allComponents[2], allComponents[3], allComponents[6], allComponents[7]}},
		{[]string{"repository", "demo-group", "name", "remote"}, []string{allComponents[7]}},
		{[]string{"format", "npm"}, []string{allComponents[4], allComponents[5]}},
		{[]string{"npm.scope", "demo"}, []string{allComponents[5]}},
		{[]string{"npm.scope", "@demo"}, []string{allComponents[5]}},
		{[]string{"docker.imageName", "app"}, []string{allComponents[0]}},
		{[]string{"docker.imageTag", "1.0"}, []string{allComponents[0]}},
		{[]string{"group", "/docs"}, []string{allComponents[9]}},
		{[]string{"name", "top.txt"}, []string{allComponents[8]}},
		{[]string{"repository", "missing"}, nil},
	}
	for _, c := range cases {
		comps, _ := search(t, f.admin(), 200, c.pairs...)
		want := c.want
		if want == nil {
			want = []string{}
		}
		if got := short(comps); !reflect.DeepEqual(got, want) {
			t.Errorf("%v: got %v, want %v", c.pairs, got, want)
		}
	}
}

func TestSearchAssetFiltersListTheMatchingAssets(t *testing.T) {
	f := newFixture(t)
	comps, _ := search(t, f.admin(), 200, "maven.extension", "pom")
	if len(comps) != 1 || !reflect.DeepEqual(comps[0].assets, []string{"com/example/demo-lib/1.0.0/demo-lib-1.0.0.pom"}) {
		t.Fatalf("%#v", comps)
	}
	comps, _ = search(t, f.admin(), 200, "maven.classifier", "sources")
	if len(comps) != 1 || !reflect.DeepEqual(comps[0].assets, []string{"com/example/demo-lib/1.0.0/demo-lib-1.0.0-sources.jar"}) {
		t.Fatalf("%#v", comps)
	}
	comps, _ = search(t, f.admin(), 200, "sha1", testkit.SHA1Hex([]byte("jar 1.0.0")))
	if got := short(comps); !reflect.DeepEqual(got, []string{allComponents[1]}) || !reflect.DeepEqual(comps[0].assets, []string{jar}) {
		t.Fatalf("%v %v", got, comps)
	}
	// the losing row of a path does not match: only the winner counts
	comps, _ = search(t, f.admin(), 200, "sha1", testkit.SHA1Hex([]byte("old readme")))
	if len(comps) != 0 {
		t.Fatalf("%#v", comps)
	}
	comps, _ = search(t, f.admin(), 200, "sha1", strings.ToUpper(testkit.SHA1Hex([]byte("new readme"))))
	if got := short(comps); !reflect.DeepEqual(got, []string{allComponents[9]}) {
		t.Fatalf("%v", got)
	}
}

func TestSearchShowsTheWinnerOfAPath(t *testing.T) {
	f := newFixture(t)
	a, err := Search(f.admin(), query("name", "docs/readme.txt"))
	page := body(t, a, err, 200).(map[string]any)
	items := list(t, page["items"])
	if len(items) != 1 {
		t.Fatalf("%#v", items)
	}
	assets := list(t, items[0]["assets"])
	if len(assets) != 1 || assets[0]["blobStoreName"] != "store-b" || assets[0]["fileSize"] != json.Number("10") ||
		assets[0]["checksum"].(map[string]any)["sha1"] != testkit.SHA1Hex([]byte("new readme")) {
		t.Fatalf("%#v", assets)
	}
	if assets[0]["id"] != AssetID("demo-raw", "docs/readme.txt") {
		t.Fatalf("id %v", assets[0]["id"])
	}
}

func TestSearchShowsOnlyBrowsableRepositories(t *testing.T) {
	f := newFixture(t)
	env := f.env("nexus:repository-view:maven2:demo-maven:browse")
	comps, _ := search(t, env, 200)
	if got := short(comps); !reflect.DeepEqual(got, allComponents[1:4]) {
		t.Fatalf("%v", got)
	}
	comps, _ = search(t, env, 200, "repository", "demo-npm")
	if len(comps) != 0 {
		t.Fatalf("%v", short(comps))
	}
	// a group is checked by its own permission, not by those of its members
	comps, _ = search(t, f.env("nexus:repository-view:maven2:demo-group:browse"), 200, "repository", "demo-group")
	if len(comps) != 5 {
		t.Fatalf("%v", short(comps))
	}
	comps, _ = search(t, env, 200, "repository", "demo-group")
	if len(comps) != 0 {
		t.Fatalf("%v", short(comps))
	}
}

func TestSearchPages(t *testing.T) {
	f := newFixture(t)
	defer func(n int) { searchPage = n }(searchPage)
	searchPage = 3
	var got []string
	pairs := []string{}
	pages := 0
	for {
		comps, token := search(t, f.admin(), 200, pairs...)
		pages++
		got = append(got, short(comps)...)
		if token == nil {
			break
		}
		if len(comps) != 3 {
			t.Fatalf("page %d has %d items and a token", pages, len(comps))
		}
		pairs = []string{"continuationToken", token.(string)}
		if pages > 10 {
			t.Fatal("no end")
		}
	}
	if pages != 4 || !reflect.DeepEqual(got, allComponents) {
		t.Fatalf("%d pages: %v", pages, got)
	}
	// a token of another query is refused
	_, token := search(t, f.admin(), 200)
	message(t, mustRefuse(t, f, 400, "continuationToken", token.(string), "format", "npm"))
	message(t, mustRefuse(t, f, 400, "continuationToken", "garbage"))
}

func mustRefuse(t *testing.T, f *fixture, status int, pairs ...string) any {
	t.Helper()
	_, v := search(t, f.admin(), status, pairs...)
	return v
}

func TestSearchParameters(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"md5", "sha256", "sha512", "npm.author", "npm.description", "npm.keywords", "npm.license",
		"docker.layerId", "docker.contentDigest", "sort", "direction", "prerelease"} {
		v := mustRefuse(t, f, 501, name, "x").(map[string]any)
		if v["area"] != "search" || !strings.Contains(v["message"].(string), name) {
			t.Errorf("%s: %#v", name, v)
		}
	}
	message(t, mustRefuse(t, f, 400, "colour", "blue"))
	message(t, mustRefuse(t, f, 400, "q", strings.Repeat("x", 1025)))
}

func TestSearchStopsWithTheRequest(t *testing.T) {
	f := newFixture(t)
	env := f.admin()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	env.Context = ctx
	if _, err := Search(env, query()); err == nil {
		t.Fatal("a cancelled search ran to the end")
	}
	env.Catalog = nil
	env.Context = context.Background()
	a, err := Search(env, query())
	message(t, body(t, a, err, 503))
}
