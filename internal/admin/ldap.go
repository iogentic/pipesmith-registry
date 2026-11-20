package admin

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The rules of the LDAP connections (leg 6-identity, contract S26 and S27):
// the form of a connection, its bind password (ruling I18), who may change
// a directory (I17) and the order of the connections. The bind password
// lives in the secret file of the connection (decision P9), never in
// state.sqlite and never in an answer (A11).

// ldapRequired are the fields of the form the contract marks as required,
// besides authPassword (CheckLDAPPassword) and groupType (required while
// ldapGroupsAsRoles is true).
var ldapRequired = []string{"name", "protocol", "host", "port", "searchBase", "authScheme", "connectionTimeoutSeconds",
	"connectionRetryDelaySeconds", "maxIncidentsCount"}

// ldapFormKeys are the keys of the form of contract S27, and id, order and
// caFile, which the interface may send back from a read and the server does
// not read (ruling I27).
var ldapFormKeys = []string{"name", "protocol", "useTrustStore", "host", "port", "searchBase", "authScheme", "authRealm",
	"authUsername", "authPassword", "connectionTimeoutSeconds", "connectionRetryDelaySeconds", "maxIncidentsCount", "userBaseDn",
	"userSubtree", "userObjectClass", "userLdapFilter", "userIdAttribute", "userRealNameAttribute", "userEmailAddressAttribute",
	"userPasswordAttribute", "ldapGroupsAsRoles", "groupType", "groupBaseDn", "groupSubtree", "groupObjectClass",
	"groupIdAttribute", "groupMemberAttribute", "groupMemberFormat", "userMemberOfAttribute", "id", "order", "caFile"}

// ldapDirectoryFields decide which entries are the users of a connection
// and which groups they have (ruling I17): the address, the search base,
// the user settings and the group settings.
var ldapDirectoryFields = []string{"protocol", "host", "port", "searchBase", "userBaseDn", "userSubtree", "userObjectClass",
	"userLdapFilter", "userIdAttribute", "userRealNameAttribute", "userEmailAddressAttribute", "userPasswordAttribute",
	"userMemberOfAttribute", "ldapGroupsAsRoles", "groupType", "groupBaseDn", "groupSubtree", "groupObjectClass",
	"groupIdAttribute", "groupMemberAttribute", "groupMemberFormat"}

const (
	schemeNone   = "NONE"
	schemeSimple = "SIMPLE"
	// maxLDAPTimeout bounds the connection timeout of a form in seconds.
	maxLDAPTimeout = 3600
	// maxLDAPRetryDelay bounds the retry delay of a form in seconds.
	maxLDAPRetryDelay = 86400
	// maxCABytes bounds a CA file of a connection.
	maxCABytes = 1 << 20
)

// LDAPScheme is the authentication scheme in the spelling ldapauth reads:
// upper case, "-" as "_".
func LDAPScheme(scheme string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(scheme), "-", "_"))
}

// NeedsBind reports a scheme that binds with a password: SIMPLE.
func NeedsBind(scheme string) bool { return LDAPScheme(scheme) == schemeSimple }

