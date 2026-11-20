package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// formatCheck is the input of CheckPolicyFormat for a case of op policy.
type formatCheck struct {
	Policy string `json:"policy"`
	Stored string `json:"stored"`
	Format string `json:"format"`
}

func TestPreviewVectors(t *testing.T) {
	var file struct {
		Family       string             `json:"family"`
		Note         string             `json:"note"`
		Repositories []model.Repository `json:"repositories"`
		Cases        []struct {
			Row         string          `json:"row"`
			Op          string          `json:"op"` // target, policy, format, preview
			Format      string          `json:"format"`
			Stored      *string         `json:"stored"`
			Repository  string          `json:"repository"`
			Criteria    json.RawMessage `json:"criteria"`
			FormatCheck *formatCheck    `json:"formatCheck"`
			Refusals    *[]wantRefusal  `json:"refusals"`
		} `json:"cases"`
	}
	readJSON(t, "preview", &file)
	s := &model.Snapshot{Repositories: file.Repositories}
	counts := tally{}
	for i, c := range file.Cases {
		label := fmt.Sprintf("case %d (%s %s %q %q %s)", i, c.Row, c.Op, c.Format, c.Repository, c.Criteria)
		var err error
		switch c.Op {
		case "target":
			err = CheckPreviewTarget(s, c.Format, c.Repository)
		case "policy":
			err = CheckCriteria(decodeCriteria(t, label, c.Criteria, false), nil)
			if f := c.FormatCheck; f != nil {
				err = errors.Join(err, CheckPolicyFormat(s, f.Policy, f.Stored, f.Format))
			}
			err = PreviewPolicy(err)
		case "format":
			err = CheckFormatValue(c.Format, c.Stored)
		case "preview":
			policy := errors.Join(CheckCriteria(decodeCriteria(t, label, c.Criteria, false), nil), CheckFormatValue(c.Format, nil))
			err = errors.Join(PreviewPolicy(policy), CheckPreviewTarget(s, c.Format, c.Repository))
		default:
			t.Fatalf("%s: unknown op", label)
		}
		checkRefusals(t, label, err, c.Refusals)
		if c.Refusals != nil {
			counts.add(c.Row, len(*c.Refusals) > 0)
		}
	}
	counts.check(t, "preview", "Q8", "Q15")
}

// PreviewPolicy gives copies whose fields have the prefix policy., an empty
// field as *; nil stays nil, and an error that holds anything but refusals
// passes unchanged.
func TestPreviewPolicyForms(t *testing.T) {
	whole := &Refusal{Code: CodeRoleReadOnly, Field: "*", Message: "the policy as a whole"}
	bare := &Refusal{Code: CodeCriteriaMissing, Message: "no field"}
	named := &Refusal{Code: CodePolicyFormat, Field: "format", Message: "a field"}
	var got []*Refusal
	if !collect(PreviewPolicy(Refusals{whole, bare, named}), &got) || len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	for i, want := range []string{"policy.*", "policy.*", "policy.format"} {
		if got[i].Field != want || got[i].Code == "" || got[i].Message == "" {
			t.Errorf("refusal %d: %+v, want the field %s", i, got[i], want)
		}
	}
	if whole.Field != "*" || bare.Field != "" || named.Field != "format" {
		t.Error("PreviewPolicy changed the refusals it was given")
	}
	other := errors.New("disk full")
	mixed := errors.Join(named, other)
	if PreviewPolicy(nil) != nil || PreviewPolicy(other) != other || PreviewPolicy(mixed) != mixed {
		t.Error("nil and an error that holds anything but refusals pass unchanged")
	}
	if status, _ := Answer(PreviewPolicy(mixed)); status != 500 {
		t.Errorf("Answer gives %d for a refusal beside another error, want 500", status)
	}
}
