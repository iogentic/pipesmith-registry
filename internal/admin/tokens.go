package admin

import (
	"fmt"
	"regexp"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The rules of the tokens and of the own account (leg 6-api, contract S29
// and S41).

// MaxTokenName is the most characters of the name of a token (ruling AP2).
const MaxTokenName = 64

// tokenNamePattern is the name pattern of the records with the bound of a
// token name.
var tokenNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// CheckTokenName refuses a token name outside the name pattern or longer
// than 64 characters with 400 at name.
func CheckTokenName(name string) error {
	if tokenNamePattern.MatchString(name) {
		return nil
	}
	return &Refusal{Code: CodeTokenName, Field: "name", Message: fmt.Sprintf(
		"the token name %q is not allowed; use 1 to %d letters, digits, dots, hyphens and underscores, starting with a letter or a digit",
		name, MaxTokenName)}
}

// TokenKindRefusal is the refusal of a token kind the server does not
// issue, with 400 at kind and the reason.
func TokenKindRefusal(reason string) error {
	return &Refusal{Code: CodeTokenKind, Field: "kind", Message: reason}
}

// TokenNameTaken is the refusal of a token name the user holds for the
// kind, with 409.
func TokenNameTaken(name, kind string) error {
	return &Refusal{Code: CodeNameExists, Message: fmt.Sprintf("a %s token of this user has the name %s", kind, name)}
}

// CheckSignedInDirectly refuses with 403 a subject that signed in with a
// token: an issue or a revoke of a token and a change of the own account
// need a session or the password.
func CheckSignedInDirectly(s *model.Subject) error {
	if s == nil || !s.ByToken {
		return nil
	}
	return &Refusal{Code: CodeForbidden, Message: "this change needs a session or the password; a token does not issue or revoke tokens " +
		"or change the account"}
}

// TokenLimit is the refusal of a named token when the user holds the most
// named tokens of the kind already, with 409.
func TokenLimit(kind string, most int) error {
	return &Refusal{Code: CodeTokenLimit, Message: fmt.Sprintf(
		"this user holds %d named %s tokens, the most a user holds; revoke one first", most, kind)}
}

// PasswordWrong is the refusal of a current password that does not
// verify, with 400 at currentPassword.
func PasswordWrong() error {
	return &Refusal{Code: CodePasswordWrong, Field: "currentPassword", Message: "the current password is wrong"}
}
