package admin

import (
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/proxy"
)

// The rules of the repository changes of leg 6-stores: the twelve kinds of
// decision A5, the remote address of a proxy (ruling ST8), the store a
// repository writes into (ST2), the Docker port (ST7), the stored upstream
// secret (ST9, R9 of the 0.2.0 plan), the delete of a group member (ST5)
// and the grant of the generated privileges of a new repository (ST10, I9).

// WriteStore is the one store this server writes new blobs into (ST2).
const WriteStore = "local-write"

const repositoryArea = "repositories"

// RepositoryFormats and RepositoryTypes are the twelve kinds of A5.
var (
	RepositoryFormats = []string{"maven2", "npm", "docker", "raw"}
	RepositoryTypes   = []string{"hosted", "proxy", "group"}
)

// segments name a format in the REST paths where the two differ.
var segments = map[string]string{"maven2": "maven"}

// FormatOfSegment is the format a REST path segment names: maven names
// maven2, a format whose segment differs (maven2) names none, any other
// segment its own format; the create, the save and the read take the same
// segments.
func FormatOfSegment(segment string) string {
	for format, s := range segments {
		if s == segment {
			return format
		}
	}
	if _, differs := segments[segment]; differs {
		return ""
	}
	return segment
}

// SegmentOf is the REST path segment of a format.
func SegmentOf(format string) string {
	if s, ok := segments[format]; ok {
		return s
	}
	return format
}

// CheckRepositoryKind refuses a format or a type outside A5 with 501 and
// the area repositories. Format and type are compared as they are.
func CheckRepositoryKind(format, kind string) error {
	if slices.Contains(RepositoryFormats, format) && slices.Contains(RepositoryTypes, kind) {
		return nil
	}
	return &Refusal{Code: CodeRepositoryKind, Area: repositoryArea, Message: fmt.Sprintf(
		"a repository of the format %s and the type %s is not available in this server, which has hosted, proxy and group of maven2, npm, docker and raw",
		format, kind)}
}

const remoteField = "proxy.remoteUrl"

// CheckRemote checks the remote address of a repository of the type kind
// (ST8): a proxy needs one of http or https with a host and no user part,
// 400 at proxy.remoteUrl; another type needs none. The address is never
// part of the message.
func CheckRemote(kind string, remote *string) error {
	if kind != "proxy" {
		return nil
	}
	fail := func(problem string) error {
		return &Refusal{Code: CodeRemoteInvalid, Field: remoteField, Message: "the remote address " + problem}
	}
	if remote == nil || strings.TrimSpace(*remote) == "" {
		return fail("is missing; a proxy needs the remote address of http or https it fetches from")
	}
	u, err := url.Parse(*remote)
	switch {
	case err != nil:
		return fail("cannot be read as an address")
	case u.Scheme != "http" && u.Scheme != "https":
		return fail(fmt.Sprintf("has the scheme %q; use http or https", u.Scheme))
	case u.User != nil:
		return fail("has a user part; give the user in httpClient.authentication")
	case u.Hostname() == "":
		return fail("has no host")
	case strings.ContainsAny(*remote, "#\x00\r\n"):
		return fail("has a fragment or a control character")
	}
	return nil
}

// Lookup resolves a host name to its addresses.
type Lookup func(host string) ([]netip.Addr, error)

// CheckRemoteHost refuses, for a subject without nexus:*, a remote address
// whose host is an address of a closed network, a name with any such
// address, or a name that does not resolve, with 400 at proxy.remoteUrl
// (ruling ST11, which replaces the 403 of ST8). lookup gives the addresses
// the caller resolved before the state write; nil resolves nothing. The
// dial guard of internal/proxy is the second layer, at every dial.
func CheckRemoteHost(remote string, lookup Lookup, holds func(permission string) bool) error {
	if holds(fullPermission) {
		return nil
	}
	u, err := url.Parse(remote)
	if err != nil {
		return nil // CheckRemote refuses it first
	}
	host := u.Hostname()
	fail := func(problem string) error {
		return &Refusal{Code: CodeRemoteInvalid, Field: remoteField, Message: fmt.Sprintf(
			"the remote host %s %s; only a holder of the full permission set may name such a host", host, problem)}
	}
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else if lookup != nil {
		addrs, _ = lookup(host)
	}
	switch {
	case len(addrs) == 0:
		return fail("does not resolve to an address")
	case slices.ContainsFunc(addrs, ClosedAddress):
		return fail("resolves to an address of a loopback, link local, private or multicast network")
	}
	return nil
}

