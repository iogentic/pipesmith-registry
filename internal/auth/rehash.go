package auth

import (
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
)

// The salted default form of the old product: Shiro 1 with SHA-512, 1024
// rounds and 16 bytes of salt (lab research 05-identity.md, section Shiro 1
// form). New passwords and the rehash of an unsalted form use it.
const (
	DefaultRounds = 1024
	SaltBytes     = 16
)

// HashPassword is the salted default form of a password, with a new salt
// from crypto/rand. A password that VerifyPassword would refuse, empty or
// longer than MaxPasswordBytes, gets no form.
func HashPassword(password string) (string, error) {
	if password == "" || len(password) > MaxPasswordBytes {
		return "", errors.New("auth: a password has 1 to " + strconv.Itoa(MaxPasswordBytes) + " bytes")
	}
	salt := make([]byte, SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	h := sha512.New()
	h.Write(salt)
	h.Write([]byte(password))
	sum := h.Sum(nil)
	for range DefaultRounds - 1 {
		h.Reset()
		h.Write(sum)
		sum = h.Sum(sum[:0])
	}
	return "$shiro1$SHA-512$" + strconv.Itoa(DefaultRounds) + "$" + base64.StdEncoding.EncodeToString(salt) + "$" +
		base64.StdEncoding.EncodeToString(sum), nil
}

// unsalted reports a stored form of unsalted hex SHA-1 or MD5, the forms the
// rehash replaces (lab research 05-identity.md, section Rehash on login).
func unsalted(stored string) bool {
	raw, err := hex.DecodeString(stored)
	return err == nil && (len(raw) == 20 || len(raw) == 16)
}

// rehash stores the salted default form of a password after a good sign in,
// when its stored form is unsalted (task 3 of the phase 3 plan). The row is
// replaced only while it still holds that form, so a password another
// process set meanwhile stays; the write raises the generation, and the
// next reload brings the new form. Each view tries a user once. A failure
// leaves the sign in as it is and says so in the log. It returns the form
// stored, "" for none: a sign in of the same view that comes while the first
// one stores it waits for that and gets the same form, which a session or a
// Docker token is bound to as well (review 3-34).
func (s *Service) rehash(v *view, id, stored, password string) string {
	if s.store == nil || !unsalted(stored) {
		return ""
	}
	mine := &rehashed{done: make(chan struct{})}
	if got, tried := v.rehashed.LoadOrStore(id, mine); tried {
		first := got.(*rehashed)
		<-first.done
		return first.form
	}
	defer close(mine.done)
	// a view loaded before the form of another view was stored holds the
	// unsalted form still: it takes the form of that try
	key := id + "\x00" + stored
	if got, tried := s.rehashes.LoadOrStore(key, mine); tried {
		first := got.(*rehashed)
		<-first.done
		mine.form = first.form
		return mine.form
	}
	defer func() {
		if mine.form == "" {
			s.rehashes.Delete(key)
		}
	}()
	form, err := HashPassword(password)
	changed := false
	if err == nil {
		changed, err = s.store.SetPasswordHash(id, stored, form)
	}
	switch {
	case err != nil:
		s.log.Warn("the salted form of a password was not stored", "user", id, "error", err.Error())
	case changed:
		s.log.Info("the password of a user is stored in the salted form now", "user", id)
		mine.form = form
	}
	return mine.form
}

// rehashed is the try of a view to store the form of a user again: done
// closes when it ended, and form is the form it stored, "" for none.
// Service.rehashes keeps the tries that stored a form, by user id and the
// unsalted form they replaced.
type rehashed struct {
	done chan struct{}
	form string
}
