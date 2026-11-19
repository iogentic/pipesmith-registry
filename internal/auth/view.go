package auth

import (
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/authz"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// view is the identity state of one load of the snapshot, with what every
// check reads of it. Its records never change after newView, only the set
// rehashed grows; a reload installs a new view (reload.go).
type view struct {
	gen      int64 // the generation of the store the snapshot was loaded at
	snap     *model.Snapshot
	resolver *authz.Resolver
	local    map[string]model.User
	// spaced gives the stored id of a local user whose id has outer white
	// space by the name without it, the name that user types (review R6);
	// a name that is an id of its own or of two such users is not held
	spaced    map[string]string
	realms    map[string]bool // the ids of the stored order
	repos     map[string]model.Repository
	localOn   bool
	ldapOn    bool
	checked   []string // the realms a check of a user name and password asks, in its order
	dirs      []Directory
	anonymous []string // permissions of the anonymous user
	// defaultRole is the role every signed in subject holds besides its own:
	// the default role of the snapshot while RealmDefaultRole is active
	defaultRole string
	// the users whose unsalted form a sign in against this view stored
	// again in the salted form (rehash.go)
	rehashed sync.Map
}

func newView(snap *model.Snapshot, gen int64, dirs []Directory) *view {
	v := &view{
		gen:      gen,
		snap:     snap,
		resolver: authz.NewResolver(snap),
		local:    map[string]model.User{},
		realms:   map[string]bool{},
		repos:    map[string]model.Repository{},
		localOn:  slices.Contains(snap.Realms, RealmLocal),
		ldapOn:   slices.Contains(snap.Realms, RealmLDAP),
		dirs:     dirs,
	}
	for _, u := range snap.Users {
		v.local[u.ID] = u
	}
	v.spaced = map[string]string{}
	twice := map[string]bool{}
	for _, u := range snap.Users {
		if name := userName(u.ID); name != u.ID {
			_, seen := v.spaced[name]
			twice[name] = twice[name] || seen
			v.spaced[name] = u.ID
		}
	}
	for name := range v.spaced {
		if _, exact := v.local[name]; exact || twice[name] {
			delete(v.spaced, name)
		}
	}
	for _, r := range snap.Realms {
		v.realms[r] = true
	}
	for _, r := range snap.Repositories {
		v.repos[r.Name] = r
	}
	// the realms that check a user name and password, in the stored order
	// (task 6 of the phase 3 plan); LDAP only with a directory to ask
	for _, r := range snap.Realms {
		switch {
		case slices.Contains(v.checked, r):
		case r == RealmLocal, r == RealmUserToken, r == RealmLDAP && len(v.dirs) > 0:
			v.checked = append(v.checked, r)
		}
	}
	v.anonymous = v.anonymousPermissions()
	if slices.Contains(snap.Realms, RealmDefaultRole) {
		v.defaultRole = snap.DefaultRole
	}
	return v
}

// localUser is the local user a typed name, without its outer white space,
// signs in as: the user of that id, else the one user whose stored id is
// that name with outer white space (review R6).
func (v *view) localUser(name string) (model.User, bool) {
	if u, ok := v.local[name]; ok {
		return u, true
	}
	if id, ok := v.spaced[name]; ok {
		return v.local[id], true
	}
	return model.User{}, false
}

// report writes the warnings of a load: realms that sign nobody in, stored
// privileges that grant nothing.
func (v *view) report(log *slog.Logger) {
	if !v.localOn {
		log.Warn("realm not in the stored order: local users do not sign in", "realm", RealmLocal)
	}
	if !v.ldapOn && len(v.dirs) > 0 {
		log.Warn("realm not in the stored order: LDAP users do not sign in", "realm", RealmLDAP)
	}
	var unknown []string
	for _, r := range v.snap.Realms {
		if _, known := RealmByID(r); !known {
			unknown = append(unknown, r)
		}
	}
	if len(unknown) > 0 {
		log.Warn("realms this server does not build: nobody signs in through them", "realms", strings.Join(unknown, ","))
	}
	for kind, n := range v.resolver.Ignored() {
		log.Warn("stored privileges that grant nothing", "type", kind, "count", n)
	}
	switch role := v.snap.DefaultRole; {
	case !slices.Contains(v.snap.Realms, RealmDefaultRole):
	case role == "":
		log.Warn("the realm DefaultRole is active and names no role, so it grants nothing: realms set <realms> --default-role <role>")
	case !slices.Contains(v.resolver.Roles([]string{role}), role):
		log.Warn("the default role "+role+" does not exist, so it grants nothing", "realm", RealmDefaultRole)
	}
}

// anonymousPermissions expands the roles of the configured anonymous user:
// its local row, else a role mapping of the local source. A missing or
// inactive user holds nothing.
func (v *view) anonymousPermissions() []string {
	id := v.snap.Anonymous.UserID
	if u, ok := v.local[id]; ok {
		if u.Status != "active" {
			return []string{}
		}
		return v.resolver.Permissions(u.Roles)
	}
	return v.resolver.Permissions(v.mappedRoles(id, SourceLocal))
}

// mappedRoles are the roles of the role mappings of a source for a user id,
// matched without regard to letter case as the old product does.
func (v *view) mappedRoles(id, source string) []string {
	var roles []string
	for _, m := range v.snap.RoleMappings {
		if m.Source == source && strings.EqualFold(m.UserID, id) {
			roles = append(roles, m.Roles...)
		}
	}
	return roles
}

// directoryRoles are the roles of a directory user: each group that names a
// role, and the role mappings of the source LDAP.
func (v *view) directoryRoles(id string, groups []string) []string {
	var roles []string
	for _, g := range groups {
		if len(v.resolver.Roles([]string{g})) > 0 {
			roles = append(roles, g)
		}
	}
	roles = append(roles, v.mappedRoles(id, SourceLDAP)...)
	slices.Sort(roles)
	return slices.Compact(roles)
}

// subjectOf is the subject of a signed in user with its roles. The rights
// hold the default role as well; the roles of the user stay its own.
func (v *view) subjectOf(u model.User, roles []string, realm string) model.Subject {
	u.PasswordHash = ""
	u.Roles = slices.Clone(roles)
	granted := roles
	if v.defaultRole != "" {
		granted = append(slices.Clone(roles), v.defaultRole)
	}
	return model.Subject{User: u, Realm: realm, Permissions: v.resolver.Permissions(granted)}
}

// principal is who a check of credentials found, before the rights: the
// user without the hash, the realm that checked it and, for a directory
// user, the groups the directory gave at the sign in and until, the end of
// that answer: CredentialTime after the bind or the lookup (review R7). A
// remembered check and a Docker token of a directory user hold no longer
// than until without asking the directory again. A local user has no until;
// the view checks it at every use.
type principal struct {
	user   model.User
	realm  string
	groups []string
	until  time.Time
}

// signedIn is a successful check: the principal, its subject against the view
// of the check, and the realm that checked it, which is the realm of the
// user or the realm of user tokens.
type signedIn struct {
	principal principal
	subject   model.Subject
	by        string
}

func (g signedIn) clone() signedIn {
	g.principal.groups = slices.Clone(g.principal.groups)
	g.subject = copySubject(g.subject)
	return g
}

// subject checks a principal of an earlier view against this one: a local
// user that is still there and active, a directory user while a directory
// answers, each while its realm is in the stored order. The rights are those
// of this view.
func (v *view) subject(p principal) (model.Subject, bool) {
	switch p.realm {
	case RealmLocal:
		u, ok := v.local[p.user.ID]
		if !v.localOn || !ok || u.Status != "active" {
			return model.Subject{}, false
		}
		return v.subjectOf(u, u.Roles, RealmLocal), true
	case RealmLDAP:
		if !v.ldapOn || len(v.dirs) == 0 {
			return model.Subject{}, false
		}
		return v.subjectOf(p.user, v.directoryRoles(p.user.ID, p.groups), RealmLDAP), true
	}
	return model.Subject{}, false
}
