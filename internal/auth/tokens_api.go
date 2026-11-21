package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The tokens of the interface (contract S29, rulings AP1 to AP4 of leg
// 6-api): a signed in subject issues an npm or user token for itself, lists
// the tokens of a user and revokes one by its id. The secret half is in
// the answer of the issue only; the list names a row by its public half
// and an imported key of the old product by an id made from its digest.
// The own password is checked as a sign in checks it (AP7).

// TokenEntry is a token of a user as the list shows it. Public is the
// public half, empty for an imported key; Name is empty for an npm login
// and for an imported key.
type TokenEntry struct {
	ID, Kind, Name, UserID, Source, Public string
	Created, LastUsed                      time.Time
	Imported                               bool
}

// TokenKindError is the refusal of a token kind the service does not issue
// on request: docker, a kind it does not know, and a kind whose realm is not
// in the stored order.
type TokenKindError struct{ Kind, Reason string }

func (e *TokenKindError) Error() string { return e.Reason }

// ErrWrongPassword is the refusal of a current password that does not
// verify.
var ErrWrongPassword = errors.New("the current password is wrong")

// importedRows is what the list reads of the imported keys; state.Store
// fulfils it.
type importedRows interface {
	ImportedKeyRows(userID, source string) ([]model.ImportedKey, error)
}

// importedPrefix starts the id of an imported key in the list.
const importedPrefix = "imported-"

// ImportedKeyID is the id of an imported key in the list: 8 bytes of a
// SHA-256 over its digest, in hex, so the answer holds no digest.
func ImportedKeyID(digest [32]byte) string {
	sum := sha256.Sum256(append([]byte("pipesmith-registry imported key id\x00"), digest[:]...))
	return importedPrefix + hex.EncodeToString(sum[:8])
}

// IssueToken issues a token of kind with name for the subject itself: the
// wire form, shown once, and the row. docker, an unknown kind and a kind
// whose realm is off are refused with a *TokenKindError; a name the user
// holds for the kind with model.ErrTokenName; a name beyond the
// MaxTokensPerKind named tokens of the kind with model.ErrTokenLimit. No
// named token is revoked by the bound.
func (s *Service) IssueToken(subject *model.Subject, kind, name string) (string, model.Token, error) {
	if subject == nil {
		return "", model.Token{}, model.ErrUnauthorized
	}
	if err := s.CheckTokenKind(kind); err != nil {
		return "", model.Token{}, err
	}
	if s.store == nil {
		return "", model.Token{}, errors.New("auth: tokens need the state file")
	}
	text, row, err := NewToken(kind, subject.User.ID, subject.User.Source, s.now())
	if err != nil {
		return "", model.Token{}, err
	}
	row.Name = name
	revoked, err := s.store.AddToken(row, MaxTokensPerKind)
	if err != nil {
		return "", model.Token{}, err
	}
	for _, public := range revoked {
		s.bindings.drop(public)
	}
	if subject.User.Source == SourceLDAP {
		s.bindings.holds(row.Public, subject.User.Entry)
	}
	return text, row, nil
}

// CheckTokenKind refuses with a *TokenKindError a kind that IssueToken
// does not issue: docker, a kind it does not know, and a kind whose realm
// is not in the stored order.
func (s *Service) CheckTokenKind(kind string) error {
	switch {
	case kind == KindDocker:
		return &TokenKindError{Kind: kind, Reason: "Docker bearer tokens come from the token route of a Docker repository " +
			"and live 300 seconds; this request issues npm and user tokens"}
	case kind != KindNpm && kind != KindUser:
		return &TokenKindError{Kind: kind, Reason: "the token kind " + quote(kind) + " is not known; use npm or user"}
	case !s.refresh().realms[tokenRealms[kind]]:
		return &TokenKindError{Kind: kind, Reason: kind + " tokens are off: the realm " + tokenRealms[kind] +
			" is not in the active realms"}
	}
	return nil
}

func quote(s string) string { return `"` + s + `"` }

// ListTokens are the tokens of a user of a source: the rows in the order
// of their issue, then the imported keys. No entry holds a secret half or
// a digest.
func (s *Service) ListTokens(userID, source string) ([]TokenEntry, error) {
	list := []TokenEntry{}
	if s.store == nil {
		return list, nil
	}
	rows, err := s.store.Tokens(userID, source)
	if err != nil {
		return nil, err
	}
	for _, t := range rows {
		list = append(list, TokenEntry{ID: t.Public, Kind: t.Kind, Name: t.Name, UserID: t.UserID, Source: t.Source,
			Public: t.Public, Created: t.Created, LastUsed: t.LastUsed})
	}
	keys, err := s.importedKeys(userID, source)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		list = append(list, TokenEntry{ID: ImportedKeyID(k.Digest), Kind: k.Kind, UserID: k.UserID, Source: k.Source,
			Created: k.Created, LastUsed: k.LastUsed, Imported: true})
	}
	return list, nil
}

