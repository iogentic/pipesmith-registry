package auth

import (
	"crypto/md5" // #nosec G501 -- the unsalted MD5 form of the old product, read only to check a password.
	"crypto/pbkdf2"
	"crypto/sha1" // #nosec G505 -- the unsalted SHA-1 form and the PBKDF2 SHA-1 form of the old product.
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"hash"
	"strconv"
	"strings"
	"sync/atomic"
)

// Limits of a stored hash and a password (decision N22).
const (
	MaxPasswordBytes = 1024
	MaxIterations    = 10_000_000
)

// hashRuns counts the digests VerifyPassword computed. Tests read it to see
// that a refusal ran a hash or ran none.
var hashRuns atomic.Int64

// shiroDigests are the digest names a Shiro 1 hash may carry.
var shiroDigests = map[string]func() hash.Hash{
	"SHA-512": sha512.New,
	"SHA-384": sha512.New384,
	"SHA-256": sha256.New,
	"SHA-1":   sha1.New,
	"MD5":     md5.New,
}

// pbkdf2Digests are the PRFs of the PBKDF2 form.
var pbkdf2Digests = map[string]func() hash.Hash{
	"PBKDF2WithHmacSHA1":   sha1.New,
	"PBKDF2WithHmacSHA256": sha256.New,
}

// VerifyPassword reports whether password matches the stored form, one of
// the forms of the old product: Shiro 1 modular crypt, PBKDF2 in the PHC
// form, or unsalted hex SHA-1 or MD5. The digests are compared in constant
// time. An unknown form, a password longer than MaxPasswordBytes and a
// stored form with more than MaxIterations rounds give false without a hash.
func VerifyPassword(password, stored string) bool {
	if len(password) > MaxPasswordBytes {
		return false
	}
	switch {
	case strings.HasPrefix(stored, "$shiro1$"):
		return verifyShiro1(password, stored)
	case strings.HasPrefix(stored, "$PBKDF2"):
		return verifyPBKDF2(password, stored)
	}
	return verifyHex(password, stored)
}

// verifyShiro1 checks $shiro1$<digest>$<rounds>$<salt>$<hash>: a digest of
// the salt and the UTF-8 password, then rounds minus one digests of the
// previous digest alone (Shiro 1.13.0 SimpleHash).
func verifyShiro1(password, stored string) bool {
	fields := strings.Split(stored, "$")
	if len(fields) != 6 {
		return false
	}
	newHash, ok := shiroDigests[fields[2]]
	if !ok {
		return false
	}
	rounds, ok := iterations(fields[3])
	salt, err1 := base64.StdEncoding.DecodeString(fields[4])
	want, err2 := base64.StdEncoding.DecodeString(fields[5])
	if !ok || err1 != nil || err2 != nil {
		return false
	}
	h := newHash()
	if len(want) != h.Size() {
		return false
	}
	hashRuns.Add(1)
	h.Write(salt)
	h.Write([]byte(password))
	sum := h.Sum(nil)
	for range rounds - 1 {
		h.Reset()
		h.Write(sum)
		sum = h.Sum(sum[:0])
	}
	return subtle.ConstantTimeCompare(sum, want) == 1
}

// verifyPBKDF2 checks $<algorithm>$key_iteration=<n>,key_len=<bits>$<salt>$<hash>
// with the rounds and length the stored form names.
func verifyPBKDF2(password, stored string) bool {
	fields := strings.Split(stored, "$")
	if len(fields) != 5 || fields[0] != "" {
		return false
	}
	prf, ok := pbkdf2Digests[fields[1]]
	if !ok {
		return false
	}
	attributes := map[string]string{}
	for _, kv := range strings.Split(fields[2], ",") {
		key, value, found := strings.Cut(kv, "=")
		if !found {
			return false
		}
		attributes[key] = value
	}
	rounds, ok := iterations(attributes["key_iteration"])
	bits, err := strconv.Atoi(attributes["key_len"])
	if !ok || err != nil || bits <= 0 || bits%8 != 0 {
		return false
	}
	salt, err1 := base64.StdEncoding.DecodeString(fields[3])
	want, err2 := base64.StdEncoding.DecodeString(fields[4])
	if err1 != nil || err2 != nil || len(want) != bits/8 {
		return false
	}
	hashRuns.Add(1)
	got, err := pbkdf2.Key(prf, password, salt, rounds, bits/8)
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// verifyHex checks an unsalted hex digest of the UTF-8 password: 40 digits
// are SHA-1, 32 are MD5.
func verifyHex(password, stored string) bool {
	want, err := hex.DecodeString(stored)
	if err != nil {
		return false
	}
	var sum []byte
	switch len(want) {
	case sha1.Size:
		hashRuns.Add(1)
		s := sha1.Sum([]byte(password)) // #nosec G401 -- the stored form of the old product.
		sum = s[:]
	case md5.Size:
		hashRuns.Add(1)
		s := md5.Sum([]byte(password)) // #nosec G401 -- the stored form of the old product.
		sum = s[:]
	default:
		return false
	}
	return subtle.ConstantTimeCompare(sum, want) == 1
}

// iterations reads a round count from 1 to MaxIterations.
func iterations(text string) (int, bool) {
	n, err := strconv.Atoi(text)
	if err != nil || n < 1 || n > MaxIterations {
		return 0, false
	}
	return n, true
}
