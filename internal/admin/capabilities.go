package admin

import (
	"fmt"
	"maps"
	"slices"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The rules of a capability save and delete (leg 6-system, contract S35
// and S36, rulings SY4 to SY6 and SY8). Nothing creates a capability.

// CapabilityDefaultRole is the type whose effect is the default role: while
// a capability of it is enabled, its property role is the role the realm
// DefaultRole gives every signed in user.
const CapabilityDefaultRole = "defaultrole"

// capabilityRole is the property of the role of a defaultrole capability.
const capabilityRole = "role"

// CapabilityKind is a capability type of testdata/capabilities/kinds.json
// with its function class, its treatment, whether it has an effect in this
// server, and the reason.
type CapabilityKind struct {
	Type      string `json:"type"`
	Class     string `json:"class"`
	Treatment string `json:"treatment"`
	Effect    bool   `json:"effect"`
	Reason    string `json:"reason"`
}

// CapabilityClass is a function class of the phase 6 plan, section
// Capabilities, whose type id in an install is not known yet (an owner
// point, SY6).
type CapabilityClass struct {
	Class     string `json:"class"`
	Treatment string `json:"treatment"`
	Effect    bool   `json:"effect"`
	Reason    string `json:"reason"`
}

// The treatments of a capability type.
const (
	TreatmentSetting  = "setting"
	TreatmentReadOnly = "read only"
	TreatmentNone     = "no effect"
)

// capabilityKinds is the Go copy of the types of kinds.json; a test fails
// when the two differ.
var capabilityKinds = []CapabilityKind{
	{"baseurl", "base address", TreatmentNone, false,
		"The addresses in answers come from the address of each request; this server keeps no base address."},
	{"defaultrole", "default role", TreatmentSetting, true,
		"While enabled, its property role is the role that the realm DefaultRole gives every signed in user."},
	{"demo.webhook", "call of an outside service", TreatmentNone, false,
		"This server calls no outside service."},
}

// capabilityClasses is the Go copy of the classes of kinds.json.
var capabilityClasses = []CapabilityClass{
	{"audit switch", TreatmentSetting, false, "Stored; its effect is built and tested in phase 7."},
	{"interface settings", TreatmentSetting, false,
		"Stored; the idle timeout of an interface session stays the 30 minutes of N7 of the phase 3 plan until the type id is known."},
	{"node identity", TreatmentReadOnly, false, "The node id is read only."},
	{"scheduler", TreatmentReadOnly, false, "The scheduler is always on."},
	{"health check configuration", TreatmentNone, false, "Decision P12 of the plans."},
	{"fetch of remote welcome content", TreatmentNone, false, "This server calls no outside service."},
	{"upgrade helper", TreatmentNone, false, "Updates are phase 8."},
}

// CapabilityKinds are the types and the classes of kinds.json.
func CapabilityKinds() ([]CapabilityKind, []CapabilityClass) {
	return slices.Clone(capabilityKinds), slices.Clone(capabilityClasses)
}

func kindOf(typeID string) (CapabilityKind, bool) {
	at := slices.IndexFunc(capabilityKinds, func(k CapabilityKind) bool { return k.Type == typeID })
	if at < 0 {
		return CapabilityKind{}, false
	}
	return capabilityKinds[at], true
}

// Support is what this server does with one capability type.
type Support struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`
}

// CapabilitySupport is the answer of the support request: each type of
// kinds.json, supported when it has an effect, with its reason. A type
// missing from the map is not supported.
func CapabilitySupport() map[string]Support {
	out := make(map[string]Support, len(capabilityKinds))
	for _, k := range capabilityKinds {
		out[k.Type] = Support{Supported: k.Effect, Reason: k.Reason}
	}
	return out
}

// CapabilityBody is the body of a save (CapabilityDTO). An absent field
// keeps the stored value.
type CapabilityBody struct {
	ID         *string            `json:"id"`
	Type       *string            `json:"type"`
	Notes      *string            `json:"notes"`
	Enabled    *bool              `json:"enabled"`
	Properties *map[string]string `json:"properties"`
}

// DecodeCapability reads the body of a save: one JSON object of the keys
// of a CapabilityDTO with values of their types.
func DecodeCapability(body []byte) (CapabilityBody, error) {
	var b CapabilityBody
	if err := decodeStrict("a capability body", body, &b); err != nil {
		return CapabilityBody{}, err
	}
	return b, nil
}

func capabilityAt(s *model.Snapshot, id string) (int, error) {
	at := slices.IndexFunc(s.Capabilities, func(c model.Capability) bool { return c.ID == id })
	if at < 0 {
		return -1, &Refusal{Code: CodeNotFound, Message: "no capability with the id " + id}
	}
	return at, nil
}

// SaveCapability saves notes, enabled and properties of the stored
// capability of the id in s; id, type and origin stay. Refused, and
// travelling together: another id (400 at id); another type (SY6, 400 at
// type); a property key the secret word rule marks (SY5, 400 at
// properties.<key>); a switch on of a type without effect or unknown
// (SY4, A23, 400 not-offered at enabled), where a value the stored record
// holds is never refused (R6); and for a defaultrole capability that is
// enabled, a missing or unknown role (400 at properties.role) unless the
// stored capability is enabled with the same role. Then the effect: an
// enabled defaultrole capability sets the default role, after CheckGrant
// on that role when it changes (SY8, as I15); one switched off clears the
// default role while it is its role.
func SaveCapability(s *model.Snapshot, id string, b CapabilityBody, holds func(string) bool) error {
	at, err := capabilityAt(s, id)
	if err != nil {
		return err
	}
	stored := s.Capabilities[at]
	next := stored
	next.Properties = maps.Clone(stored.Properties)
	var list Refusals
	if b.ID != nil && *b.ID != id {
		list = append(list, &Refusal{Code: CodeNameChanged, Field: "id", Message: fmt.Sprintf(
			"the id of the capability %s cannot change", id)})
	}
	if b.Type != nil && *b.Type != stored.TypeID {
		message := fmt.Sprintf("the type of the capability %s is %s and cannot change", id, stored.TypeID)
		if _, known := kindOf(*b.Type); !known {
			message = fmt.Sprintf("the capability type %q is not known to this server; the type of the capability %s is %s",
				*b.Type, id, stored.TypeID)
		}
		list = append(list, &Refusal{Code: CodeCapabilityType, Field: "type", Message: message})
	}
	if b.Notes != nil {
		next.Notes = *b.Notes
	}
	if b.Enabled != nil {
		next.Enabled = *b.Enabled
	}
	if b.Properties != nil {
		next.Properties = maps.Clone(*b.Properties)
		if next.Properties == nil {
			next.Properties = map[string]string{}
		}
		for _, key := range slices.Sorted(maps.Keys(next.Properties)) {
			if secretKey([]string{key}) {
				list = append(list, &Refusal{Code: CodePropertySecret, Field: "properties." + key, Message: fmt.Sprintf(
					"the key %s names a secret; a capability of this server holds no secret", key)})
			}
		}
	}
	if kind, known := kindOf(stored.TypeID); next.Enabled && !stored.Enabled && (!known || !kind.Effect) {
		reason := "the type is not known to this server"
		if known {
			reason = kind.Reason
		}
		list = append(list, &Refusal{Code: CodeNotOffered, Field: "enabled", Message: fmt.Sprintf(
			"the capability type %s has no effect in this server: %s", stored.TypeID, reason)})
	}
	role, storedRole := next.Properties[capabilityRole], stored.Properties[capabilityRole]
	defaultRole := stored.TypeID == CapabilityDefaultRole
	if defaultRole && next.Enabled && !(stored.Enabled && role == storedRole) {
		switch {
		case role == "":
			list = append(list, &Refusal{Code: CodePropertyMissing, Field: "properties." + capabilityRole,
				Message: "an enabled default role capability needs the property role"})
		case !Exists(s, KindRole, role):
			list = append(list, &Refusal{Code: CodeRoleMissing, Field: "properties." + capabilityRole,
				Message: fmt.Sprintf("the role %s does not exist", role)})
		}
	}
	if err := list.err(); err != nil {
		return err
	}
	if defaultRole {
		switch {
		case next.Enabled && role != "" && role != s.DefaultRole:
			if err := CheckGrant(s, "every signed in user through the default role "+role, []string{role}, holds); err != nil {
				return err
			}
			s.DefaultRole = role
		case !next.Enabled && stored.Enabled && storedRole != "" && s.DefaultRole == storedRole:
			s.DefaultRole = ""
		}
	}
	s.Capabilities = slices.Clone(s.Capabilities)
	s.Capabilities[at] = next
	return nil
}

// DeleteCapability removes the capability of the id from s: 404 for none.
// An enabled defaultrole capability takes the default role with it while
// it is its role.
func DeleteCapability(s *model.Snapshot, id string) error {
	at, err := capabilityAt(s, id)
	if err != nil {
		return err
	}
	stored := s.Capabilities[at]
	if role := stored.Properties[capabilityRole]; stored.TypeID == CapabilityDefaultRole && stored.Enabled && role != "" &&
		s.DefaultRole == role {
		s.DefaultRole = ""
	}
	s.Capabilities = slices.Delete(slices.Clone(s.Capabilities), at, at+1)
	return nil
}
