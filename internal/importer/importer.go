// Package importer reads an export of the old product's database, as
// tools/h2export writes it, or a repository settings snapshot, maps it to the
// records of internal/model and writes state.sqlite and repositories.json.
//
// An export is checked in full before anything is written: the manifest,
// the file list of its set, and per file the columns, the row count, the
// count of removed fields and the SHA-256. Each import replaces only the
// record kinds its source holds. No hash and no secret value is ever part of
// an error or a report.
package importer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/mirosso25/pipesmith-registry/internal/config"
	"github.com/mirosso25/pipesmith-registry/internal/model"
	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
	"github.com/mirosso25/pipesmith-registry/internal/repos"
	"github.com/mirosso25/pipesmith-registry/internal/state"
)

const (
	// Format and ManifestVersion are the values of manifest.json this
	// importer reads.
	Format          = "pipesmith-registry-h2-export"
	ManifestVersion = 1
	// ManifestName is the manifest file of an export directory.
	ManifestName = "manifest.json"

	maxManifestBytes = 1 << 20
	maxFileBytes     = 512 << 20
	sourceLocal      = "default"
)

// The sets of the export program and their tables with the columns, in the
// order the program writes them.
type table struct {
	name    string
	columns []string
	kind    state.Kind
}

var (
	identityTables = []table{
		{"SECURITY_USER", []string{"id", "first_name", "last_name", "email", "status", "password"}, state.KindUsers},
		{"ROLE", []string{"id", "name", "description", "privileges", "roles", "read_only"}, state.KindRoles},
		{"USER_ROLE_MAPPING", []string{"user_id", "source", "roles"}, state.KindUsers},
		{"PRIVILEGE", []string{"id", "name", "description", "type", "properties", "read_only"}, state.KindPrivileges},
		{"REALM_CONFIGURATION", []string{"id", "realm_names"}, state.KindRealms},
		{"ANONYMOUS_CONFIGURATION", []string{"id", "enabled", "user_id", "realm_name"}, state.KindAnonymous},
		{"LDAP_CONFIGURATION", []string{"id", "name", "order", "connection", "mapping"}, state.KindLDAP},
	}
	contentTables = []table{
		{"BLOB_STORE_CONFIGURATION", []string{"id", "name", "type", "attributes"}, state.KindBlobStores},
		{"REPOSITORY", []string{"id", "name", "recipe_name", "online", "routing_rule_id", "attributes"}, state.KindRepositories},
	}
	sets = map[string][]table{
		"identity": identityTables,
		"content":  contentTables,
		"all":      slices.Concat(identityTables, contentTables),
	}
)

func (t table) file() string { return strings.ToLower(t.name) + ".jsonl" }

// secretWords mark a key that is removed wherever it occurs, as the export
// program removes it.
var secretWords = []string{"password", "secret", "token", "key", "passphrase", "credential"}

func secretKey(key string) bool {
	lower := strings.ToLower(key)
	for _, word := range secretWords {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

// clean returns v without the secret keys of every object in it and adds
// their paths to removed.
func clean(v any, path string, removed *[]string) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for key, value := range x {
			child := path + "." + key
			if secretKey(key) {
				*removed = append(*removed, child)
				continue
			}
			out[key] = clean(value, child, removed)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			out[i] = clean(value, fmt.Sprintf("%s[%d]", path, i), removed)
		}
		return out
	}
	return v
}

// Export is what an export directory or a settings snapshot holds. Snapshot
// lists are empty, never nil, for the kinds the source does not hold.
type Export struct {
	Set              string // content, identity, all, or settings
	Kinds            []state.Kind
	Snapshot         *model.Snapshot
	PasswordHashes   int      // users with a stored hash
	NeedsCredentials []string // repositories whose credentials were removed
	UnmappedLDAPKeys []string // keys of the LDAP columns no field takes, by name only
}

// Result is an import that was written.
type Result struct {
	Export
	StatePath        string
	RepositoriesPath string // empty when the source holds no repositories
}

type manifestFile struct {
	Table   string   `json:"table"`
	File    string   `json:"file"`
	Columns []string `json:"columns"`
	Rows    int64    `json:"rows"`
	Removed int64    `json:"removed"`
	SHA256  string   `json:"sha256"`
}

type manifest struct {
	Format         string         `json:"format"`
	Version        int            `json:"version"`
	Set            string         `json:"set"`
	Driver         string         `json:"driver"`
	PasswordHashes bool           `json:"passwordHashes"`
	Files          []manifestFile `json:"files"`
}