func (s *Service) importedKeys(userID, source string) ([]model.ImportedKey, error) {
	lister, ok := s.imported.(importedRows)
	if !ok {
		return nil, nil
	}
	return lister.ImportedKeyRows(userID, source)
}

// TokenOwner is the user and source of the token or imported key of the
// id; false when there is none.
func (s *Service) TokenOwner(id string) (userID, source string, found bool, err error) {
	if s.store == nil {
		return "", "", false, nil
	}
	if k, ok, err := s.importedOf(id); err != nil || ok {
		return k.UserID, k.Source, ok, err
	}
	row, found, err := s.store.Token(id)
	if err != nil || !found {
		return "", "", false, err
	}
	return row.UserID, row.Source, true, nil
}

// importedOf is the imported key of an id of the list.
func (s *Service) importedOf(id string) (model.ImportedKey, bool, error) {
	if len(id) <= len(importedPrefix) || id[:len(importedPrefix)] != importedPrefix {
		return model.ImportedKey{}, false, nil
	}
	keys, err := s.importedKeys("", "")
	if err != nil {
		return model.ImportedKey{}, false, err
	}
	for _, k := range keys {
		if ImportedKeyID(k.Digest) == id {
			return k, true, nil
		}
	}
	return model.ImportedKey{}, false, nil
}

// RemoveTokenID revokes the token or imported key of the id; false when
// there was none. After a revoke the credential cache is empty and the view
// stale, as after ChangeOwnPassword, so a Basic check with the token that
// the cache holds signs in no more.
func (s *Service) RemoveTokenID(id string) (bool, error) {
	if s.store == nil {
		return false, nil
	}
	k, ok, err := s.importedOf(id)
	if err != nil {
		return false, err
	}
	var removed bool
	if ok {
		removed, err = s.imported.RemoveImportedKey(k.Digest)
	} else {
		removed, err = s.store.RemoveToken(id)
		s.bindings.drop(id)
	}
	if err != nil {
		return false, err
	}
	if removed {
		s.ForgetCredentials()
		s.MarkStale()
	}
	return removed, nil
}

// ChangeOwnPassword stores hash, the form of a new password, for the
// subject, a local user, once current verifies as a sign in of the
// interface checks it: under the limits of the pair, and a refusal takes
// RefusalFloor at least. The row is replaced only while it still holds the
// form current verified against (Store.SetPasswordHash); a row that another
// change replaced meanwhile refuses as a wrong password. After the change
// the credential cache is empty and the view stale, so the old password
// signs in no more and every session of the user ends at its next request.
// ErrWrongPassword when current does not verify, the 429 of the limits
// while they slow the pair down.
func (s *Service) ChangeOwnPassword(r *http.Request, subject *model.Subject, current, hash string) error {
	if subject == nil {
		return model.ErrUnauthorized
	}
	if src := subject.User.Source; src != "" && src != SourceLocal {
		return errors.New("auth: the password of a user of the source " + src + " is kept in its directory")
	}
	if s.store == nil {
		return errors.New("auth: the service has no store for a password change")
	}
	start := s.floorNow()
	v := s.refresh()
	form := ""
	_, ok, wait := s.tryPair(subject.User.ID, s.ClientAddress(r), func(user string) checked {
		got, ok, compared := s.checkLocal(v, user, current)
		if ok {
			form = s.formAfterCheck(v, user)
		}
		return checked{got: got, ok: ok, compared: compared}
	})
	switch {
	case wait > 0:
		return throttled(wait)
	case !ok:
		s.floor(start)
		return ErrWrongPassword
	}
	changed, err := s.store.SetPasswordHash(subject.User.ID, form, hash)
	if err != nil {
		return err
	}
	if !changed {
		s.floor(start)
		return ErrWrongPassword
	}
	s.ForgetCredentials()
	s.MarkStale()
	return nil
}

// formAfterCheck is the stored form of a local user that a good check of
// the view verified: the salted form the check stored in its place when it
// replaced an unsalted one (rehash.go), else the form of the view.
func (s *Service) formAfterCheck(v *view, user string) string {
	u, _ := v.localUser(user)
	if got, ok := s.rehashes.Load(u.ID + "\x00" + u.PasswordHash); ok {
		r := got.(*rehashed)
		<-r.done
		if r.form != "" {
			return r.form
		}
	}
	return u.PasswordHash
}
