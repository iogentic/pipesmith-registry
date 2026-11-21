package tasks

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"
)

// testdata/tasks/kinds.json and the Go copy hold the same types and
// classes, in the same order.
func TestKindsFileEqualsTheGoCopy(t *testing.T) {
	data, err := os.ReadFile("../../testdata/tasks/kinds.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Note    string    `json:"note"`
		Types   []KindRow `json:"types"`
		Classes []KindRow `json:"classes"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		t.Fatal(err)
	}
	types, classes := KindRows()
	if !reflect.DeepEqual(file.Types, types) {
		t.Errorf("kinds.json types\n%+v\nGo copy\n%+v", file.Types, types)
	}
	if !reflect.DeepEqual(file.Classes, classes) {
		t.Errorf("kinds.json classes\n%+v\nGo copy\n%+v", file.Classes, classes)
	}
	if len(types) == 0 || len(classes) == 0 {
		t.Fatalf("%d types, %d classes", len(types), len(classes))
	}
}

// Each row has one treatment and what that treatment needs: a built type
// its fields in the shape of TaskType, a replaced row the type that
// replaces it, a later row its leg, a disabled row its reason. A type has
// an id, a class has none.
func TestKindRowsAreComplete(t *testing.T) {
	types, classes := KindRows()
	ids := map[string]bool{}
	built := map[string]bool{}
	for _, r := range types {
		if r.ID == "" || ids[r.ID] || r.Count != 0 {
			t.Errorf("type %+v: no id, a second row or a count", r)
		}
		ids[r.ID] = true
		built[r.ID] = r.Treatment == TreatmentBuild
	}
	total := len(types)
	for _, r := range classes {
		if r.ID != "" || r.Own || r.Count < 1 {
			t.Errorf("class %+v: an id or no count", r)
		}
		total += r.Count
	}
	for _, r := range append(slices.Clone(types), classes...) {
		if r.Class == "" {
			t.Errorf("%+v has no class", r)
		}
		ok := false
		switch r.Treatment {
		case TreatmentBuild:
			ok = r.Type != nil && r.Type.ID == r.ID && r.Type.Name != "" && r.Type.Description != "" && r.ReplacedBy == "" && r.Leg == "" && r.Reason == ""
			for _, f := range r.Type.Fields {
				ok = ok && f.ID != "" && f.Label != "" && slices.Contains([]string{PropertyString, PropertyNumber, PropertyBoolean,
					PropertySelect, PropertyRepository, PropertyBlobStore}, f.Type)
			}
		case TreatmentReplaced:
			// ruling KN17: a replaced row points at a built type
			ok = built[r.ReplacedBy] && r.Type == nil && r.Leg == ""
		case TreatmentLater:
			ok = r.Leg != "" && r.Type == nil && r.ReplacedBy == ""
		case TreatmentDisabled:
			ok = r.Reason != "" && r.Type == nil && r.ReplacedBy == "" && r.Leg == ""
		}
		if !ok {
			t.Errorf("row %+v", r)
		}
	}
	// the 29 kinds of the capture, the cleanup run over all policies, script
	// execution and the health check reports of proxies
	if total != 32 {
		t.Errorf("the rows count %d kinds, want 32", total)
	}
}

// Every type id of the import fixture has a row (ruling KN3).
func TestKindsCoverTheFixture(t *testing.T) {
	f, err := os.Open("../../testdata/export/fixture-content/qrtz_job_details.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var seen []string
	for sc := bufio.NewScanner(f); sc.Scan(); {
		var row struct {
			JobData map[string]string `json:"job_data"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		id := row.JobData[".typeId"]
		if id == "" {
			continue
		}
		if !slices.Contains(seen, id) {
			seen = append(seen, id)
		}
		r, ok := KindRowOf(id)
		if !ok || r.Own {
			t.Errorf("the fixture type %q has no row of the old product: %+v %v", id, r, ok)
		}
	}
	slices.Sort(seen)
	if !slices.Equal(seen, []string{"blobstore.compact", "db.backup", "repository.cleanup", "repository.maven.remove-snapshots"}) {
		t.Fatalf("fixture types %v", seen)
	}
	// only those four rows are ids of the old product; every other type is
	// this server's own
	types, _ := KindRows()
	for _, r := range types {
		if r.Own == slices.Contains(seen, r.ID) {
			t.Errorf("type %s own %v", r.ID, r.Own)
		}
	}
}

