package auth

import (
	"context"
	"errors"

	"github.com/mirosso25/pipesmith-registry/internal/model"
)

// Directory signs a user in against one LDAP connection. Package ldapauth
// implements it; the service tries the directories in the order it got them,
// which is the order of the connections.
//
// Login returns the user (Source "LDAP") and the names of the user's
// groups. Each group grants the role whose id equals its name, so a
// connection with ldapGroupsAsRoles off returns no groups. Login wraps
// ErrWrongPair for an unknown user and for a wrong password alike, and
// ErrUnreachable when the directory did not answer. The error must not hold
// the password.
type Directory interface {
	Login(ctx context.Context, username, password string) (model.User, []string, error)
}

var (
	// ErrWrongPair is a user name and password the directory does not accept.
	ErrWrongPair = errors.New("auth: wrong user name or password")
	// ErrUnreachable is a directory that did not answer.
	ErrUnreachable = errors.New("auth: directory unreachable")
)