// LDAPConnection reads the connection of a form body (contract S27), the
// decoded JSON object, and checks its fields. prefix starts every field id,
// "connection." for the sign in check. A key that is no field of the form
// (ldapFormKeys, ruling I27) and a value of a wrong type are refused with
// body-invalid, a required field that is absent or null and a value
// the connection cannot take with ldap-field, a name outside the pattern
// with name-invalid, the schemes DIGEST_MD5 and CRAM_MD5 with not-offered;
// a form whose fields pass but that ldapauth.New refuses (a DN, an
// attribute name, the user filter) at *. The fields id, order and caFile of
// the body are not read: the id and the order belong to the stored
// connection and the CA file is set on the host (ldap ca). The password is
// not part of the connection (LDAPPassword).
func LDAPConnection(body map[string]any, prefix string) (model.LDAPServer, error) {
	var server model.LDAPServer
	var unknown Refusals
	for _, key := range slices.Sorted(maps.Keys(body)) {
		if !slices.Contains(ldapFormKeys, key) {
			unknown = append(unknown, &Refusal{Code: CodeBodyInvalid, Field: prefix + key,
				Message: fmt.Sprintf("the key %s is no field of an LDAP connection", key)})
		}
	}
	if err := unknown.err(); err != nil {
		return server, err
	}
	data, err := json.Marshal(body)
	if err != nil {
		return server, &Refusal{Code: CodeBodyInvalid, Field: whole(prefix), Message: "the connection is no JSON object"}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&server); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			return server, &Refusal{Code: CodeBodyInvalid, Field: prefix + typeErr.Field,
				Message: fmt.Sprintf("the value of %s is not of the type of the field", typeErr.Field)}
		}
		return server, &Refusal{Code: CodeBodyInvalid, Field: whole(prefix), Message: "the connection is no JSON object"}
	}
	server.ID, server.Order, server.CAFile = "", 0, ""

	var list Refusals
	field := func(key, format string, args ...any) {
		list = append(list, &Refusal{Code: CodeLDAPField, Field: prefix + key, Message: fmt.Sprintf(format, args...)})
	}
	required := slices.Clone(ldapRequired)
	if server.LDAPGroupsAsRoles {
		required = append(required, "groupType")
	}
	missing := map[string]bool{}
	for _, key := range required {
		if v, ok := body[key]; !ok || v == nil {
			missing[key] = true
			field(key, "the field %s is required", key)
		}
	}
	if !missing["name"] {
		if err := CheckName(KindLDAPConnection, server.Name); err != nil {
			r := *err.(*Refusal)
			r.Field = prefix + r.Field
			list = append(list, &r)
		}
	}
	if p := strings.ToLower(strings.TrimSpace(server.Protocol)); !missing["protocol"] && p != "ldap" && p != "ldaps" {
		field("protocol", "the protocol %q is neither ldap nor ldaps", server.Protocol)
	}
	if !missing["host"] && (strings.TrimSpace(server.Host) == "" || strings.ContainsAny(server.Host, " \t\r\n/?#@")) {
		field("host", "the host %q is no host name or address", server.Host)
	}
	if !missing["port"] && (server.Port < 1 || server.Port > 65535) {
		field("port", "the port %d is outside 1 to 65535", server.Port)
	}
	if !missing["searchBase"] && strings.TrimSpace(server.SearchBase) == "" {
		field("searchBase", "the search base is empty")
	}
	if !missing["authScheme"] {
		switch scheme := LDAPScheme(server.AuthScheme); scheme {
		case schemeNone:
		case schemeSimple:
			if strings.TrimSpace(server.AuthUsername) == "" {
				field("authUsername", "the scheme SIMPLE needs the name to bind with")
			}
		case "DIGEST_MD5", "CRAM_MD5":
			list = append(list, &Refusal{Code: CodeNotOffered, Field: prefix + "authScheme", Message: fmt.Sprintf(
				"the LDAP connection field authScheme cannot be %s, because this server binds with NONE and SIMPLE only", scheme)})
		default:
			field("authScheme", "the scheme %q is none of NONE, SIMPLE, DIGEST_MD5 and CRAM_MD5", server.AuthScheme)
		}
	}
	if !missing["connectionTimeoutSeconds"] && (server.ConnectionTimeoutSeconds < 1 || server.ConnectionTimeoutSeconds > maxLDAPTimeout) {
		field("connectionTimeoutSeconds", "the connection timeout is 1 to %d seconds", maxLDAPTimeout)
	}
	if !missing["connectionRetryDelaySeconds"] &&
		(server.ConnectionRetryDelaySeconds < 0 || server.ConnectionRetryDelaySeconds > maxLDAPRetryDelay) {
		field("connectionRetryDelaySeconds", "the retry delay is 0 to %d seconds", maxLDAPRetryDelay)
	}
	if !missing["maxIncidentsCount"] && server.MaxIncidentsCount < 0 {
		field("maxIncidentsCount", "the count of incidents is 0 or more")
	}
	if g := strings.ToLower(strings.TrimSpace(server.GroupType)); !missing["groupType"] && g != "static" && g != "dynamic" &&
		(server.LDAPGroupsAsRoles || g != "") {
		field("groupType", "the group type %q is neither static nor dynamic", server.GroupType)
	}
	if err := list.err(); err != nil {
		return server, err
	}
	// what New refuses beyond the fields above; the bind password is a
	// stand in, since the form checks it apart
	if _, err := ldapauth.New(server, "-"); err != nil {
		return server, &Refusal{Code: CodeLDAPField, Field: whole(prefix),
			Message: strings.TrimPrefix(err.Error(), ldapauth.ErrConfiguration.Error()+": ")}
	}
	return server, nil
}

