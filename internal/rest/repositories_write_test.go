package rest

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/proxy"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// repositoryStore is an identity store with one created hosted raw
// repository and its env for an administrator.
func repositoryStore(t *testing.T) (*identityStore, *Env, StoreWrite) {
	t.Helper()
	store := identityFixture()
	store.snap.Repositories = []model.Repository{{Name: "demo-h", Format: "raw", Type: "hosted", Online: true,
		Attributes: map[string]any{"storage": map[string]any{"blobStoreName": admin.WriteStore, "writePolicy": "ALLOW"}}}}
	env, iw := as(store, "admin", "nexus:*")
	env.Repositories = repos.New(nil, nil, nil).WithAttributes(store.snap.Repositories)
	return store, env, StoreWrite{Store: store, Subject: iw.Subject, DataDir: t.TempDir()}
}

// The delete removes the content while the record still stands, and the
// record last (ruling ST5): an erase that fails leaves the record.
func TestRepositoryDeleteRemovesTheContentFirst(t *testing.T) {
	store, env, w := repositoryStore(t)
	erased := 0
	w.Erase = func(name string) error {
		erased++
		if !admin.Exists(store.snap, admin.KindRepository, name) {
			t.Fatalf("the record of %s went before its content", name)
		}
		return &admin.Refusal{Code: admin.CodeUnavailable, Message: "the store is not open"}
	}
	a, err := DeleteRepository(env, w, "demo-h")
	if err != nil || a.Status != http.StatusServiceUnavailable || erased != 1 {
		t.Fatalf("%+v %v, %d erases", a, err, erased)
	}
	// the one write is the offline mark of ST17; the record stays
	if !admin.Exists(store.snap, admin.KindRepository, "demo-h") || store.writes != 1 {
		t.Fatalf("a failed erase removed the record: %d writes", store.writes)
	}
	w.Erase = func(name string) error {
		erased++
		if !admin.Exists(store.snap, admin.KindRepository, name) {
			t.Fatalf("the record of %s went before its content", name)
		}
		return nil
	}
	if a, err := DeleteRepository(env, w, "demo-h"); err != nil || a.Status != http.StatusNoContent {
		t.Fatalf("%+v %v", a, err)
	}
	if admin.Exists(store.snap, admin.KindRepository, "demo-h") || erased != 2 {
		t.Fatalf("the record stayed, %d erases", erased)
	}
}

// The upstream password of a created proxy lies in its secret file, never
// in the record or in an answer (ruling ST9, A11).
func TestRepositoryRecordHoldsNoSecret(t *testing.T) {
	store, env, w := repositoryStore(t)
	body := `{"name":"demo-p","online":true,"storage":{"blobStoreName":"local-write","strictContentTypeValidation":true},` +
		`"proxy":{"remoteUrl":"https://203.0.113.10/","contentMaxAge":1440,"metadataMaxAge":1440},` +
		`"httpClient":{"blocked":false,"autoBlock":true,"authentication":{"type":"username","username":"u","password":"example-up-pw"}}}`
	a, err := CreateRepository(env, w, "raw", "proxy", []byte(body))
	if err != nil || a.Status != http.StatusCreated {
		t.Fatalf("%+v %v", a, err)
	}
	if leaks := admin.Leaks(a.Body, ""); len(leaks) != 0 {
		t.Fatalf("the answer leaks %v", leaks)
	}
	record, _ := json.Marshal(store.snap.Repositories)
	if strings.Contains(string(record), "example-up-pw") || !strings.Contains(string(record), `"username":"u"`) {
		t.Fatalf("the record: %s", record)
	}
	secret, err := os.ReadFile(filepath.Join(w.DataDir, "secrets", "remote", "demo-p"))
	if err != nil || string(secret) != "example-up-pw" {
		t.Fatalf("the secret file: %q %v", secret, err)
	}
	env.Snapshot = store.snap
	env.Repositories = repos.New(nil, nil, nil).WithAttributes(store.snap.Repositories)
	got, err := RepositoryDetail(env, "raw", "proxy", "demo-p")
	if err != nil || got.Status != http.StatusOK || len(admin.Leaks(got.Body, "")) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
}