// textAt is the string at the dot path of a decoded object, and whether a
// string is there.
func textAt(m map[string]any, path string) (string, bool) {
	v, _ := valueAt(m, path)
	s, ok := v.(string)
	return s, ok
}

// CheckStorage checks the store of a repository create or save (ST2,
// SR4, SR5, SR7). stores are the names of every store, writable the write
// stores among them: local-write and the created stores (WritableStore).
// A hosted repository names a write store, a proxy local-write, a group any
// store. Names compare exactly; one that differs from a store only in
// letter case is unknown, with the name of the store (ST14). The store of a
// stored hosted repository is fixed: a save names the stored one (R6, SR7);
// a group keeps the rule of its create.
// write_store is a server owned attribute (CheckServerAttributes); stored
// is nil for a create.
func CheckStorage(kind string, body, stored map[string]any, stores, writable []string) error {
	var list Refusals
	name, _ := textAt(body, "storage.blobStoreName")
	held, _ := textAt(stored, "storage.blobStoreName")
	field := "storage.blobStoreName"
	switch {
	case stored != nil && name == held && name != "":
	case stored != nil && held != "" && kind == "hosted":
		list = append(list, &Refusal{Code: CodeStoreFixed, Field: field, Message: fmt.Sprintf(
			"the blob store of a repository is fixed when it is created; this one keeps %s", held)})
	case name == "":
		list = append(list, &Refusal{Code: CodeStoreUnknown, Field: field,
			Message: "a repository needs storage.blobStoreName, the name of a blob store"})
	case !slices.Contains(stores, name):
		message := fmt.Sprintf("the blob store %s does not exist", name)
		if i := slices.IndexFunc(stores, func(s string) bool { return strings.EqualFold(s, name) }); i >= 0 {
			message = fmt.Sprintf("the blob store %s does not exist; names keep their letter case, the store is %s", name, stores[i])
		}
		list = append(list, &Refusal{Code: CodeStoreUnknown, Field: field, Message: message})
	case kind == "proxy" && name != WriteStore:
		list = append(list, &Refusal{Code: CodeWriteStore, Field: field, Message: fmt.Sprintf(
			"a proxy repository of this server caches into the store %s, not into %s; name %s", WriteStore, name, WriteStore)})
	case kind == "hosted" && !slices.Contains(writable, name):
		list = append(list, &Refusal{Code: CodeStoreReadOnly, Field: field, Message: fmt.Sprintf(
			"the blob store %s was imported and is read only; a hosted repository names a store this server writes", name)})
	}
	return list.err()
}

// NewStore is storage.newBlobStore of a create of a hosted repository
// (SR6): the blob store of storage.blobStoreName is created in the same
// change, with this path and soft quota. Path nil is <data>/stores/<name>.
type NewStore struct {
	Path      *string
	SoftQuota *model.SoftQuota
}

