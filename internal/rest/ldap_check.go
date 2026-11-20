package rest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
)

// The checks of an LDAP form (contract S27): connect and bind, list the
// users of the mapping, sign one user in. A check writes nothing. A failure
// of the directory answers 200 with ok false and a message, a form the
// rules refuse 400 with the field ids of the contract, "connection." in
// front of them for the sign in check. A check connects to the host and the
// port of the form only, with the time limits of ldapauth (ruling I20), and
// sends connect, bind and search, and for the sign in check the bind of the
// user, nothing else. A check reads within the bounds of
// ldapauth.WithCheckBounds and lasts at most ldapCheckTime (I24).

// ldapCheckUsers bounds the users of the check of the user mapping.
const ldapCheckUsers = 20

// ldapCheckTime bounds one check (ruling I24); the connection timeout of
// the form counts at most as much. Tests shorten it.
var ldapCheckTime = 15 * time.Second

type checkAnswer struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

type mappedUser struct {
	UserID string   `json:"userId"`
	Name   string   `json:"name"`
	Email  string   `json:"email"`
	Groups []string `json:"groups"`
}

type mappingAnswer struct {
	OK      bool         `json:"ok"`
	Message string       `json:"message"`
	Users   []mappedUser `json:"users"`
}

// failed is the answer of a check that the directory, or what the check
// needs of the stored connection, did not pass.
func failed(message string) (Answer, error) { return ok(checkAnswer{Message: message}) }

// words is an error of ldapauth for a message of a check.
func words(err error) string { return strings.TrimPrefix(err.Error(), "ldapauth: ") }

// unreachableMessage is the one answer of a check that reached no directory
// (ruling I25).
const unreachableMessage = "the directory could not be reached"

// checkLogger names the log lines of the checks.
const checkLogger = "pipesmith_registry.security"

// checkFailure is the message of a check the directory did not pass: one
// text for a directory the check could not reach, whose detail goes to the
// server log (ruling I25); a check past its time names the bound, a bound
// of the reads its text (I24).
func checkFailure(ctx context.Context, err error) string {
	if errors.Is(err, ldapauth.ErrUnreachable) {
		slog.Default().Warn("LDAP check: "+words(err), "logger", checkLogger)
		return unreachableMessage
	}
	if ctx.Err() != nil && errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("the check took longer than %s, the bound of a check", ldapCheckTime)
	}
	return words(err)
}

// checkDirectory builds the directory of a check from the form. A blank
// authPassword of a form whose name a stored connection has takes the bind
// password that connection binds with (ruling I19); sending it to another
// protocol, host or port than the stored ones needs the full permission,
// like a save that changes the address (I17). An ldaps connection is
// verified against the system roots plus the stored CA file of that
// connection (I20). failure is the message of a check that cannot run; err
// is a refusal of the form or an error of the server.
func checkDirectory(env *Env, w IdentityWrite, form map[string]any, prefix string) (d *ldapauth.Directory, failure string, err error) {
	server, err := admin.LDAPConnection(form, prefix)
	if err != nil {
		return nil, "", err
	}
	password, err := admin.LDAPPassword(form, prefix)
	if err != nil {
		return nil, "", err
	}
	snap, err := w.Store.Load()
	if err != nil {
		return nil, "", err
	}
	i := ldapIndex(snap, server.Name)
	bind := ""
	switch blank := password == nil || *password == ""; {
	case !admin.NeedsBind(server.AuthScheme):
	case !blank:
		if err := admin.CheckLDAPPassword(server.AuthScheme, password, false, prefix); err != nil {
			return nil, "", err
		}
		bind = *password
	case i < 0:
		return nil, "", admin.CheckLDAPPassword(server.AuthScheme, nil, true, prefix)
	default:
		stored := snap.LDAP[i]
		if admin.LDAPDirectoryChanged(stored, server) {
			if err := admin.CheckLDAPGrant("a check that binds with the stored bind password of the LDAP connection "+stored.Name+
				" and other directory settings than the stored ones", env.permits); err != nil {
				return nil, "", err
			}
		}
		if bind, err = admin.LDAPSecret(w.Store.DataDir(), stored.ID); err != nil {
			return nil, fmt.Sprintf("the stored bind password of the LDAP connection %s cannot be read: %v", stored.Name, err), nil
		}
		if bind == "" {
			return nil, fmt.Sprintf("the LDAP connection %s has no stored bind password; enter it in the form", stored.Name), nil
		}
	}
	var opts []ldapauth.Option
	if i >= 0 && snap.LDAP[i].CAFile != "" && strings.EqualFold(strings.TrimSpace(server.Protocol), "ldaps") {
		pem, err := admin.LDAPCA(snap.LDAP[i].CAFile)
		if err != nil {
			return nil, fmt.Sprintf("the CA file of the LDAP connection %s cannot be read: %v", server.Name, err), nil
		}
		opts = append(opts, ldapauth.WithCACertificates(pem))
	}
	opts = append(opts, ldapauth.WithCheckBounds())
	server.ConnectionTimeoutSeconds = min(server.ConnectionTimeoutSeconds, max(1, int(ldapCheckTime/time.Second)))
	if d, err = ldapauth.New(server, bind, opts...); err != nil {
		return nil, words(err), nil
	}
	return d, "", nil
}

