package rest

import (
	"net/http"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// segments name a format in the repository paths of the REST API; a
// format not listed is its own segment.
var segments = map[string]string{"maven2": "maven"}

func segmentOf(format string) string {
	if s, ok := segments[format]; ok {
		return s
	}
	return format
}

// repositoryXO is RepositoryXO of GET /v1/repositories.
type repositoryXO struct {
	Name       string         `json:"name"`
	Format     string         `json:"format"`
	Type       string         `json:"type"`
	URL        string         `json:"url"`
	Attributes map[string]any `json:"attributes"`
}

func (env *Env) record(name string) *model.Repository {
	if env.Snapshot == nil {
		return nil
	}
	for i := range env.Snapshot.Repositories {
		if env.Snapshot.Repositories[i].Name == name {
			return &env.Snapshot.Repositories[i]
		}
	}
	return nil
}

func section(attributes map[string]any, key string) map[string]any {
	m, _ := attributes[key].(map[string]any)
	return m
}

func (env *Env) repositoryXO(r repos.Repo) repositoryXO {
	attributes := map[string]any{}
	if r.Type == "proxy" {
		var remote any
		if rec := env.record(r.Name); rec != nil {
			remote = section(rec.Attributes, "proxy")["remoteUrl"]
		}
		if remote == nil && r.RemoteURL != nil {
			remote = *r.RemoteURL
		}
		if remote != nil {
			attributes["proxy"] = map[string]any{"remoteUrl": remote}
		}
	}
	return repositoryXO{Name: r.Name, Format: r.Format, Type: r.Type, URL: env.url(r.Name), Attributes: attributes}
}

// Repositories is GET /v1/repositories: every repository the subject may
// browse, in the order of the read path.
func Repositories(env *Env) (Answer, error) {
	list := []repositoryXO{}
	for _, r := range env.Repositories.All() {
		if env.mayBrowse(r) {
			list = append(list, env.repositoryXO(r))
		}
	}
	return ok(list)
}

// Repository is GET /v1/repositories/{name}; the server checks the view
// permission first.
func Repository(env *Env, name string) (Answer, error) {
	r, found := env.Repositories.Get(name)
	if !found {
		return refuse(http.StatusNotFound, "repository "+name+" not found")
	}
	return ok(env.repositoryXO(r))
}

// RepositoryDetail is GET /v1/repositories/{segment}/{type}/{name}: the
// settings of one repository, 404 when segment and type do not fit it.
func RepositoryDetail(env *Env, segment, kind, name string) (Answer, error) {
	r, found := env.Repositories.Get(name)
	if !found || segmentOf(r.Format) != segment || r.Type != kind {
		return refuse(http.StatusNotFound, "repository "+name+" not found as "+segment+" "+kind)
	}
	return ok(env.settings(r))
}

// RepositorySettings is GET /v1/repositorySettings: the settings of every
// repository the subject may administer, 403 when there is none.
func RepositorySettings(env *Env) (Answer, error) {
	list := []map[string]any{}
	for _, r := range env.Repositories.All() {
		if env.permits(AdminPermission(r.Format, r.Name, "read")) {
			list = append(list, env.settings(r))
		}
	}
	if len(list) == 0 {
		return refuse(http.StatusForbidden, "you may administer no repository")
	}
	return ok(list)
}

// settings is the stored settings of a repository in the REST shape: the
// attributes of the import with the two keys whose database name differs
// from the REST name renamed, every secret left out, and the members of a
// group from the read path when the attributes do not name them.
func (env *Env) settings(r repos.Repo) map[string]any {
	out := map[string]any{}
	online := true
	if rec := env.record(r.Name); rec != nil {
		online = rec.Online
		for key, value := range rec.Attributes {
			out[key] = clean(value)
		}
	}
	if client, found := out["httpclient"]; found {
		if _, rest := out["httpClient"]; !rest {
			out["httpClient"] = client
		}
		delete(out, "httpclient")
	}
	if cleanup, _ := out["cleanup"].(map[string]any); cleanup != nil {
		if names, found := cleanup["policyName"]; found {
			if _, rest := cleanup["policyNames"]; !rest {
				cleanup["policyNames"] = names
			}
			delete(cleanup, "policyName")
		}
	}
	if r.Type == "group" {
		group, _ := out["group"].(map[string]any)
		if group == nil {
			group = map[string]any{}
			out["group"] = group
		}
		if _, found := group["memberNames"]; !found {
			members := make([]any, len(r.Members))
			for i, m := range r.Members {
				members[i] = m
			}
			group["memberNames"] = members
		}
	}
	out["name"] = r.Name
	out["format"] = r.Format
	out["type"] = r.Type
	out["url"] = env.url(r.Name)
	out["online"] = online
	return out
}

// secret is a key whose value never leaves the process: passwords, bearer
// tokens and other secrets of an upstream or a store.
func secret(key string) bool {
	k := strings.ToLower(key)
	return strings.Contains(k, "password") || strings.Contains(k, "secret") || k == "bearertoken" || k == "token"
}

// clean is a deep copy of a stored value without its secret keys, so that
// the answer never changes the snapshot.
func clean(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if !secret(key) {
				out[key] = clean(item)
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = clean(item)
		}
		return out
	default:
		return v
	}
}
