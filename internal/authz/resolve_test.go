package authz_test

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/authz"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// snapshot holds invented roles and privileges: a chain of three nested
// roles, a pair that contains each other, a role that names a missing role,
// a stored row that replaces a built in role, and one row of each kind.
func snapshot() *model.Snapshot {
	return &model.Snapshot{
		Roles: []model.Role{
			{ID: "demo-chain-1", Privileges: []string{"demo-settings-read"}, Roles: []string{"demo-chain-2"}},
			{ID: "demo-chain-2", Privileges: []string{"demo-raw-all"}, Roles: []string{"demo-chain-3"}},
			{ID: "demo-chain-3", Privileges: []string{"nx-repository-view-maven2-demo-maven-read"}},
			{ID: "demo-loop-a", Privileges: []string{"demo-settings-read"}, Roles: []string{"demo-loop-b"}},
			{ID: "demo-loop-b", Privileges: []string{"demo-script-run"}, Roles: []string{"demo-loop-a"}},
			{ID: "demo-dangling", Privileges: []string{"demo-missing-privilege", "demo-npm-admin"},
				Roles: []string{"demo-missing-role"}},
			{ID: "demo-selector", Privileges: []string{"demo-csel"}},
			{ID: "nx-anonymous", Privileges: []string{"nx-repository-view-raw-demo-raw-browse"}},
			{ID: "demo-generated", Privileges: []string{
				"nx-repository-view-maven2-demo-maven-*", "nx-repository-admin-npm-demo-npm-edit",
				"nx-repository-admin-npm-demo-npm-add", "nx-repository-view-maven2-demo-gone-read",
				"nx-settings-update", "nx-userschangepw", "nx-component-upload", "nx-tasks-run",
				"nx-script-*-browse", "nx-repository-view-*-*-add", "nx-repository-admin-*-*-*",
				"nx-repository-view-npm-*-browse"}},
		},
		Privileges: []model.Privilege{
			{Name: "demo-settings-read", Type: "application",
				Properties: map[string]string{"domain": "settings", "actions": "read"}},
			{Name: "demo-raw-all", Type: "wildcard",
				Properties: map[string]string{"pattern": "nexus:repository-view:raw:demo-raw:*"}},
			{Name: "demo-npm-admin", Type: "repository-admin",
				Properties: map[string]string{"format": "npm", "repository": "demo-npm", "actions": "*"}},
			{Name: "demo-script-run", Type: "script",
				Properties: map[string]string{"name": "demo-script", "actions": "run"}},
			{Name: "demo-csel", Type: "repository-content-selector",
				Properties: map[string]string{"contentSelector": "demo-selector", "repository": "*", "actions": "read"}},
			{Name: "demo-bad", Type: "repository-view",
				Properties: map[string]string{"format": "maven2", "repository": "a,b", "actions": "read"}},
		},
		Repositories: []model.Repository{
			{Name: "demo-maven", Format: "maven2", Type: "hosted"},
			{Name: "demo-raw", Format: "raw", Type: "hosted"},
			{Name: "demo-npm", Format: "npm", Type: "proxy"},
		},
	}
}

func TestPermissionsChain(t *testing.T) {
	r := authz.NewResolver(snapshot())
	want := []string{
		"nexus:repository-view:maven2:demo-maven:read",
		"nexus:repository-view:raw:demo-raw:*",
		"nexus:settings:read",
	}
	if got := r.Permissions([]string{"demo-chain-1"}); !slices.Equal(got, want) {
		t.Errorf("chain: %q, want %q", got, want)
	}
	if got := r.Roles([]string{"demo-chain-1"}); !slices.Equal(got, []string{"demo-chain-1", "demo-chain-2", "demo-chain-3"}) {
		t.Errorf("roles of the chain: %q", got)
	}
}

