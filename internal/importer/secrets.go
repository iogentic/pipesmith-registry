package importer

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/proxy"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The files of an export with --with-secrets (decisions K1 to K9 of
// docs/plans/secrets-import.md). They follow the files of the set, in this
// order. No error and no report holds a value of them.

var secretTables = []table{
	{"API_KEY_V2", []string{"username", "domain", "realm", "sha256", "created"}, ""},
	{"REMOTE_SECRETS", []string{"repository", "field", "secret"}, ""},
	{"LDAP_SECRETS", []string{"id", "secret"}, ""},
}

// keyKinds are the kinds of the domains of the old product.
var keyKinds = map[string]string{"NpmToken": "npm", "DockerToken": "docker", "NuGetApiKey": "nuget"}

// The realm values of api_key_v2.jsonl: found in the principals of the key,
// or unknown (K3).
const (
	realmLocal   = "NexusAuthenticatingRealm"
	realmLDAP    = "LdapRealm"
	realmUnknown = "unknown"
	sourceLDAP   = "LDAP"
)

// SecretsLeft are the counts of what the export program did not take over:
// rows of the secret store under a key of their own (K2), values that did
// not decrypt, keys of a domain this server does not know, and rows of the
// secret store that no key, repository or LDAP connection names.
type SecretsLeft struct {
	Keyed          int `json:"keyed"`
	Unreadable     int `json:"unreadable"`
	UnknownDomains int `json:"unknownDomains"`
	Unreferenced   int `json:"unreferenced"`
}

// secret is a value that is never printed.
type secret struct{ v string }

func (secret) String() string   { return "[secret]" }
func (secret) GoString() string { return "[secret]" }

type importedKey struct {
	user, kind, realm string
	sum               [32]byte
	created           time.Time
}

// secretSet is what the three files hold, checked.
type secretSet struct {
	keys   []importedKey
	remote map[string]secret // repository: password or bearer token
	ldap   map[string]secret // LDAP connection id: bind password
}

// checkValue refuses a secret that a secret file cannot hold.
func checkValue(file string, row int, v string) error {
	switch {
	case v == "":
		return fmt.Errorf("%s: row %d has an empty secret", file, row)
	case len(v) > state.MaxSecretBytes:
		return fmt.Errorf("%s: row %d has a secret of more than %d bytes", file, row, state.MaxSecretBytes)
	case strings.ContainsFunc(v, unicode.IsControl):
		return fmt.Errorf("%s: row %d has a secret with a control character", file, row)
	}
	return nil
}

func mapSecrets(export *Export, data map[string][]row) error {
	set := &secretSet{remote: map[string]secret{}, ldap: map[string]secret{}}
	seen := map[[32]byte]bool{}
	err := eachRow("api_key_v2.jsonl", data["API_KEY_V2"], func(rd *reader) error {
		k := importedKey{user: rd.text("username"), realm: rd.text("realm")}
		domain := rd.text("domain")
		var ok bool
		if k.kind, ok = keyKinds[domain]; !ok {
			return fmt.Errorf("%s: row %d has the unknown domain %q", rd.file, rd.row, domain)
		}
		if k.user == "" {
			return fmt.Errorf("%s: row %d has no user", rd.file, rd.row)
		}
		if k.realm != realmLocal && k.realm != realmLDAP && k.realm != realmUnknown {
			return fmt.Errorf("%s: row %d has the unknown realm %q", rd.file, rd.row, k.realm)
		}
		raw, err := hex.DecodeString(rd.text("sha256"))
		if err != nil || len(raw) != len(k.sum) {
			return fmt.Errorf("%s: row %d: sha256 is not 64 hex digits", rd.file, rd.row)
		}
		copy(k.sum[:], raw)
		if seen[k.sum] {
			return fmt.Errorf("%s: row %d holds a key of an earlier row", rd.file, rd.row)
		}
		seen[k.sum] = true
		if k.created, err = time.Parse(time.RFC3339Nano, rd.text("created")); err != nil {
			return fmt.Errorf("%s: row %d: created is not a time with an offset", rd.file, rd.row)
		}
		set.keys = append(set.keys, k)
		return nil
	})
	if err != nil {
		return err
	}
	err = eachRow("remote_secrets.jsonl", data["REMOTE_SECRETS"], func(rd *reader) error {
		repo, field, value := rd.text("repository"), rd.text("field"), rd.text("secret")
		if repo == "" || repo == "." || repo == ".." || strings.ContainsAny(repo, "/\\\x00") {
			return fmt.Errorf("%s: row %d: %q names no secret file of a repository", rd.file, rd.row, repo)
		}
		if field != "password" && field != "bearerToken" {
			return fmt.Errorf("%s: row %d: the field %q is neither password nor bearerToken", rd.file, rd.row, field)
		}
		if _, dup := set.remote[repo]; dup {
			return fmt.Errorf("%s: row %d: repository %q has a secret already", rd.file, rd.row, repo)
		}
		if err := checkValue(rd.file, rd.row, value); err != nil {
			return err
		}
		set.remote[repo] = secret{value}
		return nil
	})
	if err != nil {
		return err
	}
	err = eachRow("ldap_secrets.jsonl", data["LDAP_SECRETS"], func(rd *reader) error {
		id, value := rd.text("id"), rd.text("secret")
		if id == "" {
			return fmt.Errorf("%s: row %d has no connection id", rd.file, rd.row)
		}
		if _, dup := set.ldap[id]; dup {
			return fmt.Errorf("%s: row %d: connection %q has a secret already", rd.file, rd.row, id)
		}
		if err := checkValue(rd.file, rd.row, value); err != nil {
			return err
		}
		set.ldap[id] = secret{value}
		return nil
	})
	if err != nil {
		return err
	}
	export.secrets = set
	export.NeedsCredentials = slices.DeleteFunc(export.NeedsCredentials, func(name string) bool {
		_, ok := set.remote[name]
		return ok
	})
	return nil
}

