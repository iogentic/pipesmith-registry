package admin

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/authz"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

func TestCapabilityVectors(t *testing.T) {
	var file struct {
		Family       string             `json:"family"`
		Note         string             `json:"note"`
		Capabilities []model.Capability `json:"capabilities"`
		Roles        []model.Role       `json:"roles"`
		DefaultRole  string             `json:"defaultRole"`
		Cases        []struct {
			Row         string         `json:"row"`
			Op          string         `json:"op"`
			ID          string         `json:"id"`
			Body        map[string]any `json:"body"`
			Raw         *string        `json:"raw"`
			Held        []string       `json:"held"`
			Stored      *string        `json:"storedDefaultRole"`
			DefaultRole *string        `json:"defaultRole"`
			Refusals    *[]wantRefusal `json:"refusals"`
		} `json:"cases"`
	}
	readJSON(t, "capabilities", &file)
	counts := tally{}
	for i, c := range file.Cases {
		label := fmt.Sprintf("case %d (%s %s %s)", i, c.Row, c.Op, c.ID)
		s := &model.Snapshot{Capabilities: cloneCapabilities(file.Capabilities), Roles: file.Roles, DefaultRole: file.DefaultRole}
		if c.Stored != nil {
			s.DefaultRole = *c.Stored
		}
		before := &model.Snapshot{Capabilities: cloneCapabilities(s.Capabilities), Roles: s.Roles, DefaultRole: s.DefaultRole}
		holds := func(p string) bool {
			return slices.ContainsFunc(c.Held, func(g string) bool { return authz.Implies(g, p) })
		}
		var err error
		switch c.Op {
		case "save":
			data, _ := json.Marshal(c.Body)
			if c.Raw != nil {
				data = []byte(*c.Raw)
			}
			var b CapabilityBody
			if b, err = DecodeCapability(data); err == nil {
				err = SaveCapability(s, c.ID, b, holds)
			}
			if err == nil {
				checkSaved(t, label, before, s, c.ID, c.Body)
			}
		case "delete":
			err = DeleteCapability(s, c.ID)
			if err == nil && slices.ContainsFunc(s.Capabilities, func(x model.Capability) bool { return x.ID == c.ID }) {
				t.Errorf("%s: the capability stays", label)
			}
		default:
			t.Fatalf("%s: unknown op", label)
		}
		if err != nil && c.Refusals != nil && len(*c.Refusals) == 0 {
			t.Errorf("%s: %v", label, err)
		}
		checkRefusals(t, label, err, c.Refusals)
		if err == nil && c.DefaultRole != nil && s.DefaultRole != *c.DefaultRole {
			t.Errorf("%s: default role %q, want %q", label, s.DefaultRole, *c.DefaultRole)
		}
		if err != nil && !reflect.DeepEqual(s, before) {
			t.Errorf("%s: a refusal changed the records", label)
		}
		if err == nil && c.DefaultRole == nil && s.DefaultRole != before.DefaultRole {
			t.Errorf("%s: the default role changed to %q", label, s.DefaultRole)
		}
		counts.add(c.Row, c.Refusals != nil && len(*c.Refusals) > 0)
	}
	counts.check(t, "capabilities", "body", "SY4", "SY5", "SY6", "SY8", "role", "delete")
}

// checkSaved compares the saved capability with the body: notes, enabled
// and properties as sent, id, type and origin as stored.
func checkSaved(t *testing.T, label string, before, after *model.Snapshot, id string, body map[string]any) {
	t.Helper()
	at := slices.IndexFunc(before.Capabilities, func(x model.Capability) bool { return x.ID == id })
	now := slices.IndexFunc(after.Capabilities, func(x model.Capability) bool { return x.ID == id })
	if at < 0 || now < 0 {
		t.Errorf("%s: the capability is gone", label)
		return
	}
	want := before.Capabilities[at]
	if v, ok := body["notes"].(string); ok {
		want.Notes = v
	}
	if v, ok := body["enabled"].(bool); ok {
		want.Enabled = v
	}
	if v, ok := body["properties"].(map[string]any); ok {
		want.Properties = map[string]string{}
		for k, x := range v {
			want.Properties[k] = x.(string)
		}
	}
	if got := after.Capabilities[now]; !reflect.DeepEqual(got, want) {
		t.Errorf("%s: saved %+v, want %+v", label, got, want)
	}
}

func cloneCapabilities(list []model.Capability) []model.Capability {
	out := slices.Clone(list)
	for i := range out {
		props := map[string]string{}
		for k, v := range out[i].Properties {
			props[k] = v
		}
		out[i].Properties = props
	}
	return out
}

// The support answer names each type of the table with its treatment: a
// type with an effect is supported, every other one not, each with a
// reason; no type outside the table is named.
func TestCapabilitySupport(t *testing.T) {
	support := CapabilitySupport()
	kinds, classes := CapabilityKinds()
	if len(support) != len(kinds) || len(kinds) < 3 || len(classes) < 7 {
		t.Fatalf("%d types in the answer, %d in the table, %d classes", len(support), len(kinds), len(classes))
	}
	for _, k := range kinds {
		s, ok := support[k.Type]
		if !ok || s.Supported != k.Effect || strings.TrimSpace(s.Reason) == "" || s.Reason != k.Reason {
			t.Errorf("%s: %+v for %+v", k.Type, s, k)
		}
	}
	if !support[CapabilityDefaultRole].Supported || support["baseurl"].Supported || support["demo.webhook"].Supported {
		t.Fatalf("%+v", support)
	}
}
