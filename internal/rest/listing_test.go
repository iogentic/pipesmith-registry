package rest

import (
	"reflect"
	"strings"
	"testing"
)

// pageThrough reads every page of a listing and returns the items in the
// order they came, failing on a page without a token that is not the last.
func pageThrough(t *testing.T, env *Env, list func(*Env, map[string]string) (Answer, error), params map[string]string) ([]map[string]any, int) {
	t.Helper()
	var items []map[string]any
	pages := 0
	token := ""
	for {
		q := map[string]string{}
		for k, v := range params {
			q[k] = v
		}
		if token != "" {
			q["continuationToken"] = token
		}
		a, err := list(env, q)
		page := body(t, a, err, 200).(map[string]any)
		if got := keys(page); !reflect.DeepEqual(got, []string{"continuationToken", "items"}) {
			t.Fatalf("keys %v", got)
		}
		pages++
		items = append(items, list2(t, page["items"])...)
		next, _ := page["continuationToken"].(string)
		if next == "" {
			if page["continuationToken"] != nil {
				t.Fatalf("token %#v", page["continuationToken"])
			}
			return items, pages
		}
		token = next
		if pages > 100 {
			t.Fatal("no end")
		}
	}
}

func list2(t *testing.T, v any) []map[string]any {
	t.Helper()
	return list(t, v)
}

func assetsOf(env *Env, q map[string]string) (Answer, error) { return Assets(env, values(q)) }

func componentsOf(env *Env, q map[string]string) (Answer, error) { return Components(env, values(q)) }

func values(m map[string]string) map[string][]string {
	out := map[string][]string{}
	for k, v := range m {
		out[k] = []string{v}
	}
	return out
}

var mavenPaths = []string{
	"com/example/demo-lib/1.0.0/demo-lib-1.0.0-sources.jar",
	"com/example/demo-lib/1.0.0/demo-lib-1.0.0.jar",
	"com/example/demo-lib/1.0.0/demo-lib-1.0.0.jar.sha1",
	"com/example/demo-lib/1.0.0/demo-lib-1.0.0.pom",
	"com/example/demo-lib/1.1.0/demo-lib-1.1.0.jar",
	"com/example/demo-lib/maven-metadata.xml",
	"org/sample/tool/2.0/tool-2.0.jar",
}

// listedMaven is mavenPaths in the order of a listing: by directory, then
// by file name, the key order of the index.
var listedMaven = []string{mavenPaths[5], mavenPaths[0], mavenPaths[1], mavenPaths[2], mavenPaths[3], mavenPaths[4], mavenPaths[6]}

func TestAssetListingPagesEachPathOnce(t *testing.T) {
	f := newFixture(t)
	defer func(n int) { listPage = n }(listPage)
	for _, size := range []int{1, 3, 7, 50} {
		listPage = size
		items, pages := pageThrough(t, f.admin(), assetsOf, map[string]string{"repository": "demo-maven"})
		if got := names(items, "path"); !reflect.DeepEqual(got, listedMaven) {
			t.Fatalf("page size %d: %v", size, got)
		}
		if want := (len(listedMaven) + size - 1) / size; pages != want && pages != want+1 {
			t.Fatalf("page size %d: %d pages", size, pages)
		}
		for _, item := range items {
			if len(item) != 14 || item["repository"] != "demo-maven" || item["id"] != AssetID("demo-maven", item["path"].(string)) {
				t.Fatalf("%#v", item)
			}
		}
	}
}

func TestAssetListingOfAGroupGivesTheWinnerOfEachPath(t *testing.T) {
	f := newFixture(t)
	defer func(n int) { listPage = n }(listPage)
	listPage = 2
	items, _ := pageThrough(t, f.admin(), assetsOf, map[string]string{"repository": "demo-group"})
	want := append(append([]string{}, listedMaven[:6]...), "com/example/remote/3.0/remote-3.0.jar", listedMaven[6])
	if got := names(items, "path"); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
	for _, item := range items {
		member := "demo-maven"
		if strings.Contains(item["path"].(string), "remote") {
			member = "demo-proxy"
		}
		if item["repository"] != member || item["id"] != AssetID("demo-group", item["path"].(string)) {
			t.Fatalf("%#v", item)
		}
	}
}