// NewStoreOf reads storage.newBlobStore of a body (SR6): nil when it is
// absent or null. It is offered on a create of a hosted repository only
// (400 not-offered), as an object with an optional path and soft quota; the
// name is storage.blobStoreName (400 body-invalid for anything else).
func NewStoreOf(kind string, body map[string]any, create bool) (*NewStore, error) {
	const field = "storage.newBlobStore"
	v, _ := valueAt(body, field)
	if v == nil {
		return nil, nil
	}
	switch {
	case !create:
		return nil, &Refusal{Code: CodeNotOffered, Field: field, Message: "a new blob store is made on a create only; send storage.newBlobStore as null"}
	case kind != "hosted":
		return nil, &Refusal{Code: CodeNotOffered, Field: field, Message: "a new blob store is made with a hosted repository only; send storage.newBlobStore as null"}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, &Refusal{Code: CodeBodyInvalid, Field: field, Message: "storage.newBlobStore is no object"}
	}
	var out NewStore
	var list Refusals
	for _, key := range slices.Sorted(maps.Keys(m)) {
		item := m[key]
		switch key {
		case "path":
			if item == nil {
				continue
			}
			p, ok := item.(string)
			if !ok {
				list = append(list, &Refusal{Code: CodeBodyInvalid, Field: field + ".path", Message: "the path of a new blob store is no text"})
				continue
			}
			out.Path = &p
		case "softQuota":
			if item == nil {
				continue
			}
			q, ok := quotaOf(item)
			if !ok {
				list = append(list, &Refusal{Code: CodeBodyInvalid, Field: field + ".softQuota", Message: "the soft quota of a new blob store has no type and limit"})
				continue
			}
			out.SoftQuota = q
		default:
			list = append(list, &Refusal{Code: CodeBodyInvalid, Field: field + "." + key, Message: fmt.Sprintf(
				"storage.newBlobStore takes path and softQuota; the name of the store is storage.blobStoreName, not %s", key)})
		}
	}
	if err := list.err(); err != nil {
		return nil, err
	}
	return &out, nil
}

// quotaOf reads a soft quota of a decoded body: type text and limit a number.
func quotaOf(v any) (*model.SoftQuota, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	kind, ok := m["type"].(string)
	if !ok {
		return nil, false
	}
	limit, err := strconv.ParseInt(textOf(m["limit"]), 10, 64)
	if err != nil {
		return nil, false
	}
	return &model.SoftQuota{Type: kind, Limit: limit}, true
}

// WritableStore reports whether a stored blob store is a created store this
// server writes (SR1): its path lies strictly below the folder stores of the
// data directory, both resolved, config.json attaches no store of its name,
// and it is not equal to, inside or around the folder of local-write or the
// path of another record of others, compared without regard to letter case
// (review R7, the rule of StorePath). Every other record is an imported
// store and stays read only.
func WritableStore(dataDir string, b model.BlobStore, attached []string, others []model.BlobStore) bool {
	if b.Name == "" || b.Name == WriteStore || b.Path == "" || slices.Contains(attached, b.Name) {
		return false
	}
	root, err := filepath.EvalSymlinks(filepath.Join(dataDir, StoresDir))
	if err != nil {
		return false
	}
	p, err := resolveLoose(b.Path)
	if err != nil || !within(p, root) {
		return false
	}
	overlaps := func(other string) bool {
		o, err := resolveLoose(other)
		if err != nil {
			return true // a path that cannot be resolved may be anywhere
		}
		lowP, lowO := strings.ToLower(p), strings.ToLower(o)
		return lowP == lowO || within(lowP, lowO) || within(lowO, lowP)
	}
	if overlaps(filepath.Join(root, WriteStore)) {
		return false
	}
	for _, o := range others {
		if o.Name != b.Name && o.Path != "" && overlaps(o.Path) {
			return false
		}
	}
	return true
}

// CheckDockerPort refuses a non null docker.httpPort or docker.httpsPort of
// a create or a save with 400 at the field (ST7): a port is set on the host
// with repos port and applies at the next start. A value the stored record
// holds is never refused (R6).
func CheckDockerPort(body, stored map[string]any) error {
	var list Refusals
	for _, field := range []string{"docker.httpPort", "docker.httpsPort"} {
		v, _ := valueAt(body, field)
		if v == nil {
			continue
		}
		if held, ok := valueAt(stored, field); ok && fmt.Sprint(held) == fmt.Sprint(v) {
			continue
		}
		list = append(list, &Refusal{Code: CodeDockerPort, Field: field, Message: fmt.Sprintf(
			"the port of a Docker repository is set on the host with repos port and applies at the next start; send %s as null", field)})
	}
	return list.err()
}

// SecretAction is what a change does with the stored upstream secret.
type SecretAction string

// What a change does with the stored upstream secret.
const (
	SecretKeep   SecretAction = "keep"
	SecretWrite  SecretAction = "write"
	SecretRemove SecretAction = "remove"
)

