package tasks

import (
	"encoding/json"
	"slices"
	"testing"
)

// A task type in the shape of the contract (S39): options only for a select
// field, and a default of any JSON value, null among them.
const contractType = `{"id":"demo.purge","name":"Purge uploads","description":"Deletes uploads that were never finished.",` +
	`"fields":[{"id":"repositoryName","label":"Repository","type":"repository","required":true,"default":"*"},` +
	`{"id":"olderThanHours","label":"Older than (hours)","type":"number","required":true,"default":24},` +
	`{"id":"mode","label":"Mode","type":"select","required":false,"default":"delete",` +
	`"options":[{"value":"delete","label":"Delete them"},{"value":"report","label":"Only list them"}]},` +
	`{"id":"dryRun","label":"Dry run","type":"boolean","required":false,"default":false},` +
	`{"id":"note","label":"Note","type":"string","required":false,"default":null}]}`

// A task type reads and writes the JSON names of the contract, and the
// form comes back byte for byte.
func TestTaskTypeJSON(t *testing.T) {
	var tt TaskType
	if err := json.Unmarshal([]byte(contractType), &tt); err != nil {
		t.Fatal(err)
	}
	if tt.ID != "demo.purge" || tt.Name != "Purge uploads" || len(tt.Fields) != 5 {
		t.Fatalf("decoded %+v", tt)
	}
	mode := tt.Fields[2]
	if mode.ID != "mode" || mode.Type != PropertySelect || mode.Required || mode.Default != "delete" ||
		len(mode.Options) != 2 || mode.Options[1] != (FieldOption{Value: "report", Label: "Only list them"}) {
		t.Errorf("the select field: %+v", mode)
	}
	if f := tt.Fields[0]; !f.Required || f.Default != "*" || f.Options != nil {
		t.Errorf("the repository field: %+v", f)
	}
	if tt.Fields[1].Default != 24.0 || tt.Fields[3].Default != false || tt.Fields[4].Default != nil {
		t.Errorf("the defaults %v, %v and %v, want 24, false and null", tt.Fields[1].Default, tt.Fields[3].Default,
			tt.Fields[4].Default)
	}
	data, err := json.Marshal(tt)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != contractType {
		t.Errorf("encoded\n%s\nwant\n%s", data, contractType)
	}
}

// A field without options has no key options; a select field has it.
func TestTaskFieldOptions(t *testing.T) {
	plain, err := json.Marshal(TaskField{ID: "note", Label: "Note", Type: PropertyString})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"id":"note","label":"Note","type":"string","required":false,"default":null}`; string(plain) != want {
		t.Errorf("a field without options: %s, want %s", plain, want)
	}
	choice, err := json.Marshal(TaskField{ID: "mode", Type: PropertySelect, Options: []FieldOption{{"a", "A"}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"id":"mode","label":"","type":"select","required":false,"default":null,"options":[{"value":"a","label":"A"}]}`; string(choice) != want {
		t.Errorf("a select field: %s, want %s", choice, want)
	}
}

// The six field types are the words of the contract.
func TestPropertyTypes(t *testing.T) {
	got := []string{PropertyString, PropertyNumber, PropertyBoolean, PropertySelect, PropertyRepository, PropertyBlobStore}
	want := []string{"string", "number", "boolean", "select", "repository", "blobstore"}
	if !slices.Equal(got, want) {
		t.Errorf("%q, want %q", got, want)
	}
}