// Ruling ST12: a role that names the view privilege of a repository before
// it exists gains nothing through a create of a subject that lacks it; the
// role and the records are read back unchanged after the 403.
func TestACreateGivesARoleNoRight(t *testing.T) {
	store, _, w := repositoryStore(t)
	store.snap.Roles = append(store.snap.Roles, model.Role{ID: "demo-waiting", Source: "default", Name: "Waiting",
		Privileges: []string{"nx-repository-view-raw-demo-later-*"}})
	env, _ := as(store, "demo-repo-admin", "nexus:repository-admin:raw:demo-later:*", "nexus:repository-admin:raw:demo-later:browse",
		"nexus:repository-admin:raw:demo-later:read", "nexus:repository-admin:raw:demo-later:edit", "nexus:repository-admin:raw:demo-later:delete")
	env.Repositories = repos.New(nil, nil, nil).WithAttributes(store.snap.Repositories)
	before, _ := json.Marshal(store.snap)
	body := `{"name":"demo-later","online":true,"storage":{"blobStoreName":"local-write","writePolicy":"ALLOW"}}`
	a, err := CreateRepository(env, w, "raw", "hosted", []byte(body))
	if err != nil || a.Status != http.StatusForbidden || !strings.Contains(fmt.Sprint(a.Body), "nexus:repository-view:raw:demo-later:") {
		t.Fatalf("%+v %v", a, err)
	}
	if after, _ := json.Marshal(store.snap); string(after) != string(before) || store.writes != 0 {
		t.Fatalf("the refused create changed the state: %d writes", store.writes)
	}
}

// Ruling ST15: a create starts clean. A secret file that a repository of
// the same name left is removed by a create that writes none.
func TestACreateRemovesALeftSecretFile(t *testing.T) {
	_, env, w := repositoryStore(t)
	if _, err := proxy.WriteSecret(w.DataDir, "demo-p", "example-left-pw"); err != nil {
		t.Fatal(err)
	}
	body := `{"name":"demo-p","online":true,"storage":{"blobStoreName":"local-write"},` +
		`"proxy":{"remoteUrl":"https://203.0.113.10/"},"httpClient":{"authentication":{"type":"username","username":"u"}}}`
	if a, err := CreateRepository(env, w, "raw", "proxy", []byte(body)); err != nil || a.Status != http.StatusCreated {
		t.Fatalf("%+v %v", a, err)
	}
	if _, err := proxy.ReadSecret(w.DataDir, "demo-p"); !errors.Is(err, proxy.ErrNoSecret) {
		t.Fatalf("the left secret file stayed: %v", err)
	}
}

// racingStore is an identity store whose writes run one at a time, as the
// transactions of state.sqlite do, and that returns from a write only after
// a pause, so that a change made after the write meets the next write.
type racingStore struct {
	mu sync.Mutex
	*identityStore
	failCommit bool // the write runs fn and then fails, as a commit can
}

func (r *racingStore) Load() (*model.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.identityStore.Load()
}

func (r *racingStore) ResetCredentials() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.identityStore.ResetCredentials()
}

func (r *racingStore) Update(kinds []state.Kind, fn func(*model.Snapshot) error) error {
	r.mu.Lock()
	err := r.identityStore.Update(kinds, func(s *model.Snapshot) error {
		if err := fn(s); err != nil {
			return err
		}
		if r.failCommit {
			return errors.New("the commit failed")
		}
		return nil
	})
	r.mu.Unlock()
	time.Sleep(time.Duration(rand.IntN(3000)) * time.Microsecond)
	return err
}

func proxyBody(user, password string) string {
	return `{"name":"demo-p","online":true,"storage":{"blobStoreName":"local-write"},"proxy":{"remoteUrl":"https://203.0.113.10/"},` +
		`"httpClient":{"authentication":{"type":"username","username":"` + user + `","password":"` + password + `"}}}`
}

// Ruling ST16: the secret file changes in the write of the record. Saves
// that race leave the file with the password of the user name the record
// holds.
func TestTheSecretFileGoesWithTheRecord(t *testing.T) {
	base, env, w := repositoryStore(t)
	store := &racingStore{identityStore: base}
	w.Store = store
	if a, err := CreateRepository(env, w, "raw", "proxy", []byte(proxyBody("u-0", "p-0"))); err != nil || a.Status != http.StatusCreated {
		t.Fatalf("%+v %v", a, err)
	}
	env.Repositories = repos.New(nil, nil, nil).WithAttributes(base.snap.Repositories)
	var wg sync.WaitGroup
	for i := 1; i <= 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a, err := SaveRepository(env, w, "raw", "proxy", "demo-p", []byte(proxyBody(fmt.Sprintf("u-%d", i), fmt.Sprintf("p-%d", i)))); err != nil ||
				a.Status != http.StatusNoContent {
				t.Errorf("save %d: %+v %v", i, a, err)
			}
		}()
	}
	wg.Wait()
	rec, _ := json.Marshal(base.snap.Repositories)
	secret, err := proxy.ReadSecret(w.DataDir, "demo-p")
	if err != nil {
		t.Fatal(err)
	}
	user := "u-" + strings.TrimPrefix(secret, "p-")
	if !strings.Contains(string(rec), `"username":"`+user+`"`) {
		t.Fatalf("the secret file holds %s, the record %s", secret, rec)
	}
}

