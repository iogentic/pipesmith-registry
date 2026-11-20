package rest

import (
	"maps"

	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The capabilities (leg 6-system, contract S35 and S36): the list, the
// support answer of kinds.json, a save and a delete. Every change goes
// through admin.Change with the kinds capabilities and default role.

// capabilityXO is one CapabilityDTO.
type capabilityXO struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	Notes      string            `json:"notes"`
	Enabled    bool              `json:"enabled"`
	Properties map[string]string `json:"properties"`
}

// capabilityKinds are the kinds a capability change writes.
var capabilityKinds = []state.Kind{state.KindCapabilities, state.KindDefaultRole}

// Capabilities is GET /v1/capabilities: every stored capability by id. A
// property whose key holds a secret word is left out, though no record
// holds one (A11).
func Capabilities(env *Env) (Answer, error) {
	out := []capabilityXO{}
	if env.Snapshot != nil {
		for _, c := range env.Snapshot.Capabilities {
			props := map[string]string{}
			for key, value := range c.Properties {
				if !admin.SecretKey(key) {
					props[key] = value
				}
			}
			out = append(out, capabilityXO{ID: c.ID, Type: c.TypeID, Notes: c.Notes, Enabled: c.Enabled, Properties: props})
		}
	}
	return ok(out)
}

// CapabilitySupport is GET /api/capabilities/support.
func CapabilitySupport() (Answer, error) { return ok(admin.CapabilitySupport()) }

// SaveCapability is PUT /v1/capabilities/{id}: 204 after the save.
func SaveCapability(env *Env, w IdentityWrite, id string, body []byte) (Answer, error) {
	b, err := admin.DecodeCapability(body)
	if err != nil {
		return refused(err)
	}
	if b.Properties != nil {
		props := maps.Clone(*b.Properties)
		b.Properties = &props
	}
	_, err = admin.Change(w.Store, capabilityKinds, func(s *model.Snapshot) error {
		return admin.SaveCapability(s, id, b, env.permits)
	})
	if err != nil {
		return refused(err)
	}
	return noContent()
}

// DeleteCapability is DELETE /v1/capabilities/{id}: 204 after the delete.
// Secret files of the capability stay as they are (SY5).
func DeleteCapability(env *Env, w IdentityWrite, id string) (Answer, error) {
	_, err := admin.Change(w.Store, capabilityKinds, func(s *model.Snapshot) error {
		return admin.DeleteCapability(s, id)
	})
	if err != nil {
		return refused(err)
	}
	return noContent()
}