// whole is the field id of the form as a whole: * for a body that is the
// form, the key of the form in a body that holds it.
func whole(prefix string) string {
	if prefix == "" {
		return "*"
	}
	return strings.TrimSuffix(prefix, ".")
}

// LDAPPassword is the field authPassword of a form body: nil when the body
// leaves it out or sends null, else its text. Another type is refused with
// body-invalid at it.
func LDAPPassword(body map[string]any, prefix string) (*string, error) {
	v, ok := body["authPassword"]
	if !ok || v == nil {
		return nil, nil
	}
	text, ok := v.(string)
	if !ok {
		return nil, &Refusal{Code: CodeBodyInvalid, Field: prefix + "authPassword", Message: "the value of authPassword is no string"}
	}
	return &text, nil
}

// CheckLDAPPassword checks the bind password of a create or a save of a
// connection of the scheme (ruling I18). A scheme without a bind takes no
// password, and none is checked. For a scheme with a bind a create needs a
// password, a save keeps the stored secret when the body sends none (nil)
// and refuses an empty one, both with 400 at authPassword. A password has
// at most state.MaxSecretBytes bytes and no control character, since the
// secret file drops a line end at its end.
func CheckLDAPPassword(scheme string, password *string, create bool, prefix string) error {
	if !NeedsBind(scheme) || password == nil && !create {
		return nil
	}
	words := ""
	switch {
	case password == nil || *password == "":
		words = "the scheme SIMPLE needs a bind password"
	case len(*password) > state.MaxSecretBytes:
		words = fmt.Sprintf("a bind password has at most %d bytes", state.MaxSecretBytes)
	case strings.ContainsFunc(*password, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		words = "the bind password holds a control character"
	default:
		return nil
	}
	return &Refusal{Code: CodePasswordInvalid, Field: prefix + "authPassword", Message: words}
}

// fieldsOf is the connection as a decoded object of the contract shape.
func fieldsOf(server model.LDAPServer) map[string]any {
	data, _ := json.Marshal(server) // a struct of strings, numbers and booleans always encodes
	var out map[string]any
	_ = json.Unmarshal(data, &out) // the encoding of a struct is an object
	return out
}

// LDAPDirectoryChanged reports a save, or the form of a check, that changes
// the address, the search base, a user setting or a group setting of the
// stored connection (rulings I17 and I22).
func LDAPDirectoryChanged(stored, saved model.LDAPServer) bool {
	return changedFields(stored, saved, ldapDirectoryFields)
}

func changedFields(a, b model.LDAPServer, keys []string) bool {
	x, y := fieldsOf(a), fieldsOf(b)
	return slices.ContainsFunc(keys, func(key string) bool { return fmt.Sprint(x[key]) != fmt.Sprint(y[key]) })
}

// CheckLDAPGrant refuses a change of a directory connection with 403
// grant-exceeds unless the subject holds the full permission (ruling I17):
// a connection decides through the groups of its users which roles they
// get, as a role whose id is a group name (contract S22). what names the
// change in the message.
func CheckLDAPGrant(what string, holds func(permission string) bool) error {
	if holds(fullPermission) {
		return nil
	}
	return &Refusal{Code: CodeGrantExceeds, Message: fmt.Sprintf(
		"%s decides which roles the users of a directory get through their groups, and needs the permission %s, "+
			"which the signed in user does not hold", what, fullPermission)}
}

// CheckLDAPOrder checks the names of a change of order (contract S26): each
// stored connection once, no other name; 400 ldap-order at *.
func CheckLDAPOrder(s *model.Snapshot, names []string) error {
	var stored []string
	for _, l := range s.LDAP {
		stored = append(stored, l.Name)
	}
	words := ""
	for i, name := range names {
		switch {
		case words != "":
		case !slices.Contains(stored, name):
			words = fmt.Sprintf("no LDAP connection %q", name)
		case slices.Contains(names[:i], name):
			words = fmt.Sprintf("the LDAP connection %s is named twice", name)
		}
	}
	if words == "" && len(names) != len(stored) {
		for _, name := range stored {
			if words == "" && !slices.Contains(names, name) {
				words = fmt.Sprintf("the LDAP connection %s is missing", name)
			}
		}
	}
	if words == "" {
		return nil
	}
	return &Refusal{Code: CodeLDAPOrder, Field: "*", Message: words + "; name every connection once, in the new order"}
}

// SetLDAPOrder gives the connections the order of names, 1 for the first,
// and reports whether an order changed. The names passed CheckLDAPOrder.
func SetLDAPOrder(s *model.Snapshot, names []string) bool {
	changed := false
	for i := range s.LDAP {
		order := slices.Index(names, s.LDAP[i].Name) + 1
		changed = changed || s.LDAP[i].Order != order
		s.LDAP[i].Order = order
	}
	return changed
}

// NextLDAPOrder is the order of a new connection: one past the highest.
func NextLDAPOrder(s *model.Snapshot) int {
	next := 1
	for _, l := range s.LDAP {
		next = max(next, l.Order+1)
	}
	return next
}

// NewLDAPID is the id of a new connection: a random UUID of version 4.
func NewLDAPID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// The secret file of a connection

// errNoDataDir is the error of a secret write of a store without a data
// directory.
var errNoDataDir = errors.New("the identity store has no data directory for the secret file")

// LDAPSecret is the bind password the server binds the connection id with:
// its environment variable (ldapauth.PasswordFromEnv), else its secret file,
// else empty.
func LDAPSecret(dataDir, id string) (string, error) {
	if value, ok := ldapauth.PasswordFromEnv(id); ok {
		return value, nil
	}
	if dataDir == "" {
		return "", nil
	}
	data, err := state.ReadSecret(state.LDAPSecretPath(dataDir, id))
	if errors.Is(err, state.ErrNoSecret) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r"), nil
}

// HasLDAPSecretFile reports whether the connection id has a secret file
// that state.ReadSecret takes; false without a data directory.
func HasLDAPSecretFile(dataDir, id string) (bool, error) {
	if dataDir == "" {
		return false, nil
	}
	_, err := state.ReadSecret(state.LDAPSecretPath(dataDir, id))
	if errors.Is(err, state.ErrNoSecret) {
		return false, nil
	}
	return err == nil, err
}

// WriteLDAPSecret writes the bind password of the connection id into its
// secret file, mode 0600 (state.WriteSecret). undo puts back what the file
// held before, for a change whose write of the state fails after it.
func WriteLDAPSecret(dataDir, id, password string) (undo func(), err error) {
	if dataDir == "" {
		return nil, errNoDataDir
	}
	path := state.LDAPSecretPath(dataDir, id)
	undo, err = restorer(path)
	if err != nil {
		return nil, err
	}
	if err := state.WriteSecret(path, []byte(password)); err != nil {
		return nil, err
	}
	return undo, nil
}

// RemoveLDAPSecret removes the secret file of the connection id, when there
// is one. undo puts the file back.
func RemoveLDAPSecret(dataDir, id string) (undo func(), err error) {
	if dataDir == "" {
		return func() {}, nil
	}
	path := state.LDAPSecretPath(dataDir, id)
	undo, err = restorer(path)
	if err != nil {
		return nil, err
	}
	if err := state.DeleteSecret(path); err != nil {
		return nil, err
	}
	return undo, nil
}

// restorer reads the secret file at path and gives the function that puts
// it back as it is now: the old bytes, or no file.
func restorer(path string) (func(), error) {
	old, err := state.ReadSecret(path)
	switch {
	case errors.Is(err, state.ErrNoSecret):
		return func() { _ = state.DeleteSecret(path) }, nil // the error of the change counts
	case err != nil:
		return nil, err
	}
	return func() { _ = state.WriteSecret(path, old) }, nil // the error of the change counts
}

// LDAPCA reads the CA file of a connection: a regular file of at most 1 MiB
// that holds a certificate in PEM form.
func LDAPCA(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxCABytes {
		return nil, fmt.Errorf("%s is no regular file of at most %d bytes", path, maxCABytes)
	}
	return os.ReadFile(path) // #nosec G304 -- the CA file the operator named with ldap ca.
}

// LDAPOrderChanged reports whether names, which passed CheckLDAPOrder, put
// the connections in another sequence than the stored order, connections of
// one number in their stored place (ruling I26). The numbers themselves are
// not compared.
func LDAPOrderChanged(s *model.Snapshot, names []string) bool {
	stored := slices.Clone(s.LDAP)
	slices.SortStableFunc(stored, func(a, b model.LDAPServer) int { return a.Order - b.Order })
	for i, l := range stored {
		if i >= len(names) || names[i] != l.Name {
			return true
		}
	}
	return len(names) != len(stored)
}
