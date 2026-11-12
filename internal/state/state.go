// Package state is state.sqlite of the data directory, schema version 1: the
// users, roles, privileges, role mappings, LDAP servers, settings, blob store
// definitions and repositories an administrator set in the old product.
//
// The file has mode 0600 and its directory 0750. Writes go through one
// writer connection, each Write in one transaction; Load reads in one read
// transaction on its own pool. A file of a newer schema is refused.
package state

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/mirosso25/pipesmith-registry/internal/model"
	_ "modernc.org/sqlite" // the driver "sqlite"
)

const (
	// FileName is the file in the data directory.
	FileName = "state.sqlite"
	// SchemaVersion is the value of the schema row of the meta table.
	SchemaVersion = 1

	busyTimeoutMS = 30_000
	readerPool    = 4
)

// ErrAbsent is returned by Open when the data directory holds no state file.
var ErrAbsent = errors.New(FileName + " does not exist")

// Kind is one kind of record. Write replaces the rows of the kinds it is
// given and leaves the others.
type Kind string

// The record kinds and their tables.
const (
	KindUsers        Kind = "users"        // users, user_roles (local roles and the mappings of other sources)
	KindRoles        Kind = "roles"        // roles, role_privileges, role_roles
	KindPrivileges   Kind = "privileges"   // privileges
	KindLDAP         Kind = "ldap"         // ldap_servers
	KindRealms       Kind = "realms"       // settings row "realms"
	KindAnonymous    Kind = "anonymous"    // settings row "anonymous"
	KindBlobStores   Kind = "blob_stores"  // blob_stores
	KindRepositories Kind = "repositories" // repositories
)

// Kinds are all kinds in the order Write handles them.
var Kinds = []Kind{KindUsers, KindRoles, KindPrivileges, KindLDAP, KindRealms, KindAnonymous, KindBlobStores, KindRepositories}

const (
	settingRealms    = "realms"
	settingAnonymous = "anonymous"
)

var schema = []string{
	"CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY, first_name TEXT NOT NULL, last_name TEXT NOT NULL," +
		" email TEXT NOT NULL, status TEXT NOT NULL, source TEXT NOT NULL, password_hash TEXT NOT NULL) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS user_roles (user_id TEXT NOT NULL, source TEXT NOT NULL, position INTEGER NOT NULL," +
		" role_id TEXT NOT NULL, PRIMARY KEY (user_id, source, position)) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS roles (id TEXT PRIMARY KEY, source TEXT NOT NULL, name TEXT NOT NULL," +
		" description TEXT NOT NULL, read_only INTEGER NOT NULL) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS role_privileges (role_id TEXT NOT NULL REFERENCES roles (id) ON DELETE CASCADE," +
		" position INTEGER NOT NULL, privilege_id TEXT NOT NULL, PRIMARY KEY (role_id, position)) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS role_roles (role_id TEXT NOT NULL REFERENCES roles (id) ON DELETE CASCADE," +
		" position INTEGER NOT NULL, member_id TEXT NOT NULL, PRIMARY KEY (role_id, position)) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS privileges (name TEXT PRIMARY KEY, type TEXT NOT NULL, description TEXT NOT NULL," +
		" read_only INTEGER NOT NULL, properties TEXT NOT NULL) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS ldap_servers (id TEXT PRIMARY KEY, name TEXT NOT NULL, position INTEGER NOT NULL," +
		" record TEXT NOT NULL) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS blob_stores (name TEXT PRIMARY KEY, type TEXT NOT NULL, path TEXT NOT NULL," +
		" soft_quota TEXT) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS repositories (name TEXT PRIMARY KEY, format TEXT NOT NULL, type TEXT NOT NULL," +
		" online INTEGER NOT NULL, attributes TEXT NOT NULL) WITHOUT ROWID",
}

var bg = context.Background()

// Store is one open state file. It is safe for use by several goroutines.
type Store struct {
	path    string
	writer  *sql.DB
	readers *sql.DB
	writeMu sync.Mutex
}

