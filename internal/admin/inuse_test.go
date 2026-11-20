package admin

import (
	"fmt"
	"slices"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

func TestInUseVectors(t *testing.T) {
	var file struct {
		Family       string             `json:"family"`
		Note         string             `json:"note"`
		Repositories []model.Repository `json:"repositories"`
		Cases        []struct {
			Row      string         `json:"row"`
			Op       string         `json:"op"` // store-delete, policy-delete, rule-delete, policy-format
			Name     string         `json:"name"`
			UsedBy   []string       `json:"usedBy"`
			Stored   string         `json:"stored"`
			Format   string         `json:"format"`
			Refusals *[]wantRefusal `json:"refusals"`
		} `json:"cases"`
	}
	readJSON(t, "inuse", &file)
	s := &model.Snapshot{Repositories: file.Repositories}
	counts := tally{}
	for i, c := range file.Cases {
		label := fmt.Sprintf("case %d (%s %s %s)", i, c.Row, c.Op, c.Name)
		var err error
		switch c.Op {
		case "store-delete":
			err = CheckStoreDelete(s, c.Name)
		case "policy-delete":
			err = CheckPolicyDelete(s, c.Name)
		case "rule-delete":
			err = CheckRuleDelete(c.Name, c.UsedBy)
		case "policy-format":
			err = CheckPolicyFormat(s, c.Name, c.Stored, c.Format)
		default:
			t.Fatalf("%s: unknown op", label)
		}
		checkRefusals(t, label, err, c.Refusals)
		if c.Refusals != nil {
			counts.add(c.Row, len(*c.Refusals) > 0)
		}
	}
	counts.check(t, "inuse", "R4", "ST2")
}

// The users of a store and of a policy are the repositories that name it,
// sorted, for the answers that list them.
func TestUsers(t *testing.T) {
	s := &model.Snapshot{Repositories: []model.Repository{
		{Name: "demo-b", Attributes: map[string]any{"storage": map[string]any{"blobStoreName": "demo-store"},
			"cleanup": map[string]any{"policyNames": []any{"demo-policy"}}}},
		{Name: "demo-a", Attributes: map[string]any{"storage": map[string]any{"blobStoreName": "demo-store"},
			"cleanup": map[string]any{"policyName": "demo-policy"}}},
		{Name: "demo-c", Attributes: map[string]any{"storage": map[string]any{"blobStoreName": "demo-store-2"}}},
		{Name: "demo-d"},
	}}
	if got := StoreUsers(s, "demo-store"); !slices.Equal(got, []string{"demo-a", "demo-b"}) {
		t.Errorf("users of demo-store: %v", got)
	}
	if got := PolicyUsers(s, "demo-policy"); !slices.Equal(got, []string{"demo-a", "demo-b"}) {
		t.Errorf("users of demo-policy: %v", got)
	}
	if got := StoreUsers(s, "demo-none"); len(got) != 0 {
		t.Errorf("users of an unused store: %v", got)
	}
}