// checkContext is the context of one check.
func checkContext(env *Env) (context.Context, context.CancelFunc) {
	ctx := env.Context
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, ldapCheckTime)
}

// formOf reads the body of a check that is the form.
func formOf(body []byte) (map[string]any, error) {
	var form map[string]any
	return form, decode(body, '{', &form)
}

// VerifyLDAPConnection is POST /api/ldap/verify-connection: connect, bind
// and read the search base with the form; 200 {ok, message}.
func VerifyLDAPConnection(env *Env, w IdentityWrite, body []byte) (Answer, error) {
	form, err := formOf(body)
	if err != nil {
		return refused(err)
	}
	d, failure, err := checkDirectory(env, w, form, "")
	switch {
	case err != nil:
		return refused(err)
	case d == nil:
		return failed(failure)
	}
	ctx, cancel := checkContext(env)
	defer cancel()
	done, err := d.Check(ctx)
	if err != nil {
		return failed(checkFailure(ctx, err))
	}
	return ok(checkAnswer{OK: true, Message: done})
}

// VerifyLDAPUserMapping is POST /api/ldap/verify-user-mapping: the users
// the form finds, at most 20, each with its groups; 200 {ok, message,
// users}. ok is false when the search fails or finds nobody.
func VerifyLDAPUserMapping(env *Env, w IdentityWrite, body []byte) (Answer, error) {
	form, err := formOf(body)
	if err != nil {
		return refused(err)
	}
	d, failure, err := checkDirectory(env, w, form, "")
	switch {
	case err != nil:
		return refused(err)
	case d == nil:
		return ok(mappingAnswer{Message: failure, Users: []mappedUser{}})
	}
	ctx, cancel := checkContext(env)
	defer cancel()
	members, more, err := d.Users(ctx, ldapCheckUsers)
	if err != nil {
		return ok(mappingAnswer{Message: checkFailure(ctx, err), Users: []mappedUser{}})
	}
	users := make([]mappedUser, 0, len(members))
	for _, m := range members {
		users = append(users, mappedUser{UserID: m.User.ID, Name: m.User.FirstName, Email: m.User.Email, Groups: m.Groups})
	}
	message := fmt.Sprintf("the search found %d users", len(users))
	switch {
	case len(users) == 0:
		message = "the search found no user; check the user base, the object class, the id attribute and the filter"
	case more:
		message = fmt.Sprintf("the search found more than %d users; the first %d are shown", ldapCheckUsers, len(users))
	}
	return ok(mappingAnswer{OK: len(users) > 0, Message: message, Users: users})
}

// VerifyLDAPLogin is POST /api/ldap/verify-login: {connection, username,
// password}, the user signs in with the form; 200 {ok, message}. The field
// ids of a refusal of the form start with "connection.".
func VerifyLDAPLogin(env *Env, w IdentityWrite, body []byte) (Answer, error) {
	var b map[string]any
	if err := decode(body, '{', &b); err != nil {
		return refused(err)
	}
	form, isForm := b["connection"].(map[string]any)
	if !isForm {
		return refused(&admin.Refusal{Code: admin.CodeBodyInvalid, Field: "connection", Message: "the connection is no JSON object"})
	}
	var list admin.Refusals
	pair := map[string]string{}
	for _, key := range []string{"username", "password"} {
		text, isText := b[key].(string)
		switch {
		case b[key] != nil && !isText:
			list = append(list, &admin.Refusal{Code: admin.CodeBodyInvalid, Field: key, Message: "the value of " + key + " is no string"})
		case text == "":
			list = append(list, &admin.Refusal{Code: admin.CodeLDAPField, Field: key, Message: "the field " + key + " is required"})
		}
		pair[key] = text
	}
	d, failure, err := checkDirectory(env, w, form, "connection.")
	if err := errors.Join(err, joined(list)); err != nil {
		return refused(err)
	}
	if d == nil {
		return failed(failure)
	}
	ctx, cancel := checkContext(env)
	defer cancel()
	user, groups, err := d.Login(ctx, pair["username"], pair["password"])
	switch {
	case errors.Is(err, ldapauth.ErrInvalidCredentials):
		return failed("the directory did not accept the user name or the password")
	case err != nil:
		return failed(checkFailure(ctx, err))
	}
	text := "none"
	if len(groups) > 0 {
		text = strings.Join(groups, ", ")
	}
	return ok(checkAnswer{OK: true, Message: fmt.Sprintf("user %s signs in; groups: %s", user.ID, text)})
}

// joined is a list of refusals as an error, nil when it is empty.
func joined(list admin.Refusals) error {
	if len(list) == 0 {
		return nil
	}
	return list
}