// Path is the state file of a data directory.
func Path(dataDir string) string { return filepath.Join(dataDir, FileName) }

// Open opens the state file of dataDir. It never creates it: a missing file
// gives ErrAbsent.
func Open(dataDir string) (*Store, error) {
	path := Path(dataDir)
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrAbsent
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return open(path, false)
}

// Create opens the state file of dataDir and creates it with its directory
// when it does not exist yet. An existing file is opened as Open does.
func Create(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, err
	}
	path := Path(dataDir)
	created := false
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the state file of the data directory the operator configured.
	switch {
	case err == nil:
		created = true
		if err := f.Close(); err != nil {
			return nil, err
		}
	case errors.Is(err, fs.ErrExist):
	default:
		return nil, err
	}
	st, err := open(path, true)
	if err != nil && created {
		_ = os.Remove(path) // cleanup of the empty file this call made; the open error is returned
	}
	return st, err
}

// dsn is a file: URI, so that any character of the path reaches SQLite.
func dsn(path string, params url.Values) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return "file:" + (&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath() + "?" + params.Encode(), nil
}

func open(path string, setUp bool) (*Store, error) {
	busy := fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS)
	writerDSN, err := dsn(path, url.Values{"mode": {"rw"}, "_pragma": {busy, "foreign_keys(1)", "synchronous(NORMAL)"},
		"_txlock": {"immediate"}})
	if err != nil {
		return nil, err
	}
	readerDSN, err := dsn(path, url.Values{"_pragma": {busy, "query_only(1)"}})
	if err != nil {
		return nil, err
	}
	st := &Store{path: path}
	if st.writer, err = sql.Open("sqlite", writerDSN); err != nil {
		return nil, err
	}
	st.writer.SetMaxOpenConns(1)
	if st.readers, err = sql.Open("sqlite", readerDSN); err != nil {
		_ = st.writer.Close() // cleanup; the open error is returned
		return nil, err
	}
	st.readers.SetMaxOpenConns(readerPool)
	if err := st.check(setUp); err != nil {
		_ = st.Close() // cleanup; the check error is returned
		return nil, err
	}
	return st, nil
}