func TestAssetListingRefusals(t *testing.T) {
	f := newFixture(t)
	defer func(n int) { listPage = n }(listPage)
	listPage = 2
	for _, list := range []func(*Env, map[string]string) (Answer, error){assetsOf, componentsOf} {
		a, err := list(f.admin(), map[string]string{})
		message(t, body(t, a, err, 422))
		a, err = list(f.admin(), map[string]string{"repository": ""})
		message(t, body(t, a, err, 422))
		a, err = list(f.admin(), map[string]string{"repository": "missing"})
		message(t, body(t, a, err, 404))
		// a subject without browse learns nothing of the repository
		a, err = list(f.env("nexus:repository-view:maven2:demo-proxy:browse"), map[string]string{"repository": "demo-maven"})
		message(t, body(t, a, err, 404))
		a, err = list(f.admin(), map[string]string{"repository": "demo-maven"})
		token := body(t, a, err, 200).(map[string]any)["continuationToken"].(string)
		a, err = list(f.admin(), map[string]string{"repository": "demo-raw", "continuationToken": token})
		message(t, body(t, a, err, 400))
		a, err = list(f.admin(), map[string]string{"repository": "demo-maven", "continuationToken": "garbage"})
		message(t, body(t, a, err, 400))
		env := f.admin()
		env.Catalog = nil
		a, err = list(env, map[string]string{"repository": "demo-maven"})
		message(t, body(t, a, err, 503))
	}
	// the two listings take no token of each other
	a, err := Assets(f.admin(), query("repository", "demo-maven"))
	token := body(t, a, err, 200).(map[string]any)["continuationToken"].(string)
	a, err = Components(f.admin(), query("repository", "demo-maven", "continuationToken", token))
	message(t, body(t, a, err, 400))
}

func TestComponentListingPages(t *testing.T) {
	f := newFixture(t)
	defer func(n int) { listPage = n }(listPage)
	listPage = 1
	items, pages := pageThrough(t, f.admin(), componentsOf, map[string]string{"repository": "demo-maven"})
	if got := names(items, "name"); pages != 3 && pages != 4 || !reflect.DeepEqual(got, []string{"demo-lib", "demo-lib", "tool"}) {
		t.Fatalf("%d pages: %v", pages, got)
	}
	if got := names(items, "version"); !reflect.DeepEqual(got, []string{"1.0.0", "1.1.0", "2.0"}) {
		t.Fatalf("%v", got)
	}
	if got := names(list(t, items[0]["assets"]), "path"); !reflect.DeepEqual(got, mavenPaths[:4]) {
		t.Fatalf("%v", got)
	}
	for _, item := range items {
		id, _ := ComponentID("demo-maven", "maven2", list(t, item["assets"])[0]["path"].(string))
		if item["id"] != id {
			t.Fatalf("%#v", item)
		}
	}
}

func TestComponentByID(t *testing.T) {
	f := newFixture(t)
	id, _ := ComponentID("demo-maven", "maven2", jar)
	a, err := ComponentByID(f.admin(), id)
	m := body(t, a, err, 200).(map[string]any)
	if got := keys(m); !reflect.DeepEqual(got, []string{"assets", "format", "group", "id", "name", "repository", "version"}) {
		t.Fatalf("keys %v", got)
	}
	if m["id"] != id || m["group"] != "com.example" || m["name"] != "demo-lib" || m["version"] != "1.0.0" || m["format"] != "maven2" {
		t.Fatalf("%#v", m)
	}
	if got := names(list(t, m["assets"]), "path"); !reflect.DeepEqual(got, mavenPaths[:4]) {
		t.Fatalf("%v", got)
	}
	a, err = ComponentByID(f.admin(), "not an id")
	message(t, body(t, a, err, 422))
	a, err = ComponentByID(f.admin(), AssetID("demo-maven", jar))
	message(t, body(t, a, err, 422))
	missing, _ := ComponentID("demo-maven", "maven2", "com/example/none/9/none-9.jar")
	elsewhere, _ := ComponentID("missing", "maven2", jar)
	for _, id := range []string{missing, elsewhere} {
		a, err = ComponentByID(f.admin(), id)
		message(t, body(t, a, err, 404))
	}
}

// fakeComponents is a component source that answers from a fixed list and
// counts its calls.
type fakeComponents struct {
	paths []string
	calls int
}

func (c *fakeComponents) ComponentPaths(repo string, match func(namespace, name, version string) bool) ([]string, error) {
	c.calls++
	if repo != "demo-maven" || !match("com.example", "demo-lib", "1.0.0") {
		return nil, nil
	}
	return c.paths, nil
}

func TestComponentByIDReadsTheCatalogWhenItIsOpen(t *testing.T) {
	f := newFixture(t)
	id, _ := ComponentID("demo-maven", "maven2", jar)
	// the catalog lists a path the index no longer serves and one of another
	// component: both are left out
	source := &fakeComponents{paths: []string{mavenPaths[1], mavenPaths[3], "com/example/demo-lib/1.0.0/gone-1.0.0.jar", mavenPaths[4]}}
	env := f.admin()
	env.Components = source
	env.Catalog = nil // no walk of the index files
	a, err := ComponentByID(env, id)
	m := body(t, a, err, 200).(map[string]any)
	if got := names(list(t, m["assets"]), "path"); source.calls != 1 || !reflect.DeepEqual(got, []string{mavenPaths[1], mavenPaths[3]}) {
		t.Fatalf("%d calls: %v", source.calls, got)
	}
	other, _ := ComponentID("demo-maven", "maven2", mavenPaths[4])
	a, err = ComponentByID(env, other)
	message(t, body(t, a, err, 404))
}