// UpstreamSecret is the change of the secret file of a proxy: keep it,
// write Value into it, or remove it.
type UpstreamSecret struct {
	Action SecretAction
	Value  string
}

// maxUpstreamSecret bounds an upstream secret, as internal/proxy does.
const maxUpstreamSecret = 64 << 10

const authField = "httpClient.authentication"

// UpstreamSecretOf reads the change of the upstream secret from the body
// of a create or save (ST9, R9): no authentication object keeps the stored
// secret; the type none, empty or null removes it; the type username with a
// password, or bearerToken with a token, writes it, and an empty one keeps
// the stored secret, as the form sends it only when it was filled. A secret
// with a control character or of more than 65536 bytes, the type username
// without a user name and a type other than username, bearerToken and ntlm
// are refused with 400 at the field.
func UpstreamSecretOf(body map[string]any) (UpstreamSecret, error) {
	keep := UpstreamSecret{Action: SecretKeep}
	v, found := valueAt(body, authField)
	if !found || v == nil {
		return keep, nil
	}
	auth, ok := v.(map[string]any)
	if !ok {
		return keep, &Refusal{Code: CodeRemoteSecret, Field: authField, Message: "the authentication is no object"}
	}
	kind, _ := auth["type"].(string)
	key := ""
	switch {
	case kind == "" || strings.EqualFold(kind, "none"):
		return UpstreamSecret{Action: SecretRemove}, nil
	case kind == "username" || kind == "ntlm":
		key = "password"
		if user, _ := auth["username"].(string); user == "" {
			return keep, &Refusal{Code: CodeRemoteSecret, Field: authField + ".username",
				Message: "the authentication of type " + kind + " needs a user name"}
		}
	case kind == "bearerToken":
		key = "bearerToken"
	default:
		return keep, &Refusal{Code: CodeRemoteSecret, Field: authField + ".type", Message: fmt.Sprintf(
			"the authentication type %q is not known; use username or bearerToken", kind)}
	}
	field := authField + "." + key
	value, isText := auth[key].(string)
	switch {
	case auth[key] == nil || isText && value == "":
		return keep, nil
	case !isText:
		return keep, &Refusal{Code: CodeRemoteSecret, Field: field, Message: "the " + key + " is no string"}
	case len(value) > maxUpstreamSecret:
		return keep, &Refusal{Code: CodeRemoteSecret, Field: field, Message: fmt.Sprintf("the %s has more than %d bytes", key, maxUpstreamSecret)}
	case strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return keep, &Refusal{Code: CodeRemoteSecret, Field: field, Message: "the " + key + " holds a control character"}
	}
	return UpstreamSecret{Action: SecretWrite, Value: value}, nil
}

// CheckRepositoryDelete refuses the delete of a repository that a group
// names as a member, 409 with the groups sorted (ST5). groups maps each
// group to its members.
func CheckRepositoryDelete(name string, groups map[string][]string) error {
	var holders []string
	for group, members := range groups {
		if group != name && slices.Contains(members, name) {
			holders = append(holders, group)
		}
	}
	if len(holders) == 0 {
		return nil
	}
	slices.Sort(holders)
	return &Refusal{Code: CodeMemberInUse, Message: fmt.Sprintf("the repository %s is a member of the group %s; remove it there first",
		name, strings.Join(holders, ", "))}
}

// generatedActions are the actions of the administration privileges that
// a new repository generates, viewActions those of its view privileges
// (internal/authz, generatedPrivileges).
var (
	generatedActions = []string{"*", "browse", "read", "edit", "delete"}
	viewActions      = []string{"*", "browse", "read", "edit", "add", "delete"}
)

