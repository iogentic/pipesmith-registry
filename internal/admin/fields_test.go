package admin

import (
	"fmt"
	"slices"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

func TestPassThroughVectors(t *testing.T) {
	var file struct {
		Family       string             `json:"family"`
		Note         string             `json:"note"`
		Repositories []model.Repository `json:"repositories"`
		LDAP         []model.LDAPServer `json:"ldap"`
		Cases        []struct {
			Row      string         `json:"row"`
			Kind     Kind           `json:"kind"`
			Body     map[string]any `json:"body"`
			Stored   string         `json:"stored"`
			Refusals *[]wantRefusal `json:"refusals"`
		} `json:"cases"`
	}
	readJSON(t, "fields", &file)
	s := &model.Snapshot{Repositories: file.Repositories, LDAP: file.LDAP}
	before := fmt.Sprint(s.Repositories, s.LDAP)
	counts, rows := tally{}, tally{}
	for i, c := range file.Cases {
		label := fmt.Sprintf("case %d (%s %s %q %v)", i, c.Row, c.Kind, c.Stored, c.Body)
		if c.Stored != "" && !Exists(s, c.Kind, c.Stored) {
			t.Fatalf("%s: the snapshot holds no stored record of that name", label)
		}
		checkRefusals(t, label, CheckPassThrough(s, c.Kind, c.Stored, c.Body), c.Refusals)
		if c.Refusals == nil {
			continue
		}
		counts.add(c.Row, len(*c.Refusals) > 0)
		for _, f := range PassThroughFields() {
			value, set := valueAt(c.Body, f.Path)
			if f.Kind != c.Kind || !set || value == nil {
				continue
			}
			refused := slices.ContainsFunc(*c.Refusals, func(w wantRefusal) bool { return w.Field == f.Path })
			if refused && len(f.Refused) == 0 {
				t.Errorf("%s: the field %s is kept and never refused", label, f.Path)
			}
			rows.add(string(f.Kind)+" "+f.Path, refused)
		}
	}
	counts.check(t, "fields", "R12")
	if fmt.Sprint(s.Repositories, s.LDAP) != before {
		t.Error("CheckPassThrough changed the snapshot")
	}
	for _, f := range PassThroughFields() {
		n := rows[string(f.Kind)+" "+f.Path]
		switch {
		case n == nil || n[0] == 0:
			t.Errorf("fields.json: the field %s %s has no case that keeps a value", f.Kind, f.Path)
		case len(f.Refused) > 0 && n[1] == 0:
			t.Errorf("fields.json: the field %s %s has no case that refuses a value", f.Kind, f.Path)
		}
	}
}

// Every row of the table names its kind, its path and a reason; the fields
// that the contract names as not offered are the ones refused.
func TestPassThroughTable(t *testing.T) {
	fields := PassThroughFields()
	var refused []string
	seen := map[string]bool{}
	for _, f := range fields {
		key := string(f.Kind) + " " + f.Path
		if f.Kind == "" || f.Path == "" || f.Reason == "" || seen[key] {
			t.Errorf("row %+v: no kind, path or reason, or a row of the same field before it", f)
		}
		seen[key] = true
		if len(f.Refused) > 0 {
			refused = append(refused, key)
		}
	}
	want := []string{"repository docker.v1Enabled", "repository httpClient.connection.useTrustStore",
		"repository httpClient.authentication.type", "repository replication.preemptivePullEnabled",
		"ldap-connection useTrustStore"}
	if !slices.Equal(refused, want) {
		t.Errorf("refused fields %q, want %q", refused, want)
	}
	if len(fields) != len(want)+11 {
		t.Errorf("%d rows, want %d refused and 11 kept", len(fields), len(want))
	}
}
