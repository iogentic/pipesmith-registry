package rest

import (
	"errors"
	"slices"

	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The own account of the interface (leg 6-api, contract S41, ruling AP7).

// accountBody is the body of PUT /api/account; a key the body lacks keeps
// the stored value.
type accountBody struct {
	FirstName    *string `json:"firstName"`
	LastName     *string `json:"lastName"`
	EmailAddress *string `json:"emailAddress"`
}

// SaveAccount is PUT /api/account: the names and the mail of the own user,
// 204 (ruling AP7). A user of another source than default answers 409
// (Q6); the roles and the status stay. A subject that signed in with a
// token answers 403.
func SaveAccount(w IdentityWrite, body []byte) (Answer, error) {
	if err := admin.CheckSignedInDirectly(w.Subject); err != nil {
		return refused(err)
	}
	me := model.User{ID: w.Subject.User.ID, Source: sourceOf(w.Subject.User.Source)}
	if err := admin.CheckAccountSave(me); err != nil {
		return refused(err)
	}
	var b accountBody
	if err := decode(body, '{', &b); err != nil {
		return refused(err)
	}
	_, err := admin.Change(w.Store, []state.Kind{state.KindUsers}, func(s *model.Snapshot) error {
		i := slices.IndexFunc(s.Users, func(u model.User) bool { return u.ID == me.ID && sourceOf(u.Source) == localSource })
		if i < 0 {
			return notFound("user " + me.ID)
		}
		stored := s.Users[i]
		sent := stored
		sent.ID = ""
		for _, f := range []struct {
			from *string
			to   *string
		}{{b.FirstName, &sent.FirstName}, {b.LastName, &sent.LastName}, {b.EmailAddress, &sent.Email}} {
			if f.from != nil {
				*f.to = *f.from
			}
		}
		saved, err := admin.SaveUser(s, stored, sent, true, false)
		if err != nil {
			return err
		}
		s.Users[i] = saved
		return nil
	})
	if err != nil {
		return refused(err)
	}
	return noContent()
}

// passwordBody is the body of POST /api/account/password.
type passwordBody struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// ChangeOwnPassword is POST /api/account/password: 204 once the new
// password is stored (ruling AP7). A subject that signed in with a token
// answers 403. A user of another source than default answers 409 (Q6), a
// new password the rules refuse 400 at newPassword (I5). change verifies the current password as a sign in does and stores
// the form of the new one in its place (auth.Service.ChangeOwnPassword);
// auth.ErrWrongPassword is a 400 at currentPassword. Another error of
// change, the 429 of the sign in limits among them, is returned for the
// server to answer.
func ChangeOwnPassword(w IdentityWrite, change func(current, hash string) error, body []byte) (Answer, error) {
	if err := admin.CheckSignedInDirectly(w.Subject); err != nil {
		return refused(err)
	}
	me := model.User{ID: w.Subject.User.ID, Source: sourceOf(w.Subject.User.Source)}
	if err := admin.CheckPasswordChange(me); err != nil {
		return refused(err)
	}
	var b passwordBody
	if err := decode(body, '{', &b); err != nil {
		return refused(err)
	}
	if err := admin.CheckPassword(b.NewPassword); err != nil {
		var r *admin.Refusal
		if errors.As(err, &r) {
			moved := *r
			moved.Field = "newPassword"
			err = &moved
		}
		return refused(err)
	}
	hash, err := auth.HashPassword(b.NewPassword)
	if err != nil {
		return Answer{}, err
	}
	switch err := change(b.CurrentPassword, hash); {
	case errors.Is(err, auth.ErrWrongPassword):
		return refused(admin.PasswordWrong())
	case err != nil:
		return Answer{}, err
	}
	return noContent()
}
