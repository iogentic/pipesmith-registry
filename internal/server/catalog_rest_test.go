package server

import (
	"net/url"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/rest"
)

// The REST asset answer of a written blob carries uploader, address, the
// digests of the write and the last modified time from catalog.sqlite.
func TestTheAssetAnswerTakesWhatTheWriteKept(t *testing.T) {
	w := startCataloged(t)
	written, err := w.ingest("demo-raw", "docs/a.txt", "one")
	if err != nil {
		t.Fatal(err)
	}
	r := do(t, w.addr(), "GET", "/service/rest/v1/assets/"+url.PathEscape(rest.AssetID("demo-raw", "docs/a.txt")), nil, "basic", asAdmin)
	asset := r.object(t)
	if asset["uploader"] != "demo-admin" || asset["uploaderIp"] != "192.0.2.7" || asset["lastModified"] != asset["blobCreated"] {
		t.Fatalf("%#v", asset)
	}
	checksum, _ := asset["checksum"].(map[string]any)
	if checksum["sha256"] != sha256Hex("one") || checksum["sha1"] != *written.asset.SHA1 || len(checksum) != 4 || len(asset) != 14 {
		t.Fatalf("%#v", asset)
	}
}

// With catalog.sqlite open a component is found by id from its rows
// (TestAComponentReadAnswers503WhileTheCatalogIsBehind shows the catalog is
// read), and the listings answer with the assets of the read path.
func TestTheComponentByIDReadsTheCatalog(t *testing.T) {
	w := startCataloged(t)
	for _, path := range []string{"docs/a.txt", "docs/b.txt"} {
		if _, err := w.ingest("demo-raw", path, "one"); err != nil {
			t.Fatal(err)
		}
	}
	id, _ := rest.ComponentID("demo-raw", "raw", "docs/b.txt")
	r := do(t, w.addr(), "GET", "/service/rest/v1/components/"+id, nil, "basic", asAdmin)
	c := r.object(t)
	assets, _ := c["assets"].([]any)
	if r.status != 200 || c["name"] != "docs/b.txt" || c["group"] != "/docs" || len(assets) != 1 {
		t.Fatalf("%d %#v", r.status, c)
	}
	page := do(t, w.addr(), "GET", "/service/rest/v1/assets?repository=demo-raw", nil, "basic", asAdmin).object(t)
	if items, _ := page["items"].([]any); len(items) < 2 {
		t.Fatalf("%#v", page)
	}
}

// The download search answers 302 to the address of the one match, without
// a body, and the redirect serves the bytes.
func TestTheDownloadSearchRedirectsToTheOneMatch(t *testing.T) {
	w := startCataloged(t)
	if _, err := w.ingest("demo-raw", "docs/a.txt", "one"); err != nil {
		t.Fatal(err)
	}
	r := do(t, w.addr(), "GET", "/service/rest/v1/search/assets/download?repository=demo-raw&name=docs/a.txt", nil, "basic", asAdmin)
	if r.status != 302 || r.header.Get("Location") != "http://"+w.addr()+"/repository/demo-raw/docs/a.txt" || len(r.body) != 0 {
		t.Fatalf("%d %v %q", r.status, r.header, r.body)
	}
	if r := do(t, w.addr(), "GET", "/service/rest/v1/search/assets/download?name=none", nil, "basic", asAdmin); r.status != 404 {
		t.Fatalf("%d %s", r.status, r.body)
	}
}