// Leg 6-cleanup builds the deleting kinds (ruling CL8): the class of
// unused maven2 snapshots is the snapshot removal, the scheduling of
// compaction is the compaction kind, and the removal of unused components
// waits for leg 6-cleanup-b; KN2: npm metadata and the docker manifest
// removal are disabled; KN4: the catalog rebuild and the recount are the
// rescan.
func TestKindTreatments(t *testing.T) {
	for _, id := range []string{TypeCompact, TypeCleanup, TypeRemoveSnapshots} {
		if r, ok := KindRowOf(id); !ok || r.Treatment != TreatmentBuild || r.Leg != "" || r.Own {
			t.Errorf("%s %+v", id, r)
		}
	}
	_, classes := KindRows()
	byClass := map[string]KindRow{}
	for _, c := range classes {
		byClass[c.Class] = c
	}
	for class, by := range map[string]string{"removal of unused maven2 snapshots": TypeRemoveSnapshots,
		"compaction scheduling by expiry": TypeCompact} {
		if c := byClass[class]; c.Treatment != TreatmentReplaced || c.ReplacedBy != by {
			t.Errorf("%s %+v", class, c)
		}
	}
	if c := byClass["removal of unused components"]; c.Treatment != TreatmentLater || c.Leg != "6-cleanup-b" ||
		c.Reason != "the owner has to give its rule" {
		t.Errorf("unused components %+v", c)
	}
	for _, class := range []string{"rebuild of npm metadata", "removal of unused docker manifests and layers"} {
		if c := byClass[class]; c.Treatment != TreatmentDisabled || c.Reason != "not built in this release" {
			t.Errorf("%s %+v", class, c)
		}
	}
	for _, class := range []string{"rebuild of the catalog, which replaces the search rebuild", "recount of a store"} {
		if c := byClass[class]; c.Treatment != TreatmentReplaced || c.ReplacedBy != TypeRescan {
			t.Errorf("%s %+v", class, c)
		}
	}
	for _, r := range append(slices.Clone(classes), func() []KindRow { types, _ := KindRows(); return types }()...) {
		if r.Leg == "6-cleanup" {
			t.Errorf("a row still waits for leg 6-cleanup: %+v", r)
		}
	}
}

// TypeList is the built types, in the order of the table, with their
// fields; a change of the copy it gives changes nothing.
func TestTypeList(t *testing.T) {
	list := TypeList()
	var ids []string
	for _, tt := range list {
		ids = append(ids, tt.ID)
	}
	want := []string{TypeCompact, TypeBackup, TypeCleanup, TypeRemoveSnapshots, TypeMavenMetadata, TypeRescan, TypeUploadPurge, TypeWorkFiles}
	if !slices.Equal(ids, want) {
		t.Fatalf("types %v, want %v", ids, want)
	}
	compact := list[0]
	if len(compact.Fields) != 2 || !reflect.DeepEqual(compact.Fields[1], TaskField{ID: "olderThanDays", Label: "Older than (days)",
		Type: PropertyNumber, Required: true, Default: float64(7)}) {
		t.Fatalf("%+v", compact)
	}
	backup := list[1]
	if len(backup.Fields) != 1 || !reflect.DeepEqual(backup.Fields[0], TaskField{ID: "location", Label: "Directory", Type: PropertyString, Required: true}) {
		t.Fatalf("%+v", backup)
	}
	list[1].Fields[0].ID = "changed"
	if TypeList()[1].Fields[0].ID != "location" {
		t.Fatal("TypeList shares its fields")
	}
}

// The reason of a stored task the registry cannot run comes from its row:
// a later row names its leg, an unknown type is not known, and a built row
// without a kind is not built (ruling KN1, KN3).
func TestReasonOfARow(t *testing.T) {
	for id, want := range map[string]string{
		"blobstore.compact":   "the task type blobstore.compact is not built",
		"repository.cleanup":  "the task type repository.cleanup is not built",
		"demo.unknown":        "the task type demo.unknown is not known",
		TypeBackup:            "the task type db.backup is not built",
		"":                    "the task type  is not known",
		"repository.rebuild":  "the task type repository.rebuild is not known",
		"blobstore.compact.x": "the task type blobstore.compact.x is not known",
	} {
		if got := reasonOf(id); got != want {
			t.Errorf("%q: %q, want %q", id, got, want)
		}
	}
}