type row map[string]any

// readRegular reads a regular file that is no symbolic link and holds at
// most limit bytes.
func readRegular(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	f, err := os.Open(path) // #nosec G304 -- a file of the export directory the operator names.
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return data, nil
}

func decodeJSON(data []byte, v any, strict bool) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("more than one JSON value")
	}
	return nil
}

func readManifest(dir string) (*manifest, []table, error) {
	path := filepath.Join(dir, ManifestName)
	data, err := readRegular(path, maxManifestBytes)
	if err != nil {
		return nil, nil, err
	}
	var m manifest
	if err := decodeJSON(data, &m, true); err != nil {
		return nil, nil, fmt.Errorf("%s is not a manifest of the export program: %w", path, err)
	}
	if m.Format != Format {
		return nil, nil, fmt.Errorf("%s has format %q, not %q", path, m.Format, Format)
	}
	if m.Version != ManifestVersion {
		return nil, nil, fmt.Errorf("%s has version %d, this binary reads version %d", path, m.Version, ManifestVersion)
	}
	tables, ok := sets[m.Set]
	if !ok {
		return nil, nil, fmt.Errorf("%s names the unknown set %q", path, m.Set)
	}
	if len(m.Files) != len(tables) {
		return nil, nil, fmt.Errorf("%s lists %d files; the set %s has %d", path, len(m.Files), m.Set, len(tables))
	}
	for i, t := range tables {
		f := m.Files[i]
		if f.Table != t.name || f.File != t.file() {
			return nil, nil, fmt.Errorf("%s: entry %d is %s (%s), expected %s (%s)", path, i, f.Table, f.File, t.name, t.file())
		}
		if !slices.Equal(f.Columns, t.columns) {
			return nil, nil, fmt.Errorf("%s: the columns of %s are %v, expected %v", path, f.File, f.Columns, t.columns)
		}
	}
	return &m, tables, nil
}

// readRows checks one file against its manifest entry and decodes its rows.
func readRows(dir string, entry manifestFile) ([]row, error) {
	path := filepath.Join(dir, entry.File)
	data, err := readRegular(path, maxFileBytes)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != entry.SHA256 {
		return nil, fmt.Errorf("%s does not have the SHA-256 of the manifest", path)
	}
	var lines [][]byte
	if len(data) > 0 {
		if data[len(data)-1] != '\n' {
			return nil, fmt.Errorf("%s does not end with a line break", path)
		}
		lines = bytes.Split(data[:len(data)-1], []byte("\n"))
	}
	if int64(len(lines)) != entry.Rows {
		return nil, fmt.Errorf("%s has %d rows, the manifest says %d", path, len(lines), entry.Rows)
	}
	want := append(slices.Clone(entry.Columns), "_removed")
	slices.Sort(want)
	rows := make([]row, 0, len(lines))
	var removed int64
	for i, line := range lines {
		var r row
		if err := decodeJSON(line, &r, false); err != nil || r == nil {
			return nil, fmt.Errorf("%s: row %d is not a JSON object", path, i+1)
		}
		keys := make([]string, 0, len(r))
		for k := range r {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, want) {
			return nil, fmt.Errorf("%s: row %d has the keys %v, expected %v", path, i+1, keys, want)
		}
		list, ok := r["_removed"].([]any)
		if !ok {
			return nil, fmt.Errorf("%s: row %d has no list _removed", path, i+1)
		}
		removed += int64(len(list))
		rows = append(rows, r)
	}
	if removed != entry.Removed {
		return nil, fmt.Errorf("%s lists %d removed fields, the manifest says %d", path, removed, entry.Removed)
	}
	return rows, nil
}

// field errors name the file, the row and the key, never the value.
type reader struct {
	file string
	row  int
	r    row
	err  error
}

func (rd *reader) fail(key, want string) {
	if rd.err == nil {
		rd.err = fmt.Errorf("%s: row %d: %s is not %s", rd.file, rd.row, key, want)
	}
}

// text is a string; null gives "".
func (rd *reader) text(key string) string {
	switch v := rd.r[key].(type) {
	case nil:
		return ""
	case string:
		return v
	}
	rd.fail(key, "a string")
	return ""
}

func (rd *reader) flag(key string) bool {
	if v, ok := rd.r[key].(bool); ok {
		return v
	}
	rd.fail(key, "true or false")
	return false
}

