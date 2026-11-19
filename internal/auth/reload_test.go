package auth_test

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The security snapshot follows state.sqlite without a restart: the service
// reads the generation at most once per auth.ReloadInterval and loads the
// snapshot again when another write raised it (Base of the execution plan,
// leg S2). A reload empties the credential cache and ends what the change
// made invalid.

// stateOf writes snap into a new state.sqlite and returns the open store.
func stateOf(t testing.TB, snap *model.Snapshot) *state.Store {
	t.Helper()
	st, err := state.Create(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Write(state.Kinds, snap); err != nil {
		t.Fatal(err)
	}
	return st
}

// countingStore counts the reads of the generation and can fail them.
type countingStore struct {
	*state.Store
	reads atomic.Int64
	fail  atomic.Bool
}

func (c *countingStore) Generation() (int64, error) {
	c.reads.Add(1)
	if c.fail.Load() {
		return 0, errors.New("demo: the disk is gone")
	}
	return c.Store.Generation()
}

// newStoreEnv is newEnv over a state.sqlite with the records of snap.
func newStoreEnv(t testing.TB, snap *model.Snapshot, dirs ...auth.Directory) (*env, *countingStore) {
	t.Helper()
	store := &countingStore{Store: stateOf(t, snap)}
	e := storeEnv(t, store, dirs...)
	store.reads.Store(0) // the read of storeEnv before New
	return e, store
}

func storeEnv(t testing.TB, store auth.Store, dirs ...auth.Directory) *env {
	t.Helper()
	gen, err := store.Generation()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	c := newClock()
	logs := &bytes.Buffer{}
	svc, err := auth.New(auth.Options{Snapshot: loaded, Generation: gen, Store: store, Directories: dirs, Now: c.Now,
		Logger: slog.New(slog.NewTextHandler(&lockedWriter{w: logs}, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, svc: svc, handler: svc.SessionHandler(), clock: c, logs: logs}
}

// rewrite loads the records of the store, lets change edit them and writes
// every kind back, as another process does.
func rewrite(t testing.TB, st auth.Store, change func(*model.Snapshot)) {
	t.Helper()
	w, ok := st.(interface {
		Write([]state.Kind, *model.Snapshot) error
	})
	if !ok {
		t.Fatalf("%T cannot write", st)
	}
	snap, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	change(snap)
	if err := w.Write(state.Kinds, snap); err != nil {
		t.Fatal(err)
	}
}

func userOf(snap *model.Snapshot, id string) *model.User {
	for i := range snap.Users {
		if snap.Users[i].ID == id {
			return &snap.Users[i]
		}
	}
	return nil
}

func subjectOf(e *env, r *http.Request) (*model.Subject, error) { return e.svc.Subject(r) }

func TestReloadAtMostOncePerInterval(t *testing.T) {
	e, store := newStoreEnv(t, fixture(t))
	reader := password(t, "demo-reader")
	for i := range 100 {
		if s, err := subjectOf(e, basic(request(http.MethodGet, "/", nil), "demo-reader", reader)); err != nil || s == nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}
	if n := store.reads.Load(); n != 0 {
		t.Fatalf("%d reads of the generation within the first interval, want 0", n)
	}
	e.clock.Add(auth.ReloadInterval)
	for range 100 {
		_, _ = subjectOf(e, request(http.MethodGet, "/", nil))
		e.svc.Permits(nil, "nexus:settings:read")
	}
	if n := store.reads.Load(); n != 1 {
		t.Fatalf("%d reads of the generation after one interval, want 1", n)
	}
	// a change is not seen before the next interval, and then it is
	rewrite(t, store, func(s *model.Snapshot) { s.Anonymous.Enabled = true })
	if e.svc.AnonymousEnabled() {
		t.Fatal("the change was seen within the interval")
	}
	e.clock.Add(auth.ReloadInterval - time.Millisecond)
	_, _ = subjectOf(e, request(http.MethodGet, "/", nil))
	if e.svc.AnonymousEnabled() || store.reads.Load() != 1 {
		t.Fatalf("before the interval ended: anonymous %v, %d reads", e.svc.AnonymousEnabled(), store.reads.Load())
	}
	e.clock.Add(time.Millisecond)
	_, _ = subjectOf(e, request(http.MethodGet, "/", nil))
	if !e.svc.AnonymousEnabled() || store.reads.Load() != 2 {
		t.Fatalf("after the interval: anonymous %v, %d reads", e.svc.AnonymousEnabled(), store.reads.Load())
	}
	if !strings.Contains(e.logs.String(), "identity state loaded again") {
		t.Errorf("no log line for the reload: %q", e.logs)
	}
}

func TestReloadEndsWhatTheChangeMadeInvalid(t *testing.T) {
	d := ldapDirectory()
	e, store := newStoreEnv(t, fixture(t), d)
	readerCookie, _ := e.mustSignIn("demo-reader", password(t, "demo-reader"))
	adminCookie, adminBefore := e.mustSignIn("demo-admin", password(t, "demo-admin"))
	ldapCookie, _ := e.mustSignIn("demo-ldap-person", ldapPassword)
	if !slices.Contains(adminBefore.Permissions, "nexus:*") {
		t.Fatalf("admin before: %q", adminBefore.Permissions)
	}
	basicReader := func() (*model.Subject, error) {
		return subjectOf(e, basic(request(http.MethodGet, "/", nil), "demo-reader", password(t, "demo-reader")))
	}
	if s, err := basicReader(); err != nil || s == nil {
		t.Fatalf("Basic of the reader: %v", err)
	}
	if auth.CredentialCount(e.svc) != 1 {
		t.Fatalf("%d remembered credentials, want 1", auth.CredentialCount(e.svc))
	}

	rewrite(t, store, func(s *model.Snapshot) {
		userOf(s, "demo-reader").Status = "disabled"
		userOf(s, "demo-admin").Roles = []string{"demo-ops"}
		// the role the directory group named is gone
		s.Roles = slices.DeleteFunc(s.Roles, func(r model.Role) bool { return r.ID == "demo-ldap-group" })
	})
	// within the interval the old state still answers
	if s, err := subjectOf(e, request(http.MethodGet, "/", readerCookie)); err != nil || s == nil {
		t.Fatalf("reader session within the interval: %v", err)
	}
	e.clock.Add(auth.ReloadInterval)

	if s, err := subjectOf(e, request(http.MethodGet, "/", readerCookie)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Errorf("session of the disabled reader: %+v %v", s, err)
	}
	if s, err := basicReader(); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Errorf("Basic of the disabled reader: %+v %v", s, err)
	}
	if n := auth.CredentialCount(e.svc); n != 0 {
		t.Errorf("%d remembered credentials after the reload, want 0", n)
	}
	admin, err := subjectOf(e, request(http.MethodGet, "/", adminCookie))
	if err != nil || admin == nil || !slices.Equal(admin.Permissions, []string{"nexus:settings:read"}) ||
		e.svc.Permits(admin, "nexus:users:read") || !slices.Equal(admin.User.Roles, []string{"demo-ops"}) {
		t.Errorf("admin after losing nx-admin: %+v %v", admin, err)
	}
	rec := e.do(request(http.MethodGet, "/api/session", adminCookie))
	if a := decodeSession(t, rec); a.User == nil || !slices.Equal(a.Permissions, []string{"nexus:settings:read"}) {
		t.Errorf("session answer of the admin after the change: %+v", a)
	}
	person, err := subjectOf(e, request(http.MethodGet, "/", ldapCookie))
	if err != nil || person == nil || !slices.Equal(person.Permissions, []string{"nexus:settings:read"}) {
		t.Errorf("directory session after its group role went away: %+v %v", person, err)
	}
	if n := auth.SessionCount(e.svc); n != 2 {
		t.Errorf("%d sessions after the reload, want 2", n)
	}

	// the realm of the directory leaves the order: its session ends
	rewrite(t, store, func(s *model.Snapshot) {
		s.Realms = slices.DeleteFunc(s.Realms, func(r string) bool { return r == auth.RealmLDAP })
	})
	e.clock.Add(auth.ReloadInterval)
	if s, err := subjectOf(e, request(http.MethodGet, "/", ldapCookie)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Errorf("directory session after its realm left the order: %+v %v", s, err)
	}
	if s, err := subjectOf(e, request(http.MethodGet, "/", adminCookie)); err != nil || s == nil {
		t.Errorf("the local session stays: %v", err)
	}
}

// A reload that cannot read the store keeps the state it has and says so.
func TestReloadKeepsTheStateWhenTheStoreFails(t *testing.T) {
	e, store := newStoreEnv(t, fixture(t))
	cookie, _ := e.mustSignIn("demo-admin", password(t, "demo-admin"))
	store.fail.Store(true)
	rewrite(t, store, func(s *model.Snapshot) { userOf(s, "demo-admin").Status = "disabled" })
	e.clock.Add(auth.ReloadInterval)
	if s, err := subjectOf(e, request(http.MethodGet, "/", cookie)); err != nil || s == nil {
		t.Fatalf("session while the store fails: %v", err)
	}
	if !strings.Contains(e.logs.String(), "the identity state was not loaded again") || strings.Contains(e.logs.String(), "$shiro1$") {
		t.Fatalf("log %q", e.logs)
	}
	store.fail.Store(false)
	e.clock.Add(auth.ReloadInterval)
	if s, err := subjectOf(e, request(http.MethodGet, "/", cookie)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("session after the store answers again: %+v %v", s, err)
	}
}

// Every change of the generation hands the LDAP rows to DirectoriesFor and
// signs in through what it gives, also when the rows are the same: the bind
// password of a connection can change outside state.sqlite (its secret file),
// so the builder decides which connection to build again (internal/cli,
// ldapBuilds). Without a change of the generation it is not asked.
func TestReloadAsksForTheDirectoriesAtEachChange(t *testing.T) {
	snap := fixture(t)
	snap.LDAP = []model.LDAPServer{{ID: "demo-ldap-a", Name: "demo-a", Order: 1, Host: "ldap.example.com", Port: 389}}
	st := &countingStore{Store: stateOf(t, snap)}
	gen, _ := st.Generation()
	loaded, _ := st.Load()
	first, second, third := ldapDirectory(), ldapDirectory(), ldapDirectory()
	var mu sync.Mutex
	var built [][]model.LDAPServer
	c := newClock()
	svc, err := auth.New(auth.Options{Snapshot: loaded, Generation: gen, Store: st, Directories: []auth.Directory{first}, Now: c.Now,
		DirectoriesFor: func(servers []model.LDAPServer) []auth.Directory {
			mu.Lock()
			defer mu.Unlock()
			built = append(built, servers)
			if len(built) == 1 {
				return []auth.Directory{second}
			}
			return []auth.Directory{third}
		}})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, svc: svc, handler: svc.SessionHandler(), clock: c, logs: nil}
	e.mustSignIn("demo-ldap-person", ldapPassword)
	c.Add(auth.ReloadInterval)
	e.mustSignIn("demo-ldap-person", ldapPassword)
	if len(built) != 0 || first.count() != 2 {
		t.Fatalf("no change: %d builds, %d calls of the first directory", len(built), first.count())
	}
	rewrite(t, st, func(s *model.Snapshot) { s.Anonymous.Enabled = true })
	c.Add(auth.ReloadInterval)
	e.mustSignIn("demo-ldap-person", ldapPassword)
	if len(built) != 1 || built[0][0].Host != "ldap.example.com" || first.count() != 2 || second.count() != 1 {
		t.Fatalf("unchanged rows: %d builds, calls %d and %d", len(built), first.count(), second.count())
	}
	rewrite(t, st, func(s *model.Snapshot) { s.LDAP[0].Host = "ldap2.example.com" })
	c.Add(auth.ReloadInterval)
	e.mustSignIn("demo-ldap-person", ldapPassword)
	if len(built) != 2 || built[1][0].Host != "ldap2.example.com" || second.count() != 1 || third.count() != 1 {
		t.Fatalf("changed rows: %d builds, calls %d and %d", len(built), second.count(), third.count())
	}
}

// Sign ins, Basic requests and reloads run side by side under -race.
func TestReloadConcurrent(t *testing.T) {
	e, store := newStoreEnv(t, fixture(t), ldapDirectory())
	snap, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			snap.Anonymous.Enabled = i%2 == 0
			if err := store.Write([]state.Kind{state.KindAnonymous}, snap); err != nil {
				t.Errorf("write %d: %v", i, err)
				return
			}
			e.clock.Add(auth.ReloadInterval)
		}
	}()
	// 12 sign ins per user stay below the 20 sessions a user may hold
	users := []string{"demo-admin", "demo-reader", "demo-legacy-md5"}
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			user := users[i%len(users)]
			for range 3 {
				rec := e.signIn(user, password(t, user), "192.0.2.10")
				if rec.Code != http.StatusOK {
					t.Errorf("%s: sign in %d", user, rec.Code)
					return
				}
				cookie := sessionCookie(t, rec)
				if s, err := subjectOf(e, request(http.MethodGet, "/", cookie)); err != nil || s == nil || s.User.ID != user {
					t.Errorf("%s: %+v %v", user, s, err)
				}
				if s, err := subjectOf(e, basic(request(http.MethodGet, "/", nil), user, password(t, user))); err != nil || s == nil {
					t.Errorf("%s Basic: %v", user, err)
				}
				e.svc.Permits(nil, "nexus:repository-view:raw:demo-raw:read")
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	if store.reads.Load() == 0 {
		t.Fatal("no reload ran beside the requests")
	}
}

// Review N3: the reload interval runs on the monotonic clock, so a step of
// the wall clock backwards does not pause the reload.
func TestReloadIntervalOnTheMonotonicClock(t *testing.T) {
	e, store := newStoreEnv(t, fixture(t))
	var mono atomic.Int64
	auth.SetIntervalClock(e.svc, func() time.Duration { return time.Duration(mono.Load()) })
	rewrite(t, store, func(s *model.Snapshot) { s.Anonymous.Enabled = true })
	e.clock.Add(-time.Hour) // the wall clock steps back by an hour
	mono.Store(int64(auth.ReloadInterval))
	_, _ = subjectOf(e, request(http.MethodGet, "/", nil))
	if !e.svc.AnonymousEnabled() || store.reads.Load() != 1 {
		t.Fatalf("after the interval on the monotonic clock: anonymous %v, %d reads", e.svc.AnonymousEnabled(), store.reads.Load())
	}
}
