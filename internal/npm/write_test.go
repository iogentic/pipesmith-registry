package npm

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

func TestParseWrite(t *testing.T) {
	for path, want := range map[string]WritePath{
		"demo-pkg":                            {Kind: WriteDocument, Name: "demo-pkg"},
		"@demo/pkg":                           {Kind: WriteDocument, Name: "@demo/pkg"},
		"@demo/pkg/-rev/3-abc":                {Kind: WriteDocument, Name: "@demo/pkg", Rev: "3-abc"},
		"demo-pkg/-/demo-pkg-1.0.0.tgz":       {Kind: WriteTarball, Name: "demo-pkg", File: "demo-pkg-1.0.0.tgz"},
		"@demo/pkg/-/pkg-1.0.0.tgz/-rev/2-x":  {Kind: WriteTarball, Name: "@demo/pkg", File: "pkg-1.0.0.tgz", Rev: "2-x"},
		"-/package/@demo/pkg/dist-tags/beta":  {Kind: WriteDistTag, Name: "@demo/pkg", Tag: "beta"},
		"-/package/demo-pkg/dist-tags/v1-lts": {Kind: WriteDistTag, Name: "demo-pkg", Tag: "v1-lts"},
	} {
		got, ok := ParseWrite(path)
		if !ok || got != want {
			t.Errorf("%s: %+v %v, want %+v", path, got, ok, want)
		}
	}
	for _, path := range []string{"", "-/user/org.couchdb.user:demo", "-/package/demo-pkg/dist-tags", "demo-pkg/1.0.0",
		"demo-pkg/-/notes.txt", "demo-pkg/-rev/", "@demo/pkg/extra/more", ".hidden", "-/package/../dist-tags/x"} {
		if got, ok := ParseWrite(path); ok {
			t.Errorf("%q: %+v", path, got)
		}
	}
}