func TestPermissionsLoopAndMissing(t *testing.T) {
	r := authz.NewResolver(snapshot())
	want := []string{"nexus:script:demo-script:run", "nexus:settings:read"}
	for _, start := range []string{"demo-loop-a", "demo-loop-b"} {
		if got := r.Permissions([]string{start}); !slices.Equal(got, want) {
			t.Errorf("loop from %s: %q, want %q", start, got, want)
		}
	}
	if got := r.Permissions([]string{"demo-dangling", "demo-no-such-role"}); !slices.Equal(got,
		[]string{"nexus:repository-admin:npm:demo-npm:*"}) {
		t.Errorf("dangling: %q", got)
	}
	if got := r.Roles([]string{"demo-dangling", "demo-no-such-role"}); !slices.Equal(got, []string{"demo-dangling"}) {
		t.Errorf("roles of dangling: %q", got)
	}
	if got := r.Permissions(nil); got == nil || len(got) != 0 {
		t.Errorf("no roles: %#v, want an empty list", got)
	}
}

// N14: a content selector grants nothing; the resolver counts it.
func TestContentSelectorGrantsNothing(t *testing.T) {
	r := authz.NewResolver(snapshot())
	if got := r.Permissions([]string{"demo-selector"}); len(got) != 0 {
		t.Errorf("selector role: %q", got)
	}
	ignored := r.Ignored()
	if ignored["repository-content-selector"] != 1 || ignored["repository-view"] != 1 || len(ignored) != 2 {
		t.Errorf("ignored: %v", ignored)
	}
}

// N15: built in roles and privileges, and the generated privileges of the
// repositories, where no stored row has the same id.
func TestBuiltInAndGenerated(t *testing.T) {
	r := authz.NewResolver(snapshot())
	if got := r.Permissions([]string{"nx-admin"}); !slices.Equal(got, []string{"nexus:*"}) {
		t.Errorf("nx-admin: %q", got)
	}
	// the stored nx-anonymous replaces the built in one
	if got := r.Permissions([]string{"nx-anonymous"}); !slices.Equal(got,
		[]string{"nexus:repository-view:raw:demo-raw:browse"}) {
		t.Errorf("stored nx-anonymous: %q", got)
	}
	want := []string{
		"nexus:component:create",
		"nexus:repository-admin:*:*:*",
		"nexus:repository-admin:npm:demo-npm:edit",
		"nexus:repository-view:*:*:add",
		"nexus:repository-view:maven2:demo-maven:*",
		"nexus:repository-view:npm:*:browse",
		"nexus:script:*:browse,read",
		"nexus:settings:update,read",
		"nexus:tasks:start,stop",
		"nexus:userschangepw:create,read",
	}
	if got := r.Permissions([]string{"demo-generated"}); !slices.Equal(got, want) {
		t.Errorf("generated:\n got %q\nwant %q", got, want)
	}
}

func TestBuiltInAnonymousRole(t *testing.T) {
	s := snapshot()
	s.Roles = slices.DeleteFunc(s.Roles, func(r model.Role) bool { return r.ID == "nx-anonymous" })
	r := authz.NewResolver(s)
	want := []string{
		"nexus:repository-view:*:*:browse",
		"nexus:repository-view:*:*:read",
		"nexus:search:read",
	}
	if got := r.Permissions([]string{"nx-anonymous"}); !slices.Equal(got, want) {
		t.Errorf("built in nx-anonymous: %q, want %q", got, want)
	}
}

// A stored privilege row with the id of a generated one is the more specific
// statement and wins.
func TestStoredPrivilegeWins(t *testing.T) {
	s := snapshot()
	s.Privileges = append(s.Privileges, model.Privilege{Name: "nx-all", Type: "application",
		Properties: map[string]string{"domain": "search", "actions": "read"}})
	r := authz.NewResolver(s)
	if got := r.Permissions([]string{"nx-admin"}); !slices.Equal(got, []string{"nexus:search:read"}) {
		t.Errorf("nx-admin over a stored nx-all: %q", got)
	}
}

func TestResolverConcurrent(t *testing.T) {
	r := authz.NewResolver(snapshot())
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := r.Permissions([]string{"demo-chain-1", fmt.Sprint("demo-loop-", "ab"[i%2:i%2+1])})
			if len(got) != 4 {
				t.Errorf("goroutine %d: %q", i, got)
			}
		}()
	}
	wg.Wait()
}
