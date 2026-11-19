package admin

import (
	"fmt"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

func TestGroupVectors(t *testing.T) {
	var file struct {
		Family       string             `json:"family"`
		Note         string             `json:"note"`
		Repositories []model.Repository `json:"repositories"`
		Cases        []struct {
			Row      string         `json:"row"`
			Op       string         `json:"op"` // create, save
			Group    string         `json:"group"`
			Format   string         `json:"format"`
			Members  []string       `json:"members"`
			Refusals *[]wantRefusal `json:"refusals"`
		} `json:"cases"`
	}
	readJSON(t, "groups", &file)
	s := &model.Snapshot{Repositories: file.Repositories}
	counts := tally{}
	for i, c := range file.Cases {
		label := fmt.Sprintf("case %d (%s %s %s %v)", i, c.Row, c.Op, c.Group, c.Members)
		if stored := Exists(s, KindRepository, c.Group); stored != (c.Op == "save") {
			t.Fatalf("%s: a create names a new group, a save a stored one", label)
		}
		err := returns(t, label, func() error { return CheckMembers(s, c.Group, c.Format, c.Members) })
		checkRefusals(t, label, err, c.Refusals)
		if c.Refusals != nil {
			counts.add(c.Row, len(*c.Refusals) > 0)
		}
	}
	counts.check(t, "groups", "R3")
}

// The members of a group are read from the attribute group.memberNames,
// as a decoded list or as a list of strings.
func TestMemberNames(t *testing.T) {
	cases := []struct {
		attributes map[string]any
		want       []string
	}{
		{map[string]any{"group": map[string]any{"memberNames": []any{"a", "b"}}}, []string{"a", "b"}},
		{map[string]any{"group": map[string]any{"memberNames": []string{"c"}}}, []string{"c"}},
		{map[string]any{"group": map[string]any{"memberNames": []any{"d", 7, nil}}}, []string{"d"}},
		{map[string]any{"group": map[string]any{}}, nil},
		{map[string]any{}, nil},
		{nil, nil},
	}
	for _, c := range cases {
		got := memberNames(model.Repository{Name: "g", Type: "group", Attributes: c.attributes})
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%v: %v, want %v", c.attributes, got, c.want)
		}
	}
}
