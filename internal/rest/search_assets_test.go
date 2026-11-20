package rest

import (
	"reflect"
	"testing"
)

func searchAssets(t *testing.T, env *Env, status int, pairs ...string) ([]map[string]any, any) {
	t.Helper()
	a, err := SearchAssets(env, query(pairs...))
	v := body(t, a, err, status)
	if status != 200 {
		return nil, v
	}
	page := v.(map[string]any)
	if got := keys(page); !reflect.DeepEqual(got, []string{"continuationToken", "items"}) {
		t.Fatalf("keys %v", got)
	}
	items := list(t, page["items"])
	for _, item := range items {
		if len(item) != 14 {
			t.Fatalf("asset keys %v", keys(item))
		}
	}
	return items, page["continuationToken"]
}

func TestSearchAssetsListsTheMatchingAssets(t *testing.T) {
	f := newFixture(t)
	items, token := searchAssets(t, f.admin(), 200, "repository", "demo-maven", "version", "1.0.0")
	if got := names(items, "path"); token != nil || !reflect.DeepEqual(got, listedMaven[1:5]) {
		t.Fatalf("%v %v", got, token)
	}
	items, _ = searchAssets(t, f.admin(), 200, "maven.extension", "pom")
	if got := names(items, "path"); !reflect.DeepEqual(got, []string{"com/example/demo-lib/1.0.0/demo-lib-1.0.0.pom"}) {
		t.Fatalf("%v", got)
	}
	items, _ = searchAssets(t, f.admin(), 200, "name", "nothing")
	if len(items) != 0 {
		t.Fatalf("%v", items)
	}
	// the same refusals as the component search
	_, v := searchAssets(t, f.admin(), 400, "colour", "blue")
	message(t, v)
	_, v = searchAssets(t, f.admin(), 501, "md5", "x")
	if v.(map[string]any)["area"] != "search" {
		t.Fatalf("%#v", v)
	}
	env := f.admin()
	env.Catalog = nil
	_, v = searchAssets(t, env, 503)
	message(t, v)
	// only what the subject may browse
	items, _ = searchAssets(t, f.env("nexus:repository-view:npm:demo-npm:browse"), 200)
	if got := names(items, "repository"); len(got) != 2 || got[0] != "demo-npm" || got[1] != "demo-npm" {
		t.Fatalf("%v", got)
	}
}

func TestSearchAssetsPages(t *testing.T) {
	f := newFixture(t)
	defer func(n int) { searchPage = n }(searchPage)
	all, _ := searchAssets(t, f.admin(), 200)
	if len(all) < 10 {
		t.Fatalf("%d assets", len(all))
	}
	searchPage = 3
	var got []map[string]any
	pairs := []string{}
	for pages := 0; ; pages++ {
		items, token := searchAssets(t, f.admin(), 200, pairs...)
		got = append(got, items...)
		if token == nil {
			break
		}
		if len(items) != 3 || pages > 20 {
			t.Fatalf("page %d has %d items", pages, len(items))
		}
		pairs = []string{"continuationToken", token.(string)}
	}
	if !reflect.DeepEqual(names(got, "id"), names(all, "id")) {
		t.Fatalf("pages %v\nall   %v", names(got, "id"), names(all, "id"))
	}
	// a token of the component search is refused here, and the other way round
	_, token := search(t, f.admin(), 200)
	_, v := searchAssets(t, f.admin(), 400, "continuationToken", token.(string))
	message(t, v)
	_, token = searchAssets(t, f.admin(), 200)
	message(t, mustRefuse(t, f, 400, "continuationToken", token.(string)))
}

func TestSearchAssetsDownload(t *testing.T) {
	f := newFixture(t)
	a, err := SearchAssetsDownload(f.admin(), query("repository", "demo-maven", "maven.extension", "pom"))
	if err != nil || a.Status != 302 || a.Location != "http://localhost:14650/repository/demo-maven/com/example/demo-lib/1.0.0/demo-lib-1.0.0.pom" {
		t.Fatalf("%v %#v", err, a)
	}
	a, err = SearchAssetsDownload(f.admin(), query("repository", "demo-maven", "version", "1.0.0"))
	message(t, body(t, a, err, 400))
	a, err = SearchAssetsDownload(f.admin(), query("name", "nothing"))
	message(t, body(t, a, err, 404))
	a, err = SearchAssetsDownload(f.admin(), query("continuationToken", "x"))
	message(t, body(t, a, err, 400))
	a, err = SearchAssetsDownload(f.admin(), query("sha512", "x"))
	body(t, a, err, 501)
}