// check refuses a file of another schema; with setUp it writes schema 1
// into a file that has no meta table yet.
func (st *Store) check(setUp bool) error {
	var tables int
	if err := st.writer.QueryRowContext(bg, "SELECT count(*) FROM sqlite_master WHERE type = 'table'").Scan(&tables); err != nil {
		return fmt.Errorf("%s is not a SQLite file: %w", st.path, err)
	}
	var hasMeta int
	if err := st.writer.QueryRowContext(bg, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'meta'").
		Scan(&hasMeta); err != nil {
		return err
	}
	if hasMeta == 0 {
		if !setUp || tables > 0 {
			return fmt.Errorf("%s has no meta table, so it is no state file", st.path)
		}
		if _, err := st.writer.ExecContext(bg, "PRAGMA journal_mode = WAL"); err != nil {
			return err
		}
		return st.write(func(tx *sql.Tx) error {
			for _, statement := range schema {
				if _, err := tx.ExecContext(bg, statement); err != nil {
					return err
				}
			}
			_, err := tx.ExecContext(bg, "INSERT INTO meta VALUES ('schema', ?)", SchemaVersion)
			return err
		})
	}
	var version int64
	if err := st.writer.QueryRowContext(bg, "SELECT value FROM meta WHERE key = 'schema'").Scan(&version); err != nil {
		return fmt.Errorf("%s has no schema row: %w", st.path, err)
	}
	if version != SchemaVersion {
		return fmt.Errorf("%s has schema %d, this binary knows schema %d", st.path, version, SchemaVersion)
	}
	return nil
}

// Close closes both pools.
func (st *Store) Close() error {
	return errors.Join(st.readers.Close(), st.writer.Close())
}

func (st *Store) write(fn func(tx *sql.Tx) error) (err error) {
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	tx, err := st.writer.BeginTx(bg, nil)
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			_ = tx.Rollback() // only after a failed step, whose error is returned
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	done = true
	return tx.Commit()
}

// Write replaces the rows of the kinds named with the records of snap, in
// one transaction. Kinds not named keep their rows. The local roles of a
// user are User.Roles; RoleMappings hold the users of other sources, so a
// mapping of a user's own id and source is refused.
func (st *Store) Write(kinds []Kind, snap *model.Snapshot) error {
	if len(kinds) == 0 {
		return errors.New("no record kind named for the write")
	}
	for _, k := range kinds {
		if !slices.Contains(Kinds, k) {
			return fmt.Errorf("unknown record kind %q", k)
		}
	}
	if snap == nil {
		snap = &model.Snapshot{}
	}
	return st.write(func(tx *sql.Tx) error {
		for _, k := range Kinds {
			if !slices.Contains(kinds, k) {
				continue
			}
			if err := writers[k](tx, snap); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		}
		return nil
	})
}

var writers = map[Kind]func(*sql.Tx, *model.Snapshot) error{
	KindUsers:        writeUsers,
	KindRoles:        writeRoles,
	KindPrivileges:   writePrivileges,
	KindLDAP:         writeLDAP,
	KindRealms:       writeRealms,
	KindAnonymous:    writeAnonymous,
	KindBlobStores:   writeBlobStores,
	KindRepositories: writeRepositories,
}

func exec(tx *sql.Tx, query string, args ...any) error {
	_, err := tx.ExecContext(bg, query, args...)
	return err
}

func flag(b bool) int {
	if b {
		return 1
	}
	return 0
}

func encode(v any) (string, error) {
	data, err := json.Marshal(v)
	return string(data), err
}

func writeUsers(tx *sql.Tx, snap *model.Snapshot) error {
	if err := exec(tx, "DELETE FROM user_roles"); err != nil {
		return err
	}
	if err := exec(tx, "DELETE FROM users"); err != nil {
		return err
	}
	local := map[[2]string]bool{}
	for _, u := range snap.Users {
		if local[[2]string{u.ID, u.Source}] {
			return fmt.Errorf("user %q of source %q is given twice", u.ID, u.Source)
		}
		local[[2]string{u.ID, u.Source}] = true
		if err := exec(tx, "INSERT INTO users VALUES (?, ?, ?, ?, ?, ?, ?)", u.ID, u.FirstName, u.LastName, u.Email,
			u.Status, u.Source, u.PasswordHash); err != nil {
			return fmt.Errorf("user %q: %w", u.ID, err)
		}
		if err := userRoles(tx, u.ID, u.Source, u.Roles); err != nil {
			return err
		}
	}
	mapped := map[[2]string]bool{}
	for _, m := range snap.RoleMappings {
		key := [2]string{m.UserID, m.Source}
		if local[key] {
			return fmt.Errorf("the role mapping of %q, source %q, duplicates the roles of that local user", m.UserID, m.Source)
		}
		if mapped[key] {
			return fmt.Errorf("the role mapping of %q, source %q, is given twice", m.UserID, m.Source)
		}
		mapped[key] = true
		if err := userRoles(tx, m.UserID, m.Source, m.Roles); err != nil {
			return err
		}
	}
	return nil
}

func userRoles(tx *sql.Tx, user, source string, roles []string) error {
	for i, role := range roles {
		if err := exec(tx, "INSERT INTO user_roles VALUES (?, ?, ?, ?)", user, source, i, role); err != nil {
			return fmt.Errorf("roles of %q, source %q: %w", user, source, err)
		}
	}
	return nil
}

func writeRoles(tx *sql.Tx, snap *model.Snapshot) error {
	if err := exec(tx, "DELETE FROM roles"); err != nil {
		return err
	}
	for _, r := range snap.Roles {
		if err := exec(tx, "INSERT INTO roles VALUES (?, ?, ?, ?, ?)", r.ID, r.Source, r.Name, r.Description,
			flag(r.ReadOnly)); err != nil {
			return fmt.Errorf("role %q: %w", r.ID, err)
		}
		for i, p := range r.Privileges {
			if err := exec(tx, "INSERT INTO role_privileges VALUES (?, ?, ?)", r.ID, i, p); err != nil {
				return fmt.Errorf("role %q: %w", r.ID, err)
			}
		}
		for i, member := range r.Roles {
			if err := exec(tx, "INSERT INTO role_roles VALUES (?, ?, ?)", r.ID, i, member); err != nil {
				return fmt.Errorf("role %q: %w", r.ID, err)
			}
		}
	}
	return nil
}

func writePrivileges(tx *sql.Tx, snap *model.Snapshot) error {
	if err := exec(tx, "DELETE FROM privileges"); err != nil {
		return err
	}
	for _, p := range snap.Privileges {
		properties := p.Properties
		if properties == nil {
			properties = map[string]string{}
		}
		text, err := encode(properties)
		if err != nil {
			return err
		}
		if err := exec(tx, "INSERT INTO privileges VALUES (?, ?, ?, ?, ?)", p.Name, p.Type, p.Description,
			flag(p.ReadOnly), text); err != nil {
			return fmt.Errorf("privilege %q: %w", p.Name, err)
		}
	}
	return nil
}

func writeLDAP(tx *sql.Tx, snap *model.Snapshot) error {
	if err := exec(tx, "DELETE FROM ldap_servers"); err != nil {
		return err
	}
	for _, server := range snap.LDAP {
		text, err := encode(server)
		if err != nil {
			return err
		}
		if err := exec(tx, "INSERT INTO ldap_servers VALUES (?, ?, ?, ?)", server.ID, server.Name, server.Order,
			text); err != nil {
			return fmt.Errorf("LDAP server %q: %w", server.ID, err)
		}
	}
	return nil
}

func setting(tx *sql.Tx, key string, value any) error {
	text, err := encode(value)
	if err != nil {
		return err
	}
	return exec(tx, "INSERT OR REPLACE INTO settings VALUES (?, ?)", key, text)
}

func writeRealms(tx *sql.Tx, snap *model.Snapshot) error {
	realms := snap.Realms
	if realms == nil {
		realms = []string{}
	}
	return setting(tx, settingRealms, realms)
}

func writeAnonymous(tx *sql.Tx, snap *model.Snapshot) error {
	return setting(tx, settingAnonymous, snap.Anonymous)
}

func writeBlobStores(tx *sql.Tx, snap *model.Snapshot) error {
	if err := exec(tx, "DELETE FROM blob_stores"); err != nil {
		return err
	}
	for _, b := range snap.BlobStores {
		var quota any
		if b.SoftQuota != nil {
			text, err := encode(b.SoftQuota)
			if err != nil {
				return err
			}
			quota = text
		}
		if err := exec(tx, "INSERT INTO blob_stores VALUES (?, ?, ?, ?)", b.Name, b.Type, b.Path, quota); err != nil {
			return fmt.Errorf("blob store %q: %w", b.Name, err)
		}
	}
	return nil
}

func writeRepositories(tx *sql.Tx, snap *model.Snapshot) error {
	if err := exec(tx, "DELETE FROM repositories"); err != nil {
		return err
	}
	for _, r := range snap.Repositories {
		attributes := r.Attributes
		if attributes == nil {
			attributes = map[string]any{}
		}
		text, err := encode(attributes)
		if err != nil {
			return fmt.Errorf("attributes of repository %q: %w", r.Name, err)
		}
		if err := exec(tx, "INSERT INTO repositories VALUES (?, ?, ?, ?, ?)", r.Name, r.Format, r.Type,
			flag(r.Online), text); err != nil {
			return fmt.Errorf("repository %q: %w", r.Name, err)
		}
	}
	return nil
}

// Load reads every record in one read transaction. Records come in the
// order of their key, LDAP servers by their order and then their id. Lists
// and maps are never nil.
func (st *Store) Load() (*model.Snapshot, error) {
	tx, err := st.readers.BeginTx(bg, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }() // a read transaction has nothing to keep
	snap := &model.Snapshot{}
	for _, step := range []func(*sql.Tx, *model.Snapshot) error{loadUsers, loadRoles, loadPrivileges, loadLDAP,
		loadSettings, loadBlobStores, loadRepositories} {
		if err := step(tx, snap); err != nil {
			return nil, err
		}
	}
	return snap, nil
}

func each(tx *sql.Tx, query string, scan func(*sql.Rows) error) error {
	rows, err := tx.QueryContext(bg, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func decode(text string, v any) error {
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.UseNumber()
	return dec.Decode(v)
}

func loadUsers(tx *sql.Tx, snap *model.Snapshot) error {
	snap.Users = []model.User{}
	snap.RoleMappings = []model.RoleMapping{}
	local := map[[2]string]int{}
	err := each(tx, "SELECT id, first_name, last_name, email, status, source, password_hash FROM users ORDER BY id, source",
		func(rows *sql.Rows) error {
			var u model.User
			if err := rows.Scan(&u.ID, &u.FirstName, &u.LastName, &u.Email, &u.Status, &u.Source, &u.PasswordHash); err != nil {
				return err
			}
			u.Roles = []string{}
			local[[2]string{u.ID, u.Source}] = len(snap.Users)
			snap.Users = append(snap.Users, u)
			return nil
		})
	if err != nil {
		return err
	}
	mapped := map[[2]string]int{}
	return each(tx, "SELECT user_id, source, role_id FROM user_roles ORDER BY user_id, source, position",
		func(rows *sql.Rows) error {
			var user, source, role string
			if err := rows.Scan(&user, &source, &role); err != nil {
				return err
			}
			key := [2]string{user, source}
			if i, ok := local[key]; ok {
				snap.Users[i].Roles = append(snap.Users[i].Roles, role)
				return nil
			}
			i, ok := mapped[key]
			if !ok {
				i = len(snap.RoleMappings)
				mapped[key] = i
				snap.RoleMappings = append(snap.RoleMappings, model.RoleMapping{UserID: user, Source: source, Roles: []string{}})
			}
			snap.RoleMappings[i].Roles = append(snap.RoleMappings[i].Roles, role)
			return nil
		})
}

func loadRoles(tx *sql.Tx, snap *model.Snapshot) error {
	snap.Roles = []model.Role{}
	byID := map[string]int{}
	err := each(tx, "SELECT id, source, name, description, read_only FROM roles ORDER BY id", func(rows *sql.Rows) error {
		var r model.Role
		var readOnly int
		if err := rows.Scan(&r.ID, &r.Source, &r.Name, &r.Description, &readOnly); err != nil {
			return err
		}
		r.ReadOnly = readOnly != 0
		r.Privileges, r.Roles = []string{}, []string{}
		byID[r.ID] = len(snap.Roles)
		snap.Roles = append(snap.Roles, r)
		return nil
	})
	if err != nil {
		return err
	}
	err = each(tx, "SELECT role_id, privilege_id FROM role_privileges ORDER BY role_id, position", func(rows *sql.Rows) error {
		var role, privilege string
		if err := rows.Scan(&role, &privilege); err != nil {
			return err
		}
		snap.Roles[byID[role]].Privileges = append(snap.Roles[byID[role]].Privileges, privilege)
		return nil
	})
	if err != nil {
		return err
	}
	return each(tx, "SELECT role_id, member_id FROM role_roles ORDER BY role_id, position", func(rows *sql.Rows) error {
		var role, member string
		if err := rows.Scan(&role, &member); err != nil {
			return err
		}
		snap.Roles[byID[role]].Roles = append(snap.Roles[byID[role]].Roles, member)
		return nil
	})
}

func loadPrivileges(tx *sql.Tx, snap *model.Snapshot) error {
	snap.Privileges = []model.Privilege{}
	return each(tx, "SELECT name, type, description, read_only, properties FROM privileges ORDER BY name",
		func(rows *sql.Rows) error {
			var p model.Privilege
			var readOnly int
			var properties string
			if err := rows.Scan(&p.Name, &p.Type, &p.Description, &readOnly, &properties); err != nil {
				return err
			}
			p.ReadOnly = readOnly != 0
			p.Properties = map[string]string{}
			if err := decode(properties, &p.Properties); err != nil {
				return fmt.Errorf("properties of privilege %q: %w", p.Name, err)
			}
			snap.Privileges = append(snap.Privileges, p)
			return nil
		})
}

func loadLDAP(tx *sql.Tx, snap *model.Snapshot) error {
	snap.LDAP = []model.LDAPServer{}
	return each(tx, "SELECT id, record FROM ldap_servers ORDER BY position, id", func(rows *sql.Rows) error {
		var id, record string
		if err := rows.Scan(&id, &record); err != nil {
			return err
		}
		var server model.LDAPServer
		if err := decode(record, &server); err != nil {
			return fmt.Errorf("LDAP server %q: %w", id, err)
		}
		snap.LDAP = append(snap.LDAP, server)
		return nil
	})
}

func loadSettings(tx *sql.Tx, snap *model.Snapshot) error {
	snap.Realms = []string{}
	return each(tx, "SELECT key, value FROM settings ORDER BY key", func(rows *sql.Rows) error {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return err
		}
		switch key {
		case settingRealms:
			if err := decode(value, &snap.Realms); err != nil {
				return fmt.Errorf("setting %s: %w", key, err)
			}
			if snap.Realms == nil {
				snap.Realms = []string{}
			}
		case settingAnonymous:
			if err := decode(value, &snap.Anonymous); err != nil {
				return fmt.Errorf("setting %s: %w", key, err)
			}
		}
		return nil
	})
}

func loadBlobStores(tx *sql.Tx, snap *model.Snapshot) error {
	snap.BlobStores = []model.BlobStore{}
	return each(tx, "SELECT name, type, path, soft_quota FROM blob_stores ORDER BY name", func(rows *sql.Rows) error {
		var b model.BlobStore
		var quota sql.NullString
		if err := rows.Scan(&b.Name, &b.Type, &b.Path, &quota); err != nil {
			return err
		}
		if quota.Valid {
			b.SoftQuota = &model.SoftQuota{}
			if err := decode(quota.String, b.SoftQuota); err != nil {
				return fmt.Errorf("soft quota of blob store %q: %w", b.Name, err)
			}
		}
		snap.BlobStores = append(snap.BlobStores, b)
		return nil
	})
}

func loadRepositories(tx *sql.Tx, snap *model.Snapshot) error {
	snap.Repositories = []model.Repository{}
	return each(tx, "SELECT name, format, type, online, attributes FROM repositories ORDER BY name",
		func(rows *sql.Rows) error {
			var r model.Repository
			var online int
			var attributes string
			if err := rows.Scan(&r.Name, &r.Format, &r.Type, &online, &attributes); err != nil {
				return err
			}
			r.Online = online != 0
			r.Attributes = map[string]any{}
			if err := decode(attributes, &r.Attributes); err != nil {
				return fmt.Errorf("attributes of repository %q: %w", r.Name, err)
			}
			if r.Attributes == nil {
				r.Attributes = map[string]any{}
			}
			snap.Repositories = append(snap.Repositories, r)
			return nil
		})
}

// Counts are the rows per record kind, for reports.
func Counts(snap *model.Snapshot) map[string]int {
	return map[string]int{
		"users":         len(snap.Users),
		"role_mappings": len(snap.RoleMappings),
		"roles":         len(snap.Roles),
		"privileges":    len(snap.Privileges),
		"ldap_servers":  len(snap.LDAP),
		"realms":        len(snap.Realms),
		"blob_stores":   len(snap.BlobStores),
		"repositories":  len(snap.Repositories),
	}
}
