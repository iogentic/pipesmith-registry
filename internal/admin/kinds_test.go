package admin

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"
)

// testdata/capabilities/kinds.json and the Go copy of internal/admin hold
// the same types and classes, in the same order (ruling SY6).
func TestKindsFileEqualsTheGoCopy(t *testing.T) {
	data, err := os.ReadFile("../../testdata/capabilities/kinds.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Note    string            `json:"note"`
		Types   []CapabilityKind  `json:"types"`
		Classes []CapabilityClass `json:"classes"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		t.Fatal(err)
	}
	kinds, classes := CapabilityKinds()
	if !reflect.DeepEqual(file.Types, kinds) {
		t.Errorf("kinds.json types\n%+v\nGo copy\n%+v", file.Types, kinds)
	}
	if !reflect.DeepEqual(file.Classes, classes) {
		t.Errorf("kinds.json classes\n%+v\nGo copy\n%+v", file.Classes, classes)
	}
	for _, k := range kinds {
		if k.Type == "" || k.Reason == "" || !slices.Contains([]string{TreatmentSetting, TreatmentReadOnly, TreatmentNone}, k.Treatment) ||
			k.Effect != (k.Treatment == TreatmentSetting) {
			t.Errorf("type %+v", k)
		}
	}
	for _, c := range classes {
		if c.Class == "" || c.Reason == "" || !slices.Contains([]string{TreatmentSetting, TreatmentReadOnly, TreatmentNone}, c.Treatment) {
			t.Errorf("class %+v", c)
		}
	}
}

// Every capability type of the content fixture is in kinds.json.
func TestKindsCoverTheFixture(t *testing.T) {
	f, err := os.Open("../../testdata/export/fixture-content/capability_storage_item.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	for sc := bufio.NewScanner(f); sc.Scan(); {
		var row struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		if _, ok := kindOf(row.Type); !ok {
			t.Errorf("the fixture type %q is not in kinds.json", row.Type)
		}
		n++
	}
	if n != 3 {
		t.Fatalf("%d capabilities in the fixture, want 3", n)
	}
}