// sourceOf is the source of the user of a key: the realm of its principals,
// else a local user of that name, else the directory (K3).
func (k importedKey) sourceOf(local map[string]bool) string {
	switch {
	case k.realm == realmLocal:
		return sourceLocal
	case k.realm == realmLDAP:
		return sourceLDAP
	case local[k.user]:
		return sourceLocal
	}
	return sourceLDAP
}

// writeFiles writes the secret files of the export: the secret of the
// remote of a proxy through proxy.WriteSecret (decision 5.13 of phase 5),
// the bind password of an LDAP connection as `ldap password` does. A file
// that exists is kept and named in kept: a value set by hand may be newer
// than the one the old product stored. It runs before state.sqlite is
// opened, so a file that fails ends the import before state.sqlite takes it
// (review secrets-1).
func (set *secretSet) writeFiles(dataDir string) (remote, ldap int, kept []string, err error) {
	kept = []string{}
	if err := os.MkdirAll(dataDir, 0o750); err != nil { // the mode state.Create gives it
		return 0, 0, nil, err
	}
	for _, repo := range sortedKeys(set.remote) {
		_, err := proxy.ReadSecret(dataDir, repo)
		switch {
		case err == nil:
			kept = append(kept, filepath.ToSlash(filepath.Join(state.SecretsDir, "remote", repo)))
			continue
		case !errors.Is(err, proxy.ErrNoSecret):
			return 0, 0, nil, fmt.Errorf("the secret file of the remote of %s cannot be read, nothing imported: %w", repo, err)
		}
		if _, err := proxy.WriteSecret(dataDir, repo, set.remote[repo].v); err != nil {
			return 0, 0, nil, fmt.Errorf("the secret of the remote of %s was not written, nothing imported: %w", repo, err)
		}
		remote++
	}
	for _, id := range sortedKeys(set.ldap) {
		path := state.LDAPSecretPath(dataDir, id)
		_, err := state.ReadSecret(path)
		switch {
		case err == nil:
			rel, _ := filepath.Rel(dataDir, path)
			kept = append(kept, filepath.ToSlash(rel))
			continue
		case !errors.Is(err, state.ErrNoSecret):
			return 0, 0, nil, fmt.Errorf("the bind password file of LDAP connection %s cannot be read, nothing imported: %w", id, err)
		}
		if err := state.WriteSecret(path, []byte(set.ldap[id].v)); err != nil {
			return 0, 0, nil, fmt.Errorf("the bind password of LDAP connection %s was not written, nothing imported: %w", id, err)
		}
		ldap++
	}
	return remote, ldap, kept, nil
}

// importedKeyRows are the imported keys of the export under the install key,
// each with the source its user has among the users given (the users of the
// state after the import).
func importedKeyRows(set *secretSet, installKey []byte, users []model.User) []model.ImportedKey {
	local := map[string]bool{}
	for _, u := range users {
		if u.Source == sourceLocal {
			local[u.ID] = true
		}
	}
	rows := make([]model.ImportedKey, 0, len(set.keys))
	for _, k := range set.keys {
		rows = append(rows, model.ImportedKey{Kind: k.kind, UserID: k.user, Source: k.sourceOf(local),
			Digest: model.ImportedDigest(installKey, k.sum), Created: k.created.UTC()})
	}
	return rows
}

func sortedKeys(m map[string]secret) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