// Ruling ST16: a write of the state that fails leaves the secret file as
// it was, for a save and for a create.
func TestAFailedStateWriteKeepsTheSecretFile(t *testing.T) {
	base, env, w := repositoryStore(t)
	if a, err := CreateRepository(env, w, "raw", "proxy", []byte(proxyBody("u-old", "p-old"))); err != nil || a.Status != http.StatusCreated {
		t.Fatalf("%+v %v", a, err)
	}
	env.Repositories = repos.New(nil, nil, nil).WithAttributes(base.snap.Repositories)
	w.Store = &racingStore{identityStore: base, failCommit: true}
	if _, err := SaveRepository(env, w, "raw", "proxy", "demo-p", []byte(proxyBody("u-new", "p-new"))); err == nil {
		t.Fatal("a save whose state write failed answered")
	}
	if secret, err := proxy.ReadSecret(w.DataDir, "demo-p"); err != nil || secret != "p-old" {
		t.Fatalf("after the failed save the secret file holds %q, %v", secret, err)
	}
	body := strings.Replace(proxyBody("u-x", "p-x"), "demo-p", "demo-q", 1)
	if _, err := CreateRepository(env, w, "raw", "proxy", []byte(body)); err == nil {
		t.Fatal("a create whose state write failed answered")
	}
	if _, err := proxy.ReadSecret(w.DataDir, "demo-q"); !errors.Is(err, proxy.ErrNoSecret) {
		t.Fatalf("a failed create left a secret file: %v", err)
	}
}

// Ruling ST17: a delete sets the repository offline before its content
// goes, so that no write lands meanwhile; a repository with rows in a store
// of the old product is kept with 409, since a scan of that store would
// bring the rows back.
func TestADeleteGoesOfflineFirstAndKeepsOldRows(t *testing.T) {
	store, env, w := repositoryStore(t)
	w.OldRows = func(string) ([]string, error) { return []string{"store-a"}, nil }
	w.Erase = func(string) error {
		t.Fatal("erase of a repository with rows in a store of the old product")
		return nil
	}
	a, err := DeleteRepository(env, w, "demo-h")
	if err != nil || a.Status != http.StatusConflict || !strings.Contains(fmt.Sprint(a.Body), "store-a") || store.writes != 0 {
		t.Fatalf("%+v %v, %d writes", a, err, store.writes)
	}
	w.OldRows = func(string) ([]string, error) { return nil, nil }
	w.Erase = func(name string) error {
		i := slices.IndexFunc(store.snap.Repositories, func(r model.Repository) bool { return r.Name == name })
		if i < 0 || store.snap.Repositories[i].Online {
			t.Fatalf("the content of %s goes while its record is missing or online", name)
		}
		return nil
	}
	if a, err := DeleteRepository(env, w, "demo-h"); err != nil || a.Status != http.StatusNoContent || store.writes != 2 {
		t.Fatalf("%+v %v, %d writes", a, err, store.writes)
	}
}

// Finding 11 with ruling ST11: the lookup of a remote host runs before the
// write of the state, so a slow resolver holds no write.
func TestASlowLookupHoldsNoStateWrite(t *testing.T) {
	base, env, w := repositoryStore(t)
	store := &racingStore{identityStore: base}
	w.Store = store
	env.Permits = func(p string) bool { return strings.HasPrefix(p, "nexus:repository-") }
	var held, called bool
	w.Lookup = func(string) ([]netip.Addr, error) {
		called = true
		held = !store.mu.TryLock()
		if !held {
			store.mu.Unlock()
		}
		time.Sleep(50 * time.Millisecond)
		return []netip.Addr{netip.MustParseAddr("203.0.113.10")}, nil
	}
	body := strings.Replace(proxyBody("u", "p"), "https://203.0.113.10/", "https://remote.example.com/", 1)
	if a, err := CreateRepository(env, w, "raw", "proxy", []byte(body)); err != nil || a.Status != http.StatusCreated {
		t.Fatalf("%+v %v", a, err)
	}
	if !called || held {
		t.Fatalf("the lookup ran %v, while the state write was held %v", called, held)
	}
}