func (rd *reader) integer(key string) int64 {
	if n, ok := rd.r[key].(json.Number); ok {
		if v, err := n.Int64(); err == nil {
			return v
		}
	}
	rd.fail(key, "an integer")
	return 0
}

func (rd *reader) names(key string) []string {
	list, ok := rd.r[key].([]any)
	if !ok {
		rd.fail(key, "a list")
		return []string{}
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			rd.fail(key, "a list of names")
			return []string{}
		}
		out = append(out, s)
	}
	return out
}

// object is a JSON object; null, as the export writes a removed column,
// gives an empty one.
func (rd *reader) object(key string) map[string]any {
	switch v := rd.r[key].(type) {
	case nil:
		return map[string]any{}
	case map[string]any:
		return v
	}
	rd.fail(key, "an object")
	return map[string]any{}
}

func (rd *reader) removed() []string {
	list, _ := rd.r["_removed"].([]any)
	out := []string{}
	for _, v := range list {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func eachRow(file string, rows []row, fn func(rd *reader) error) error {
	for i, r := range rows {
		rd := &reader{file: file, row: i + 1, r: r}
		err := fn(rd)
		if rd.err != nil {
			return rd.err
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func emptySnapshot() *model.Snapshot {
	return &model.Snapshot{Users: []model.User{}, Roles: []model.Role{}, Privileges: []model.Privilege{},
		RoleMappings: []model.RoleMapping{}, LDAP: []model.LDAPServer{}, Realms: []string{},
		BlobStores: []model.BlobStore{}, Repositories: []model.Repository{}}
}

// ReadExport checks an export directory and maps its rows. It writes
// nothing.
func ReadExport(dir string) (*Export, error) {
	m, tables, err := readManifest(dir)
	if err != nil {
		return nil, err
	}
	data := map[string][]row{}
	for i, t := range tables {
		rows, err := readRows(dir, m.Files[i])
		if err != nil {
			return nil, err
		}
		data[t.name] = rows
	}
	if !m.PasswordHashes {
		err := eachRow("security_user.jsonl", data["SECURITY_USER"], func(rd *reader) error {
			if rd.text("password") != "" {
				return fmt.Errorf("security_user.jsonl: row %d holds a password hash, but the manifest says the export has none",
					rd.row)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	export := &Export{Set: m.Set, Snapshot: emptySnapshot(), NeedsCredentials: []string{}, UnmappedLDAPKeys: []string{}}
	for _, k := range state.Kinds {
		if slices.ContainsFunc(tables, func(t table) bool { return t.kind == k }) {
			export.Kinds = append(export.Kinds, k)
		}
	}
	steps := []struct {
		table string
		fn    func(*Export, map[string][]row) error
	}{
		{"SECURITY_USER", mapUsers}, {"ROLE", mapRoles}, {"PRIVILEGE", mapPrivileges},
		{"REALM_CONFIGURATION", mapRealms}, {"ANONYMOUS_CONFIGURATION", mapAnonymous}, {"LDAP_CONFIGURATION", mapLDAP},
		{"BLOB_STORE_CONFIGURATION", mapBlobStores}, {"REPOSITORY", mapRepositories},
	}
	for _, step := range steps {
		if _, ok := data[step.table]; ok {
			if err := step.fn(export, data); err != nil {
				return nil, err
			}
		}
	}
	return export, nil
}

func mapUsers(export *Export, data map[string][]row) error {
	snap := export.Snapshot
	index := map[string]int{}
	err := eachRow("security_user.jsonl", data["SECURITY_USER"], func(rd *reader) error {
		u := model.User{ID: rd.text("id"), FirstName: rd.text("first_name"), LastName: rd.text("last_name"),
			Email: rd.text("email"), Status: rd.text("status"), Source: sourceLocal, Roles: []string{},
			PasswordHash: rd.text("password")}
		if u.ID == "" {
			return fmt.Errorf("%s: row %d has no user id", rd.file, rd.row)
		}
		if _, dup := index[u.ID]; dup {
			return fmt.Errorf("%s: user %q is given twice", rd.file, u.ID)
		}
		if u.PasswordHash != "" {
			export.PasswordHashes++
		}
		index[u.ID] = len(snap.Users)
		snap.Users = append(snap.Users, u)
		return nil
	})
	if err != nil {
		return err
	}
	err = eachRow("user_role_mapping.jsonl", data["USER_ROLE_MAPPING"], func(rd *reader) error {
		user, source, roles := rd.text("user_id"), rd.text("source"), rd.names("roles")
		if i, ok := index[user]; ok && source == sourceLocal {
			snap.Users[i].Roles = roles
			return nil
		}
		snap.RoleMappings = append(snap.RoleMappings, model.RoleMapping{UserID: user, Source: source, Roles: roles})
		return nil
	})
	if err != nil {
		return err
	}
	slices.SortFunc(snap.Users, func(a, b model.User) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(snap.RoleMappings, func(a, b model.RoleMapping) int {
		return strings.Compare(a.UserID+"\x00"+a.Source, b.UserID+"\x00"+b.Source)
	})
	return nil
}

func mapRoles(export *Export, data map[string][]row) error {
	snap := export.Snapshot
	err := eachRow("role.jsonl", data["ROLE"], func(rd *reader) error {
		snap.Roles = append(snap.Roles, model.Role{ID: rd.text("id"), Source: sourceLocal, Name: rd.text("name"),
			Description: rd.text("description"), ReadOnly: rd.flag("read_only"), Privileges: rd.names("privileges"),
			Roles: rd.names("roles")})
		return nil
	})
	slices.SortFunc(snap.Roles, func(a, b model.Role) int { return strings.Compare(a.ID, b.ID) })
	return err
}

// mapPrivileges keeps the id as the name: roles refer to privileges by id.
func mapPrivileges(export *Export, data map[string][]row) error {
	snap := export.Snapshot
	err := eachRow("privilege.jsonl", data["PRIVILEGE"], func(rd *reader) error {
		properties := map[string]string{}
		for key, value := range rd.object("properties") {
			if s, ok := value.(string); ok {
				properties[key] = s
				continue
			}
			text, err := json.Marshal(value)
			if err != nil {
				return err
			}
			properties[key] = string(text)
		}
		snap.Privileges = append(snap.Privileges, model.Privilege{Name: rd.text("id"), Type: rd.text("type"),
			Description: rd.text("description"), ReadOnly: rd.flag("read_only"), Properties: properties})
		return nil
	})
	slices.SortFunc(snap.Privileges, func(a, b model.Privilege) int { return strings.Compare(a.Name, b.Name) })
	return err
}

func mapRealms(export *Export, data map[string][]row) error {
	rows := data["REALM_CONFIGURATION"]
	if len(rows) > 1 {
		return fmt.Errorf("realm_configuration.jsonl has %d rows, the old product keeps one", len(rows))
	}
	return eachRow("realm_configuration.jsonl", rows, func(rd *reader) error {
		export.Snapshot.Realms = rd.names("realm_names")
		return nil
	})
}

func mapAnonymous(export *Export, data map[string][]row) error {
	rows := data["ANONYMOUS_CONFIGURATION"]
	if len(rows) > 1 {
		return fmt.Errorf("anonymous_configuration.jsonl has %d rows, the old product keeps one", len(rows))
	}
	return eachRow("anonymous_configuration.jsonl", rows, func(rd *reader) error {
		export.Snapshot.Anonymous = model.Anonymous{Enabled: rd.flag("enabled"), UserID: rd.text("user_id"),
			RealmName: rd.text("realm_name")}
		return nil
	})
}

// ldapKeys are the JSON names of model.LDAPServer that the two JSON columns
// may fill; id, name and order come from their own columns.
var ldapKeys = func() map[string]bool {
	keys := map[string]bool{}
	t := reflect.TypeFor[model.LDAPServer]()
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		keys[name] = true
	}
	delete(keys, "id")
	delete(keys, "name")
	delete(keys, "order")
	return keys
}()

func mapLDAP(export *Export, data map[string][]row) error {
	snap := export.Snapshot
	unmapped := map[string]bool{}
	err := eachRow("ldap_configuration.jsonl", data["LDAP_CONFIGURATION"], func(rd *reader) error {
		id := rd.text("id")
		merged := map[string]any{}
		for _, column := range []string{"connection", "mapping"} {
			var removed []string
			fields, _ := clean(rd.object(column), column, &removed).(map[string]any)
			for key, value := range fields {
				if ldapKeys[key] {
					merged[key] = value
				} else {
					unmapped[column+"."+key] = true
				}
			}
		}
		text, err := json.Marshal(merged)
		if err != nil {
			return err
		}
		var server model.LDAPServer
		if err := json.Unmarshal(text, &server); err != nil {
			var typeErr *json.UnmarshalTypeError
			if errors.As(err, &typeErr) {
				return fmt.Errorf("%s: LDAP server %q: %s is not of type %s", rd.file, id, typeErr.Field, typeErr.Type)
			}
			return fmt.Errorf("%s: LDAP server %q cannot be read", rd.file, id)
		}
		server.ID, server.Name, server.Order = id, rd.text("name"), int(rd.integer("order"))
		server.Protocol = strings.ToLower(server.Protocol)
		server.GroupType = strings.ToLower(server.GroupType)
		snap.LDAP = append(snap.LDAP, server)
		return nil
	})
	slices.SortFunc(snap.LDAP, func(a, b model.LDAPServer) int {
		if a.Order != b.Order {
			return a.Order - b.Order
		}
		return strings.Compare(a.ID, b.ID)
	})
	for key := range unmapped {
		export.UnmappedLDAPKeys = append(export.UnmappedLDAPKeys, key)
	}
	slices.Sort(export.UnmappedLDAPKeys)
	return err
}

func section(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

func mapBlobStores(export *Export, data map[string][]row) error {
	snap := export.Snapshot
	err := eachRow("blob_store_configuration.jsonl", data["BLOB_STORE_CONFIGURATION"], func(rd *reader) error {
		var removed []string
		attributes, _ := clean(rd.object("attributes"), "attributes", &removed).(map[string]any)
		b := model.BlobStore{Name: rd.text("name"), Type: rd.text("type")}
		if path, ok := section(attributes, "file")["path"].(string); ok {
			b.Path = path
		}
		if quota, ok := attributes["blobStoreQuotaConfig"].(map[string]any); ok {
			kind, okType := quota["quotaType"].(string)
			number, okLimit := quota["quotaLimitBytes"].(json.Number)
			limit, err := number.Int64()
			if !okType || !okLimit || err != nil {
				return fmt.Errorf("%s: the soft quota of blob store %q has no type or no integer limit", rd.file, b.Name)
			}
			b.SoftQuota = &model.SoftQuota{Type: kind, Limit: limit}
		}
		snap.BlobStores = append(snap.BlobStores, b)
		return nil
	})
	slices.SortFunc(snap.BlobStores, func(a, b model.BlobStore) int { return strings.Compare(a.Name, b.Name) })
	return err
}

func mapRepositories(export *Export, data map[string][]row) error {
	snap := export.Snapshot
	err := eachRow("repository.jsonl", data["REPOSITORY"], func(rd *reader) error {
		name, recipe := rd.text("name"), rd.text("recipe_name")
		cut := strings.LastIndex(recipe, "-")
		if cut <= 0 || cut == len(recipe)-1 {
			return fmt.Errorf("%s: the recipe of repository %q is not <format>-<type>", rd.file, name)
		}
		removed := rd.removed()
		attributes, _ := clean(rd.object("attributes"), "attributes", &removed).(map[string]any)
		if len(removed) > 0 {
			export.NeedsCredentials = append(export.NeedsCredentials, name)
		}
		snap.Repositories = append(snap.Repositories, model.Repository{Name: name, Format: recipe[:cut],
			Type: recipe[cut+1:], Online: rd.flag("online"), Attributes: attributes})
		return nil
	})
	slices.SortFunc(snap.Repositories, func(a, b model.Repository) int { return strings.Compare(a.Name, b.Name) })
	slices.Sort(export.NeedsCredentials)
	return err
}

// write puts the kinds of the export into state.sqlite of cfg.
func write(export *Export, cfg *config.Config) (*Result, error) {
	st, err := state.Create(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	if err := st.Write(export.Kinds, export.Snapshot); err != nil {
		return nil, err
	}
	return &Result{Export: *export, StatePath: state.Path(cfg.DataDir)}, nil
}

// Import checks the export directory dir, then writes its record kinds into
// state.sqlite of cfg, and repositories.json when the export holds
// repositories. A refused export writes nothing.
func Import(dir string, cfg *config.Config) (*Result, error) {
	export, err := ReadExport(dir)
	if err != nil {
		return nil, err
	}
	result, err := write(export, cfg)
	if err != nil {
		return nil, err
	}
	if slices.Contains(export.Kinds, state.KindRepositories) {
		if err := repos.WriteDefinitions(cfg.RepositoriesPath(), Definitions(export.Snapshot.Repositories)); err != nil {
			return nil, fmt.Errorf("state.sqlite holds the import, but %s was not written: %w", cfg.RepositoriesPath(), err)
		}
		result.RepositoriesPath = cfg.RepositoriesPath()
	}
	return result, nil
}

func nonEmpty(v any) any {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return nil
}

// Definitions are the entries of repositories.json for the repositories,
// with the fields that import-settings takes from a settings snapshot.
func Definitions(list []model.Repository) []*pyjson.Object {
	out := make([]*pyjson.Object, 0, len(list))
	for _, r := range list {
		members := []any{}
		if names, ok := section(r.Attributes, "group")["memberNames"].([]any); ok {
			for _, m := range names {
				if s, ok := m.(string); ok {
					members = append(members, s)
				}
			}
		}
		var port any
		if n, ok := section(r.Attributes, "docker")["httpPort"].(json.Number); ok {
			if p, err := n.Int64(); err == nil && p >= 1 && p <= 65535 {
				port = int(p)
			}
		}
		out = append(out, pyjson.NewObject().
			Set("name", r.Name).
			Set("format", r.Format).
			Set("type", r.Type).
			Set("members", members).
			Set("version_policy", nonEmpty(section(r.Attributes, "maven")["versionPolicy"])).
			Set("layout_policy", nonEmpty(section(r.Attributes, "maven")["layoutPolicy"])).
			Set("write_policy", nonEmpty(section(r.Attributes, "storage")["writePolicy"])).
			Set("remote_url", nonEmpty(section(r.Attributes, "proxy")["remoteUrl"])).
			Set("http_port", port))
	}
	return out
}

// ReadSettings maps a repository settings snapshot, the file import-settings
// reads: a list of repositories, or an object with that list under "items".
// The first entry of a name wins; entries without a name are left out. The
// attributes are the entry without name, format, type, online and url, and
// without the secret keys. It writes nothing.
func ReadSettings(path string) (*Export, error) {
	data, err := readRegular(path, maxFileBytes)
	if err != nil {
		return nil, err
	}
	var value any
	if err := decodeJSON(data, &value, false); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON", path)
	}
	if obj, ok := value.(map[string]any); ok {
		if items, ok := obj["items"].([]any); ok {
			value = items
		}
	}
	list, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s does not hold a list of repositories", path)
	}
	export := &Export{Set: "settings", Kinds: []state.Kind{state.KindRepositories}, Snapshot: emptySnapshot(),
		NeedsCredentials: []string{}, UnmappedLDAPKeys: []string{}}
	seen := map[string]bool{}
	for _, entry := range list {
		item, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		name, _ := item["name"].(string)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		r := model.Repository{Name: name, Format: "unknown", Type: "hosted", Online: true, Attributes: map[string]any{}}
		if s, ok := item["format"].(string); ok && s != "" {
			r.Format = s
		}
		if s, ok := item["type"].(string); ok && s != "" {
			r.Type = s
		}
		if b, ok := item["online"].(bool); ok {
			r.Online = b
		}
		var removed []string
		for key, v := range item {
			switch key {
			case "name", "format", "type", "online", "url":
				continue
			}
			if secretKey(key) {
				removed = append(removed, key)
				continue
			}
			r.Attributes[key] = clean(v, key, &removed)
		}
		if len(removed) > 0 {
			export.NeedsCredentials = append(export.NeedsCredentials, name)
		}
		export.Snapshot.Repositories = append(export.Snapshot.Repositories, r)
	}
	slices.SortFunc(export.Snapshot.Repositories, func(a, b model.Repository) int { return strings.Compare(a.Name, b.Name) })
	slices.Sort(export.NeedsCredentials)
	return export, nil
}

// ImportSettings writes the repositories of a settings snapshot into
// state.sqlite of cfg, and repositories.json as import-settings writes it.
func ImportSettings(path string, cfg *config.Config) (*Result, error) {
	export, err := ReadSettings(path)
	if err != nil {
		return nil, err
	}
	result, err := write(export, cfg)
	if err != nil {
		return nil, err
	}
	count, err := repos.ImportRepositorySettings(path, cfg.RepositoriesPath())
	if err != nil {
		return nil, fmt.Errorf("state.sqlite holds the repositories, but %s was not written: %w", cfg.RepositoriesPath(), err)
	}
	if count != len(export.Snapshot.Repositories) {
		return nil, fmt.Errorf("state.sqlite holds %d repositories, %s %d", len(export.Snapshot.Repositories),
			cfg.RepositoriesPath(), count)
	}
	result.RepositoriesPath = cfg.RepositoriesPath()
	return result, nil
}
