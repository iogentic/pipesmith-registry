package state_test

import (
	"crypto/sha256"
	"reflect"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The keys an import takes over from the old product (decisions K4, K8, K9
// and K10 of docs/plans/secrets-import.md). Every digest here is made from
// an invented word.

func importedKey(kind, user, source string) model.ImportedKey {
	return model.ImportedKey{Kind: kind, UserID: user, Source: source,
		Digest: sha256.Sum256([]byte("invented " + kind + " " + user + " " + source)), Created: created}
}

func TestImportedKeysAreWrittenWithTheImportAndFoundByKindAndDigest(t *testing.T) {
	_, st := create(t)
	keys := []model.ImportedKey{importedKey("npm", "admin", "default"), importedKey("docker", "ldap.person", "LDAP"),
		importedKey("nuget", "admin", "default")}
	before := generation(t, st)
	if err := st.WriteImport([]state.Kind{state.KindUsers}, sample(), keys); err != nil {
		t.Fatal(err)
	}
	if got := generation(t, st); got != before+1 {
		t.Fatalf("generation %d after the import, want %d", got, before+1)
	}
	for _, want := range keys {
		got, found, err := st.ImportedKey(want.Kind, want.Digest)
		if err != nil || !found {
			t.Fatalf("%v: found %v, %v", want, found, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	}
	if _, found, err := st.ImportedKey("docker", keys[0].Digest); err != nil || found {
		t.Fatalf("the npm key was found as a Docker key: %v %v", found, err)
	}
	counts, err := st.ImportedKeyCounts()
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"npm": 1, "docker": 1, "nuget": 1}; !reflect.DeepEqual(counts, want) {
		t.Fatalf("counts %v, want %v", counts, want)
	}
}

// An import with keys replaces all imported keys; one without keys (nil)
// keeps them.
func TestAnImportReplacesTheImportedKeysOnlyWhenItHoldsKeys(t *testing.T) {
	_, st := create(t)
	first, second := importedKey("npm", "admin", "default"), importedKey("npm", "ldap.person", "LDAP")
	if err := st.WriteImport([]state.Kind{state.KindUsers}, sample(), []model.ImportedKey{first}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteImport([]state.Kind{state.KindRoles}, sample(), nil); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := st.ImportedKey("npm", first.Digest); !found {
		t.Fatal("an import without keys removed the imported keys")
	}
	if err := st.WriteImport([]state.Kind{state.KindRoles}, sample(), []model.ImportedKey{second}); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := st.ImportedKey("npm", first.Digest); found {
		t.Fatal("an import with keys kept a key it does not hold")
	}
	if _, found, _ := st.ImportedKey("npm", second.Digest); !found {
		t.Fatal("the key of the second import is not there")
	}
}

// Two keys of one digest in one import are refused, and nothing is written.
func TestAnImportWithOneDigestTwiceWritesNothing(t *testing.T) {
	_, st := create(t)
	key := importedKey("npm", "admin", "default")
	if err := st.WriteImport([]state.Kind{state.KindUsers}, sample(), []model.ImportedKey{key, key}); err == nil {
		t.Fatal("two keys of one digest were written")
	}
	if got := load(t, st); len(got.Users) != 0 {
		t.Fatalf("the refused import wrote %d users", len(got.Users))
	}
}

// The key of a local id leaves with the id, as a token row does (review S4).
func TestTheImportedKeyOfALocalUserLeavesWithTheUser(t *testing.T) {
	_, st := create(t)
	local, ldap := importedKey("npm", "viewer", "default"), importedKey("npm", "viewer", "LDAP")
	if err := st.WriteImport([]state.Kind{state.KindUsers}, sample(), []model.ImportedKey{local, ldap}); err != nil {
		t.Fatal(err)
	}
	without := sample()
	without.Users = without.Users[:1]
	if err := st.Write([]state.Kind{state.KindUsers}, without); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := st.ImportedKey("npm", local.Digest); found {
		t.Fatal("the key of the removed local user is still there")
	}
	if _, found, _ := st.ImportedKey("npm", ldap.Digest); !found {
		t.Fatal("the key of the directory user of the same name went with the local user")
	}
}

func TestAnImportedKeyIsTouchedRemovedAndForgotten(t *testing.T) {
	_, st := create(t)
	a, b := importedKey("npm", "admin", "default"), importedKey("docker", "admin", "default")
	if err := st.WriteImport([]state.Kind{state.KindUsers}, sample(), []model.ImportedKey{a, b}); err != nil {
		t.Fatal(err)
	}
	gen := generation(t, st)
	used := created.Add(time.Hour)
	if err := st.TouchImportedKey(a.Digest, used); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.ImportedKey("npm", a.Digest); !got.LastUsed.Equal(used) {
		t.Fatalf("last use %v, want %v", got.LastUsed, used)
	}
	if generation(t, st) != gen {
		t.Fatal("the last use raised the generation")
	}
	removed, err := st.RemoveImportedKey(a.Digest)
	if err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if removed, err := st.RemoveImportedKey(a.Digest); err != nil || removed {
		t.Fatalf("a second remove: %v %v", removed, err)
	}
	if generation(t, st) != gen+1 {
		t.Fatal("the remove did not raise the generation once")
	}
	n, err := st.ForgetImportedKeys()
	if err != nil || n != 1 {
		t.Fatalf("forget: %d %v", n, err)
	}
	if counts, _ := st.ImportedKeyCounts(); len(counts) != 0 {
		t.Fatalf("counts after forget: %v", counts)
	}
}
