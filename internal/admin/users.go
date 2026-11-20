package admin

import (
	"fmt"
	"slices"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// userStatuses are the statuses of a user of the contract.
var userStatuses = []string{statusActive, "locked", "disabled", "changepassword"}

// CheckUserDelete checks the delete of a stored user (ruling Q5). It is
// refused with 409, each case with a code of its own: a user whose source
// is not the local one, default, is a directory user (user-directory); the
// own user, whose id and source equal those of the acting subject
// (user-own); and the user that the anonymous access setting of the
// snapshot names, whether the setting is on or off (user-anonymous). The
// refusals come in this order. actor is nil when no subject acts. The rule
// of the last administrator stays CheckAdministrators.
func CheckUserDelete(s *model.Snapshot, actor *model.Subject, user model.User) error {
	var list Refusals
	if r := directoryUser(user, "is kept in its directory and cannot be deleted here"); r != nil {
		list = append(list, r)
	}
	if actor != nil && actor.User.ID == user.ID && actor.User.Source == user.Source {
		list = append(list, &Refusal{Code: CodeUserOwn, Message: fmt.Sprintf(
			"the user %s is the signed in user and cannot delete itself", user.ID)})
	}
	if s.Anonymous.UserID == user.ID {
		list = append(list, &Refusal{Code: CodeUserAnonymous, Message: fmt.Sprintf(
			"the user %s is the user that anonymous access names and cannot be deleted", user.ID)})
	}
	return list.err()
}

// CheckAccountSave refuses a change of the own names and mail of a user
// whose source is not the local one with 409 (ruling Q6).
func CheckAccountSave(user model.User) error {
	return directoryError(directoryUser(user, "keeps its names and mail in its directory"))
}

// CheckPasswordChange refuses a change of the password of a user whose
// source is not the local one with 409: the own password, with the user of
// the session (ruling Q6), and the password an administrator sets for
// another user, with the target user (ruling Q18, contract S24).
func CheckPasswordChange(user model.User) error {
	return directoryError(directoryUser(user, "keeps its password in its directory"))
}

// directoryUser is the refusal user-directory of a user whose source is not
// the local one, with the words of the change; nil for a local user.
func directoryUser(user model.User, words string) *Refusal {
	if user.Source == sourceLocal {
		return nil
	}
	return &Refusal{Code: CodeUserDirectory, Message: fmt.Sprintf("the user %s of the source %q %s", user.ID, user.Source, words)}
}

// directoryError is the refusal as an error, nil when there is none.
func directoryError(r *Refusal) error {
	if r == nil {
		return nil
	}
	return r
}

// CheckUserStatus checks the status of a user create or save (ruling Q7):
// active, locked, disabled or changepassword, compared as it is, else 400
// at status. stored is the stored user on a save and nil on a create; a
// status the stored user holds is never refused (ruling R6).
func CheckUserStatus(status string, stored *model.User) error {
	if slices.Contains(userStatuses, status) || stored != nil && status == stored.Status {
		return nil
	}
	return &Refusal{Code: CodeUserStatus, Field: "status", Message: fmt.Sprintf(
		"the status %q is not known; use active, locked, disabled or changepassword", status)}
}
