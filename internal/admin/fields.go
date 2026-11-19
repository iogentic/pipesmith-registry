package admin

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// PassThroughField is a field of a contract body that this server stores
// and returns unchanged and does not act on (decision A23, finding F14):
// the kind of record, the dot path, the reason in a few words (what this
// server does not have), and the values that would switch on that
// function. A field without such values is kept and never refused.
type PassThroughField struct {
	Kind    Kind
	Path    string
	Reason  string
	Refused []any
}

// passThroughFields is the committed table of decision A23. The refused
// fields are the ones the contract names as not offered.
var passThroughFields = []PassThroughField{
	{KindRepository, "docker.v1Enabled", "no first Docker protocol version", []any{true}},
	{KindRepository, "httpClient.connection.useTrustStore", "no trust store", []any{true}},
	{KindRepository, "httpClient.authentication.type", "no NTLM sign in toward an upstream", []any{"ntlm"}},
	{KindRepository, "replication.preemptivePullEnabled", "no replication", []any{true}},
	{KindLDAPConnection, "useTrustStore", "no trust store", []any{true}},
	{KindRepository, "httpClient.authentication.ntlmHost", "no NTLM sign in toward an upstream", nil},
	{KindRepository, "httpClient.authentication.ntlmDomain", "no NTLM sign in toward an upstream", nil},
	{KindRepository, "replication.assetPathRegex", "no replication", nil},
	{KindRepository, "component.proprietaryComponents", "no firewall for proprietary components", nil},
	{KindRepository, "npm.removeQuarantined", "no quarantine of npm packages", nil},
	{KindRepository, "dockerProxy.indexType", "no Docker index search", nil},
	{KindRepository, "dockerProxy.indexUrl", "no Docker index search", nil},
	{KindRepository, "dockerProxy.cacheForeignLayers", "no cache of foreign layers", nil},
	{KindRepository, "dockerProxy.foreignLayerUrlWhitelist", "no cache of foreign layers", nil},
	{KindRepository, "httpClient.connection.enableCookies", "no cookies toward an upstream", nil},
	{KindRepository, "httpClient.connection.enableCircularRedirects", "no circular redirects toward an upstream", nil},
}

// PassThroughFields lists the fields of decision A23.
func PassThroughFields() []PassThroughField {
	list := slices.Clone(passThroughFields)
	for i := range list {
		list[i].Refused = slices.Clone(list[i].Refused)
	}
	return list
}

// CheckPassThrough refuses, with 400 at its path, a value of a field of the
// table that would switch on a function this server does not have, unless
// the stored record holds the same value. body is the decoded request body
// in the shape of the contract. stored names the record of the snapshot
// that a save changes and is empty for a create.
func CheckPassThrough(s *model.Snapshot, kind Kind, stored string, body map[string]any) error {
	record := storedRecord(s, kind, stored)
	var list Refusals
	for _, f := range passThroughFields {
		if f.Kind != kind {
			continue
		}
		value, set := valueAt(body, f.Path)
		if !set || !slices.ContainsFunc(f.Refused, func(r any) bool { return reflect.DeepEqual(r, value) }) {
			continue
		}
		if held, ok := valueAt(record, f.Path); ok && reflect.DeepEqual(held, value) {
			continue
		}
		list = append(list, &Refusal{Code: CodeNotOffered, Field: f.Path, Message: fmt.Sprintf(
			"the %s field %s cannot be %v, because this server has %s", kind.noun(), f.Path, value, f.Reason)})
	}
	return list.err()
}

// storedRecord is the record of the kind and name in the snapshot, in the
// shape of the contract, or nil: the attributes of a repository, whose HTTP
// client is read under httpClient or else under httpclient, the key of the
// import, as internal/rest reads it; or the fields of an LDAP connection.
// The snapshot is not changed.
func storedRecord(s *model.Snapshot, kind Kind, name string) map[string]any {
	if name == "" {
		return nil
	}
	switch kind {
	case KindRepository:
		for _, r := range s.Repositories {
			if r.Name != name {
				continue
			}
			out := maps.Clone(r.Attributes)
			if client, found := out["httpclient"]; found {
				if _, contract := out["httpClient"]; !contract {
					out["httpClient"] = client
				}
			}
			return out
		}
	case KindLDAPConnection:
		for _, l := range s.LDAP {
			if l.Name == name {
				var out map[string]any
				data, err := json.Marshal(l)
				if err != nil || json.Unmarshal(data, &out) != nil {
					return nil
				}
				return out
			}
		}
	}
	return nil
}

// valueAt gives the value at the dot path of a decoded object, and whether
// the key is there.
func valueAt(m map[string]any, path string) (any, bool) {
	parent, key := parentOf(m, path)
	if parent == nil {
		return nil, false
	}
	value, ok := parent[key]
	return value, ok
}
