package auth

import (
	"context"
	"crypto/sha256"
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// Keys of the old product that an import with secrets took over (decisions
// K4 to K8 of docs/plans/secrets-import.md, which reverse N9 for them
// only). A client sends such a key as the old product gave it out,
// <prefix>.<key>. The row holds the HMAC-SHA-256 under the install key of
// the SHA-256 of the key; a match signs its user in under the rules of a
// token of this server. A key that matches no row keeps the answer of N9.

// ImportedKeys is what the service reads and writes of the imported keys;
// package state implements it.
type ImportedKeys interface {
	ImportedKey(kind string, digest [32]byte) (model.ImportedKey, bool, error)
	RemoveImportedKey(digest [32]byte) (bool, error)
	TouchImportedKey(digest [32]byte, at time.Time) error
}

// importedKinds are the kinds of the prefixes of the old product that an
// imported key may carry; NuGet keys have no route (K7).
var importedKinds = map[string]string{tokenPrefixes[KindNpm]: KindNpm, tokenPrefixes[KindDocker]: KindDocker}

// loadInstallKey reads the install key; a failure leaves none, so no
// imported key signs in until the next reload reads it.
func (s *Service) loadInstallKey() {
	if s.installKeyOf == nil || s.imported == nil {
		return
	}
	key, err := s.installKeyOf()
	if err != nil {
		s.log.Warn("the install key of the imported keys was not read; no imported key signs in", "error", err.Error())
		key = nil
	}
	if len(key) == 0 {
		s.installKey.Store(nil)
		return
	}
	s.installKey.Store(&key)
}

// importedRow is the imported key of text in the old shape: its kind and
// the row of its digest.
func (s *Service) importedRow(text string) (string, model.ImportedKey, bool) {
	key := s.installKey.Load()
	prefix, rest, found := strings.Cut(text, ".")
	kind, known := importedKinds[prefix]
	if key == nil || !found || !known || rest == "" {
		return "", model.ImportedKey{}, false
	}
	row, ok, err := s.imported.ImportedKey(kind, model.ImportedDigest(*key, sha256.Sum256([]byte(rest))))
	if err != nil {
		s.log.Warn("an imported key could not be read from the state file", "error", err.Error())
		return "", model.ImportedKey{}, false
	}
	return kind, row, ok
}

// importedBearer resolves a bearer in the old shape. found false means no
// imported key matched, and the caller answers as N9 says.
func (s *Service) importedBearer(ctx context.Context, v *view, text string) (subject *model.Subject, found bool) {
	kind, row, ok := s.importedRow(text)
	if !ok {
		return nil, false
	}
	if !v.realms[tokenRealms[kind]] {
		return nil, true
	}
	p, ok := s.importedUser(ctx, v, row)
	if !ok {
		return nil, true
	}
	got, ok := v.subject(p)
	if !ok {
		return nil, true
	}
	s.touchImported(row)
	return &got, true
}

// importedUser is the user of an imported key, as tokenUser is for a token
// row; the key of a local user that is gone is deleted under the rule of
// review S1.
func (s *Service) importedUser(ctx context.Context, v *view, row model.ImportedKey) (principal, bool) {
	switch row.Source {
	case SourceLocal:
		if _, there := v.local[row.UserID]; !there {
			s.forgetOrphanKey(v, row)
			return principal{}, false
		}
		return principal{user: model.User{ID: row.UserID, Source: SourceLocal}, realm: RealmLocal}, true
	case SourceLDAP:
		return s.directoryUser(ctx, v, row.UserID)
	}
	return principal{}, false
}

func (s *Service) forgetOrphanKey(v *view, row model.ImportedKey) {
	if s.store == nil {
		return
	}
	if gen, err := s.store.Generation(); err != nil || gen != v.gen {
		return
	}
	if _, err := s.imported.RemoveImportedKey(row.Digest); err != nil {
		s.log.Warn("the imported key of a removed user was not deleted", "user", row.UserID, "error", err.Error())
		return
	}
	s.log.Info("imported key of a removed user deleted", "kind", row.Kind, "user", row.UserID)
}

// touchImported writes the last use of an imported key, at most once per
// TouchInterval.
func (s *Service) touchImported(row model.ImportedKey) {
	now := s.now()
	if !row.LastUsed.IsZero() && now.Sub(row.LastUsed) < TouchInterval {
		return
	}
	if err := s.imported.TouchImportedKey(row.Digest, now); err != nil {
		s.log.Warn("the last use of an imported key was not stored", "user", row.UserID, "error", err.Error())
	}
}

// revokeImported deletes an imported npm key that the subject holds (npm
// logout). false when the text names no such key.
func (s *Service) revokeImported(subject *model.Subject, text string) (bool, error) {
	kind, row, ok := s.importedRow(text)
	if !ok || kind != KindNpm || row.UserID != subject.User.ID || row.Source != subject.User.Source {
		return false, nil
	}
	return s.imported.RemoveImportedKey(row.Digest)
}
