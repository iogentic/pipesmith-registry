// Package rest builds the REST read answers of the interface screens in the
// shape of the old product: repositories and their settings, blob stores,
// privileges, roles, users, user sources, anonymous access, realms, LDAP
// connections, the health checks, the read only state, one asset by id and
// the component search. Every answer is built from the snapshot of the
// identity and configuration state, the repositories and the index of the
// read path. Nothing here writes, and nothing here checks the permission of
// a route: the server does that before it calls a function. A list shows
// only what Env.Permits allows.
package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/mirosso25/pipesmith-registry/internal/index"
	"github.com/mirosso25/pipesmith-registry/internal/model"
	"github.com/mirosso25/pipesmith-registry/internal/repos"
)

// UnknownFormat is the format of the permission asked for a repository
// that does not exist. Only a grant with a wildcard format implies it, so
// only such a subject learns that the name is free.
const UnknownFormat = "-"

// Index is what the answers read of the index.
type Index interface {
	Lookup(repo, path string) (*index.Asset, error)
	StoreStats() ([]index.StoreStat, error)
}

// Store is one store the read path attaches.
type Store struct {
	Name, Kind, Root string
	Available        bool
}

// Env is what one answer is built from.
type Env struct {
	Context      context.Context
	Snapshot     *model.Snapshot
	Repositories *repos.Repositories // the repositories the read path serves
	Index        Index
	Catalog      *Catalog // the search reads it; nil answers 503
	Stores       []Store
	// Permits reports whether the subject of the request holds a permission.
	Permits func(permission string) bool
	// Base is scheme and host of the request, for the url fields.
	Base string
}

// Answer is a JSON answer of the old product's shape.
type Answer struct {
	Status int
	Body   any
}

// JSON is the body as the server sends it.
func (a Answer) JSON() ([]byte, error) {
	return json.Marshal(a.Body)
}

func ok(body any) (Answer, error) { return Answer{Status: http.StatusOK, Body: body}, nil }

type messageBody struct {
	Message string `json:"message"`
}

// refuse is an answer with {"message"} in the form the contract reads.
func refuse(status int, message string) (Answer, error) {
	return Answer{Status: status, Body: messageBody{message}}, nil
}

type refusalBody struct {
	Message string `json:"message"`
	Area    string `json:"area"`
}

// NotImplemented is the 501 of a function this server does not have yet,
// {"message", "area"} as the contract reads it.
func NotImplemented(area, what string) Answer {
	return Answer{Status: http.StatusNotImplemented,
		Body: refusalBody{Message: what + " is not available in this server yet", Area: area}}
}

// ViewPermission is the permission on the content of a repository.
func ViewPermission(format, repo, action string) string {
	return "nexus:repository-view:" + format + ":" + repo + ":" + action
}

// AdminPermission is the permission on the settings of a repository.
func AdminPermission(format, repo, action string) string {
	return "nexus:repository-admin:" + format + ":" + repo + ":" + action
}

func (env *Env) permits(permission string) bool {
	return env.Permits != nil && env.Permits(permission)
}

// mayBrowse is the view permission of a repository with the action browse.
func (env *Env) mayBrowse(r repos.Repo) bool {
	return env.permits(ViewPermission(r.Format, r.Name, "browse"))
}

// url is the address of a repository below the base of the request.
func (env *Env) url(repo string) string {
	return env.Base + "/repository/" + url.PathEscape(repo)
}

// pathURL escapes each segment of a path and keeps the slashes.
func pathURL(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}
