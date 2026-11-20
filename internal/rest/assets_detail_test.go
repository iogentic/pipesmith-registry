package rest

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// details is a catalog that holds the side data of one blob.
type details struct {
	repo, path, store string
	created           int64
	detail            catalog.AssetDetail
}

func (d details) Detail(repo, path, store string, createdMS int64) (*catalog.AssetDetail, error) {
	if repo != d.repo || path != d.path || store != d.store || createdMS != d.created {
		return nil, nil
	}
	detail := d.detail
	return &detail, nil
}

// With catalog.sqlite open the asset answer takes uploader, address, the
// digests of a write and the last modified time from it, in the 14 keys of
// the answer without it.
func TestAssetTakesTheDetailsOfTheCatalog(t *testing.T) {
	f := newFixture(t)
	env := f.admin()
	uploader, address, modified := "demo-admin", "192.0.2.7", older+5
	env.Details = details{repo: "demo-maven", path: jar, store: "store-a", created: older, detail: catalog.AssetDetail{
		Uploader: &uploader, Address: &address, MD5: "m5", SHA256: "s256", SHA512: "s512", LastModifiedMS: &modified}}
	a, err := Asset(env, AssetID("demo-maven", jar))
	m := body(t, a, err, 200).(map[string]any)
	if len(m) != 14 {
		t.Fatalf("%d keys: %#v", len(m), m)
	}
	want := map[string]any{"sha1": testkit.SHA1Hex([]byte("jar 1.0.0")), "md5": "m5", "sha256": "s256", "sha512": "s512"}
	if got, _ := json.Marshal(m["checksum"]); string(got) != string(mustJSON(t, want)) {
		t.Fatalf("checksum %s", got)
	}
	if m["uploader"] != uploader || m["uploaderIp"] != address ||
		m["lastModified"] != time.UnixMilli(modified).UTC().Format("2006-01-02T15:04:05.000Z") || m["lastDownloaded"] != nil {
		t.Fatalf("%#v", m)
	}
	// a path the catalog holds nothing of answers as without it
	a, err = Asset(env, AssetID("demo-raw", "docs/readme.txt"))
	m = body(t, a, err, 200).(map[string]any)
	if m["uploader"] != nil || m["uploaderIp"] != nil || m["lastModified"] != nil || len(m["checksum"].(map[string]any)) != 1 {
		t.Fatalf("%#v", m)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
