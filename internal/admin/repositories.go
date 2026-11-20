package admin

import (
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

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

// CheckStorage checks the store of a repository create or save (ST2): a
// hosted or proxy repository names local-write in storage.blobStoreName, a
// group a store of stores. write_store is a server owned attribute
// (CheckServerAttributes). A value the stored record holds is never
// refused (R6);
// stored is nil for a create.
func CheckStorage(kind string, body, stored map[string]any, stores []string) error {
	var list Refusals
	name, _ := textAt(body, "storage.blobStoreName")
	held, _ := textAt(stored, "storage.blobStoreName")
	switch {
	case stored != nil && name == held && name != "":
	case name == "":
		list = append(list, &Refusal{Code: CodeStoreUnknown, Field: "storage.blobStoreName",
			Message: "a repository needs storage.blobStoreName, the name of a blob store"})
	case kind != "group" && name != WriteStore:
		list = append(list, &Refusal{Code: CodeWriteStore, Field: "storage.blobStoreName", Message: fmt.Sprintf(
			"a %s repository of this server writes into the store %s, not into %s; name %s", kind, WriteStore, name, WriteStore)})
	case !slices.Contains(stores, name):
		list = append(list, &Refusal{Code: CodeStoreUnknown, Field: "storage.blobStoreName", Message: fmt.Sprintf(
			"the blob store %s does not exist", name)})
	}
	return list.err()
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
func CheckServerAttributes(body, stored map[string]any, create bool) error {
	var list Refusals
	for _, key := range slices.Sorted(maps.Keys(ServerAttributes)) {
		v, sent := body[key]
		if !sent || v == nil {
			continue
		}
		held, found := stored[key]
		if create || stored == nil {
			held, found = ServerAttributes[key], ServerAttributes[key] != nil
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
