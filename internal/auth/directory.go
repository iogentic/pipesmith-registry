package auth

import (
	"context"
	"errors"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// Directory signs a user in against one LDAP connection. Package ldapauth
// implements it; the service tries the directories in the order it got them,
// which is the order of the connections.
//
// Login returns the user (Source "LDAP") and the names of the user's
// groups. Each group grants the role whose id equals its name, so a
// connection with ldapGroupsAsRoles off returns no groups. Login wraps
// ErrWrongPair for an unknown user and for a wrong password alike,
// ErrUnreachable when the directory did not answer, and ErrBusy when the
// connection had no place for another sign in and asked nothing. The error
// must not hold the password.
//
// Lookup finds a user by name under the service bind, the search Login runs
// before its bind, and returns the user and its groups as Login does, with
// no bind as the user. It wraps ErrNoUser when the directory holds no single
// entry of the name, which covers a user that is gone and one the user
// filter of the connection leaves out, and ErrUnreachable when the directory
// did not answer. The user of a token is found this way (tokens.go).
type Directory interface {
	Login(ctx context.Context, username, password string) (model.User, []string, error)
	Lookup(ctx context.Context, username string) (model.User, []string, error)
}

var (
	// ErrWrongPair is a user name and password the directory does not accept.
	ErrWrongPair = errors.New("auth: wrong user name or password")
	// ErrUnreachable is a directory that did not answer.
	ErrUnreachable = errors.New("auth: directory unreachable")
	// ErrNoUser is a user name the directory holds no single entry of.
	ErrNoUser = errors.New("auth: the directory holds no such user")
	// ErrBusy is a sign in the directory had no place for; it asked nothing.
	ErrBusy = errors.New("auth: the directory has no place for another sign in")
)
