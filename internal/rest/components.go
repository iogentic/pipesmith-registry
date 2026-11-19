package rest

import (
	"bytes"
	"encoding/base64"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/index"
)

// The ids of the REST writes. Both stay the stateless forms of the read
// path (decision E5 of the phase 4 execution plan): an asset id names its
// repository and path (AssetID); a component id names its repository and
// the first 128 bits of the SHA-256 of the coordinates its format gives it
// (componentID of the search). A component id round trips by a walk of its
// repository: the component is every path whose coordinates give that key.

// componentKeyBytes is the length of the key of a component id.
const componentKeyBytes = 16

// IDRepository is the repository an asset id or a component id names;
// false for a text none of them can be.
func IDRepository(id string) (string, bool) {
	data, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", false
	}
	i := bytes.IndexByte(data, ':')
	if i <= 0 || !utf8.Valid(data[:i]) {
		return "", false
	}
	return string(data[:i]), true
}

// DecodeComponentID is the repository and the key of a component id, false
// for an id componentID cannot have made.
func DecodeComponentID(id string) (string, []byte, bool) {
	data, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", nil, false
	}
	i := bytes.IndexByte(data, ':')
	if i <= 0 || !utf8.Valid(data[:i]) || len(data)-i-1 != componentKeyBytes || base64.RawURLEncoding.EncodeToString(data) != id {
		return "", nil, false
	}
	return string(data[:i]), data[i+1:], true
}

// Component is one component of a repository: its coordinates as its
// format gives them and the paths of its assets, in path order.
type Component struct {
	Repository, Format   string
	Group, Name, Version string
	HasGroup, HasVersion bool
	Paths                []string
}

// FindComponent is the component of a component id in the repository it
// names: every path whose coordinates give the id. Nil when the repository
// holds none; a group or an id that does not decode gives nil as well, the
// caller refuses those first.
func FindComponent(env *Env, id string) (*Component, error) {
	repoName, _, valid := DecodeComponentID(id)
	if !valid || env.Catalog == nil {
		return nil, nil
	}
	repo, found := env.Repositories.Get(repoName)
	if !found || repo.Type == "group" {
		return nil, nil
	}
	var c *Component
	err := env.Catalog.walk(env.Context, repo.Name, prefilter{}, env.Index.Lookup, func(a index.Asset) error {
		coords, ok := componentOf(repo.Format, a.Path)
		if !ok {
			return nil
		}
		key := [4]string{repo.Name, coords.group, coords.name, coords.version}
		if componentID(key) != id {
			return nil
		}
		if c == nil {
			c = &Component{Repository: repo.Name, Format: repo.Format, Group: coords.group, Name: coords.name, Version: coords.version,
				HasGroup: coords.hasGroup, HasVersion: coords.hasVersion}
		}
		c.Paths = append(c.Paths, a.Path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ComponentID is the id of the component of a path of a repository of a
// format, as the search gives it; false for a path that belongs to no
// component.
func ComponentID(repo, format, path string) (string, bool) {
	coords, ok := componentOf(format, path)
	if !ok {
		return "", false
	}
	return componentID([4]string{repo, coords.group, coords.name, coords.version}), true
}
