package repos

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

func str(s string) *string { return &s }

// Without attributes and definition fields: no write policy, which refuses
// every write, the write store of the server and the limits of W14.
func TestWriteSettingsDefaults(t *testing.T) {
	got := Repo{Name: "demo-raw", Format: "raw", Type: "hosted"}.WriteSettings()
	want := WriteSettings{WriteStore: DefaultWriteStore, MaxBodyBytes: 10 << 30, FreeSpaceFloor: 2 << 30}
	if got != want {
		t.Fatalf("%+v", got)
	}
	if DefaultWriteStore != "local-write" {
		t.Fatal(DefaultWriteStore)
	}
}

// The fields of repositories.json apply when the state has no attributes.
func TestWriteSettingsFromDefinition(t *testing.T) {
	r := Repo{Name: "demo-releases", Format: "maven2", Type: "hosted", WritePolicy: str("ALLOW_ONCE"),
		VersionPolicy: str("RELEASE"), LayoutPolicy: str("STRICT")}
	got := r.WriteSettings()
	if got.WritePolicy != "ALLOW_ONCE" || got.VersionPolicy != "RELEASE" || got.LayoutPolicy != "STRICT" {
		t.Fatalf("%+v", got)
	}
}

func attributesOf(t *testing.T, document string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(document), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The stored attributes win over the definition; numbers come as the state
// decodes them.
func TestWriteSettingsFromAttributes(t *testing.T) {
	attrs := attributesOf(t, `{
		"storage": {"blobStoreName": "store-a", "strictContentTypeValidation": true, "writePolicy": "ALLOW", "latestPolicy": true},
		"maven": {"versionPolicy": "SNAPSHOT", "layoutPolicy": "PERMISSIVE"},
		"write_store": "other-write", "max_body_bytes": 1024, "free_space_floor_bytes": 2048}`)
	r := Repo{Name: "demo-snapshots", Format: "maven2", Type: "hosted", WritePolicy: str("DENY"), VersionPolicy: str("RELEASE"),
		LayoutPolicy: str("STRICT"), Attributes: attrs}
	want := WriteSettings{WritePolicy: "ALLOW", VersionPolicy: "SNAPSHOT", LayoutPolicy: "PERMISSIVE", StrictContentType: true,
		LatestRedeploy: true, BlobStoreName: "store-a", WriteStore: "other-write", MaxBodyBytes: 1024, FreeSpaceFloor: 2048}
	if got := r.WriteSettings(); got != want {
		t.Fatalf("%+v\nwant %+v", got, want)
	}
	// as internal/state decodes them: json.Number
	r.Attributes = map[string]any{"max_body_bytes": json.Number("4096"), "free_space_floor_bytes": json.Number("0"),
		"storage": map[string]any{"writePolicy": "ALLOW_ONCE"}}
	got := r.WriteSettings()
	if got.MaxBodyBytes != 4096 || got.FreeSpaceFloor != 0 || got.WritePolicy != "ALLOW_ONCE" || got.VersionPolicy != "RELEASE" {
		t.Fatalf("%+v", got)
	}
	// values of the wrong kind leave the field of the definition or the default
	r.Attributes = map[string]any{"max_body_bytes": "big", "free_space_floor_bytes": json.Number("-5"), "write_store": "",
		"storage": map[string]any{"writePolicy": 7, "strictContentTypeValidation": "yes"}}
	got = r.WriteSettings()
	if got.MaxBodyBytes != DefaultMaxBodyBytes || got.FreeSpaceFloor != DefaultFreeSpaceFloor || got.WriteStore != DefaultWriteStore ||
		got.WritePolicy != "DENY" || got.StrictContentType {
		t.Fatalf("%+v", got)
	}
}

// WithAttributes gives each repository the attributes of the record of its
// name and leaves the repositories it was called on as they are.
func TestWithAttributes(t *testing.T) {
	rs := New([]Repo{{Name: "demo-raw", Format: "raw", Type: "hosted", Defined: true}}, []string{"discovered"}, nil)
	records := []model.Repository{
		{Name: "demo-raw", Format: "raw", Type: "hosted", Attributes: map[string]any{"storage": map[string]any{"writePolicy": "ALLOW"}}},
		{Name: "elsewhere", Format: "raw", Type: "hosted", Attributes: map[string]any{"x": 1}},
	}
	with := rs.WithAttributes(records)
	raw, _ := with.Get("demo-raw")
	if raw.WriteSettings().WritePolicy != "ALLOW" || !raw.Defined {
		t.Fatalf("%+v", raw)
	}
	if _, ok := with.Get("elsewhere"); ok {
		t.Fatal("a record without a repository became one")
	}
	if d, _ := with.Get("discovered"); d.Attributes != nil {
		t.Fatalf("%+v", d)
	}
	if before, _ := rs.Get("demo-raw"); before.Attributes != nil {
		t.Fatal("WithAttributes changed the repositories it was called on")
	}
	if !reflect.DeepEqual(names(with.All()), names(rs.All())) {
		t.Fatalf("%v", names(with.All()))
	}
	if rs.WithAttributes(nil) == nil {
		t.Fatal("nil records")
	}
}

func names(list []Repo) []string {
	out := []string{}
	for _, r := range list {
		out = append(out, r.Name)
	}
	return out
}