func decode(t *testing.T, text string) *pyjson.Object {
	t.Helper()
	value, err := pyjson.Decode([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return value.(*pyjson.Object)
}

func dumps(t *testing.T, v any) string {
	t.Helper()
	data, err := pyjson.Dumps(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

const sha1One = "0123456789abcdef0123456789abcdef01234567"

func published(t *testing.T, version, tag string) (*pyjson.Object, *Published) {
	t.Helper()
	document := decode(t, `{"_id": "@demo/pkg", "name": "@demo/pkg", "description": "release `+version+`", "dist-tags": {"`+tag+`": "`+version+`"},
		"versions": {"`+version+`": {"name": "@demo/pkg", "version": "`+version+`", "dist": {"shasum": "`+strings.ToUpper(sha1One)+`",
		"integrity": "sha512-AAEC", "tarball": "http://host/r/@demo/pkg/-/@demo/pkg-`+version+`.tgz", "fileCount": 2}}}, "access": null}`)
	p, err := CheckPublish(document, "@demo/pkg", []string{"pkg-" + version + ".tgz"})
	if err != nil {
		t.Fatal(err)
	}
	return document, p
}

// A publish into no document, then a second version; the stored entries
// carry the tarball path in the repository and the digests of the stored
// tarball.
func TestMergePublish(t *testing.T) {
	document, p := published(t, "1.0.0", "latest")
	if p.Version != "1.0.0" || strings.Join(p.Tags, ",") != "latest" || p.Digests["sha1"] != sha1One || p.Digests["sha512"] != "000102" {
		t.Fatalf("%+v", p)
	}
	first := MergePublish(nil, document, "@demo/pkg", p, sha1One, "sha512-AAEC", "2026-11-18T10:00:00.000Z")
	want := `{"_id": "@demo/pkg", "_rev": "", "name": "@demo/pkg", "versions": {"1.0.0": {"name": "@demo/pkg", "version": "1.0.0", ` +
		`"dist": {"shasum": "` + sha1One + `", "integrity": "sha512-AAEC", "tarball": "@demo/pkg/-/pkg-1.0.0.tgz", "fileCount": 2}}}, ` +
		`"dist-tags": {"latest": "1.0.0"}, "description": "release 1.0.0", "access": null, ` +
		`"time": {"created": "2026-11-18T10:00:00.000Z", "modified": "2026-11-18T10:00:00.000Z", "1.0.0": "2026-11-18T10:00:00.000Z"}}`
	if got := dumps(t, first); got != want {
		t.Fatalf("first\n%s\nwant\n%s", got, want)
	}
	document, p = published(t, "1.1.0-beta.1", "beta")
	second := MergePublish(first, document, "@demo/pkg", p, sha1One, "sha512-AAEC", "2026-11-18T11:00:00.000Z")
	tags := dumps(t, get(second, "dist-tags"))
	if tags != `{"latest": "1.0.0", "beta": "1.1.0-beta.1"}` || get(second, "description") != "release 1.0.0" ||
		versionsOf(second).Len() != 2 || get(get(second, "time").(*pyjson.Object), "created") != "2026-11-18T10:00:00.000Z" {
		t.Fatalf("second %s", dumps(t, second))
	}
	if dumps(t, first) != want {
		t.Fatal("the merge changed its input")
	}
	document, p = published(t, "1.1.0", "latest")
	third := MergePublish(second, document, "@demo/pkg", p, sha1One, "sha512-AAEC", "2026-11-18T12:00:00.000Z")
	if get(third, "description") != "release 1.1.0" || dumps(t, get(third, "dist-tags")) != `{"latest": "1.1.0", "beta": "1.1.0-beta.1"}` {
		t.Fatalf("third %s", dumps(t, third))
	}
}

func TestCheckPublishRefusals(t *testing.T) {
	good := `"versions": {"1.0.0": {"name": "demo-pkg", "version": "1.0.0"}}`
	for _, c := range []struct {
		name, body  string
		attachments []string
		text        string
	}{
		{"name", `{"name": "other", ` + good + `}`, []string{"demo-pkg-1.0.0.tgz"}, "the package of the path"},
		{"two versions", `{"versions": {"1.0.0": {}, "1.0.1": {}}}`, []string{"demo-pkg-1.0.0.tgz"}, "exactly one version"},
		{"bad version", `{"versions": {"../x": {}}}`, []string{"x.tgz"}, "is not a version"},
		{"version field", `{"versions": {"1.0.0": {"version": "2.0.0"}}}`, []string{"demo-pkg-1.0.0.tgz"}, "differs from its key"},
		{"tag of another version", `{"dist-tags": {"latest": "9.9.9"}, ` + good + `}`, []string{"demo-pkg-1.0.0.tgz"}, "no tag of version"},
		{"no attachment", `{` + good + `}`, nil, "exactly one attachment"},
		{"other attachment", `{` + good + `}`, []string{"evil.tgz"}, "is not the tarball"},
		{"integrity", `{"versions": {"1.0.0": {"dist": {"integrity": "sha512"}}}}`, []string{"demo-pkg-1.0.0.tgz"}, "no <algorithm>-<base64>"},
		{"two SHA-1", `{"versions": {"1.0.0": {"dist": {"shasum": "aa", "integrity": "sha1-AAEC"}}}}`, []string{"demo-pkg-1.0.0.tgz"}, "two SHA-1"},
	} {
		_, err := CheckPublish(decode(t, c.body), "demo-pkg", c.attachments)
		var refusal *Refusal
		if !errors.As(err, &refusal) || !strings.Contains(refusal.Message, c.text) {
			t.Errorf("%s: %v, want %q", c.name, err, c.text)
		}
	}
}

func stored(t *testing.T) *pyjson.Object {
	t.Helper()
	return decode(t, `{"_id": "demo-pkg", "_rev": "4-aa", "name": "demo-pkg", "dist-tags": {"latest": "2.0.0", "old": "1.0.0", "next": "3.0.0-rc.1"},
		"versions": {"1.0.0": {"dist": {"tarball": "demo-pkg/-/demo-pkg-1.0.0.tgz"}}, "2.0.0": {"deprecated": "old"},
		"3.0.0-rc.1": {}}, "time": {"created": "a", "modified": "b", "1.0.0": "c", "2.0.0": "d", "3.0.0-rc.1": "e"}}`)
}

func TestUnpublishedAndRemoveVersions(t *testing.T) {
	removed, err := Unpublished(stored(t), decode(t, `{"versions": {"1.0.0": {}}}`))
	if err != nil || strings.Join(removed, ",") != "2.0.0,3.0.0-rc.1" {
		t.Fatalf("%v %v", removed, err)
	}
	left, err := RemoveVersions(stored(t), []string{"2.0.0"}, "now")
	if err != nil {
		t.Fatal(err)
	}
	if got := dumps(t, get(left, "dist-tags")); got != `{"old": "1.0.0", "next": "3.0.0-rc.1", "latest": "1.0.0"}` {
		t.Fatalf("tags %s", got)
	}
	if got := dumps(t, get(left, "time")); got != `{"created": "a", "modified": "now", "1.0.0": "c", "3.0.0-rc.1": "e"}` {
		t.Fatalf("time %s", got)
	}
	for _, c := range []struct{ body, text string }{
		{`{"maintainers": []}`, "lists none"},
		{`{"versions": {"9.0.0": {}}}`, "is not published"},
	} {
		if _, err := Unpublished(stored(t), decode(t, c.body)); err == nil || !strings.Contains(err.Error(), c.text) {
			t.Errorf("%s: %v", c.body, err)
		}
	}
}

func TestTagsAndDeprecate(t *testing.T) {
	tagged, err := SetTag(stored(t), "beta", "1.0.0", "now")
	if err != nil || !HasTag(tagged, "beta") || HasTag(stored(t), "beta") {
		t.Fatalf("%v", err)
	}
	if _, err := SetTag(stored(t), "beta", "9.9.9", "now"); err == nil {
		t.Fatal("a tag of a version that is not published")
	}
	dropped, err := DropTag(stored(t), "old", "now")
	if err != nil || HasTag(dropped, "old") {
		t.Fatalf("%v", err)
	}
	if _, err := DropTag(stored(t), "latest", "now"); err == nil {
		t.Fatal("latest dropped")
	}
	changed, err := Deprecate(stored(t), decode(t, `{"versions": {"1.0.0": {"deprecated": "use 2"}, "2.0.0": {"deprecated": ""}, "3.0.0-rc.1": {"x": 1}}}`), "now")
	if err != nil {
		t.Fatal(err)
	}
	if got := dumps(t, get(changed, "versions")); got != `{"1.0.0": {"dist": {"tarball": "demo-pkg/-/demo-pkg-1.0.0.tgz"}, "deprecated": "use 2"}, "2.0.0": {}, "3.0.0-rc.1": {}}` {
		t.Fatalf("versions %s", got)
	}
	for _, body := range []string{`{}`, `{"versions": {"9.0.0": {"deprecated": "x"}}}`, `{"versions": {"1.0.0": {"deprecated": 7}}}`} {
		if _, err := Deprecate(stored(t), decode(t, body), "now"); err == nil {
			t.Errorf("%s: no refusal", body)
		}
	}
}

func TestRevisionsAndPaths(t *testing.T) {
	rev := NextRev("4-aa", []byte("document"))
	if !regexp.MustCompile(`^5-[0-9a-f]{32}$`).MatchString(rev) || NextRev("", []byte("document"))[:2] != "1-" || rev == NextRev("4-aa", []byte("other")) {
		t.Fatalf("%s", rev)
	}
	if Rev(stored(t)) != "4-aa" || Rev(nil) != "" {
		t.Fatal("Rev")
	}
	if TarballPath("@demo/pkg", "1.0.0") != "@demo/pkg/-/pkg-1.0.0.tgz" || TarballPath("demo-pkg", "2.0.0") != "demo-pkg/-/demo-pkg-2.0.0.tgz" {
		t.Fatal("TarballPath")
	}
	digests, err := Integrity("sha1-AAEC sha384-AAEC sha512-AAEC?opt")
	if err != nil || digests["sha1"] != "000102" || digests["sha512"] != "000102" || len(digests) != 2 {
		t.Fatalf("%v %v", digests, err)
	}
}