// CheckRepositoryGrant checks a create against the privileges the new
// repository generates (ST10, ST12, I9): a role may name such a privilege
// before the repository exists, and the create makes it grant. The subject
// holds nexus:* or every permission of them: the administration and the
// view privileges of the repository by name, and with newFormat, the first
// repository of its format, the view privileges of every repository of the
// format. Else 403 grant-exceeds naming the first permission it lacks.
func CheckRepositoryGrant(format, name string, newFormat bool, holds func(permission string) bool) error {
	if holds(fullPermission) {
		return nil
	}
	var needed []string
	for _, action := range generatedActions {
		needed = append(needed, "nexus:repository-admin:"+format+":"+name+":"+action)
	}
	for _, action := range viewActions {
		needed = append(needed, "nexus:repository-view:"+format+":"+name+":"+action)
	}
	if newFormat {
		for _, action := range viewActions {
			needed = append(needed, "nexus:repository-view:"+format+":*:"+action)
		}
	}
	for _, p := range needed {
		if !holds(p) {
			return &Refusal{Code: CodeGrantExceeds, Message: fmt.Sprintf(
				"the new repository %s generates privileges with the permission %s, which the signed in user does not hold", name, p)}
		}
	}
	return nil
}

// CheckMemberGrant checks the members a group adds, mapped to their
// formats (ruling ST12): whoever may read the group reads them, so the
// subject holds nexus:* or the view read permission of each; else 403
// grant-exceeds naming the first member, in name order, it may not read.
func CheckMemberGrant(group string, members map[string]string, holds func(permission string) bool) error {
	if holds(fullPermission) {
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(members)) {
		p := "nexus:repository-view:" + members[name] + ":" + name + ":read"
		if !holds(p) {
			return &Refusal{Code: CodeGrantExceeds, Message: fmt.Sprintf(
				"the group %s would serve the repository %s, whose content needs the permission %s, which the signed in user does not hold",
				group, name, p)}
		}
	}
	return nil
}

// ClosedAddress reports an address of a closed network (ruling ST11): the
// set of internal/proxy, which its dial guard reads as well.
func ClosedAddress(a netip.Addr) bool { return proxy.Closed(a) }

// ServerAttributes are the attributes of a repository record that this
// server reads as its own placement and limits, with their value on a
// create (ruling ST13; internal/repos WriteSettings and ProxySettings):
// nil for an attribute that is absent by default. They are set on the
// host, never taken from a request body.
var ServerAttributes = map[string]any{
	"write_store":            WriteStore,
	"max_body_bytes":         int64(10 << 30), // the numbers of repos.DefaultMaxBodyBytes and DefaultFreeSpaceFloor
	"free_space_floor_bytes": int64(2 << 30),
	"remote_public_only":     nil,
}

// CreateAttributes are the server owned attributes of a create of the kind
// with the body (ST13, SR5): ServerAttributes, and for a hosted repository
// write_store set to the store the body names, where its uploads land.
func CreateAttributes(kind string, body map[string]any) map[string]any {
	out := maps.Clone(ServerAttributes)
	if name, _ := textAt(body, "storage.blobStoreName"); kind == "hosted" && name != "" {
		out["write_store"] = name
	}
	return out
}

// sameText compares two decoded values in their text form, so that a
// number of the body and one of the record compare by value.
func sameText(a, b any) bool { return textOf(a) == textOf(b) }

func textOf(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// CheckServerAttributes refuses, with 400 not-offered at the key, a server
// owned attribute of a body whose value differs from the stored one, or on
// a create from the default (ruling ST13). null and an absent key send no
// value.
func CheckServerAttributes(kind string, body, stored map[string]any, create bool) error {
	var list Refusals
	defaults := CreateAttributes(kind, body)
	for _, key := range slices.Sorted(maps.Keys(ServerAttributes)) {
		v, sent := body[key]
		if !sent || v == nil {
			continue
		}
		held, found := stored[key]
		if create || stored == nil {
			held, found = defaults[key], defaults[key] != nil
		}
		if found && sameText(held, v) {
			continue
		}
		list = append(list, &Refusal{Code: CodeNotOffered, Field: key, Message: fmt.Sprintf(
			"the attribute %s is set on the host, not through a request; send it as null or with its value %v", key, held)})
	}
	return list.err()
}

// CheckSegment gives the format of the REST path segment of a create, or
// the 501 of CheckRepositoryKind, which names the segment when it names no
// format.
func CheckSegment(segment, kind string) (string, error) {
	format := FormatOfSegment(segment)
	if format == "" {
		return "", CheckRepositoryKind(segment+" (no segment of a format)", kind)
	}
	return format, CheckRepositoryKind(format, kind)
}
