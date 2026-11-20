package auth_test

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// changed gives the stored form with one byte of its digest changed.
func changed(t *testing.T, stored string) string {
	t.Helper()
	if !strings.HasPrefix(stored, "$") {
		digest, err := hex.DecodeString(stored)
		if err != nil {
			t.Fatal(err)
		}
		digest[len(digest)/2] ^= 0x01
		return hex.EncodeToString(digest)
	}
	cut := strings.LastIndex(stored, "$")
	digest, err := base64.StdEncoding.DecodeString(stored[cut+1:])
	if err != nil {
		t.Fatal(err)
	}
	digest[len(digest)/2] ^= 0x01
	return stored[:cut+1] + base64.StdEncoding.EncodeToString(digest)
}

func TestPasswordVectors(t *testing.T) {
	vf := loadVectors(t)
	if len(vf.Verify) != 21 || len(vf.Refuse) != 6 {
		t.Fatalf("vector file holds %d and %d vectors", len(vf.Verify), len(vf.Refuse))
	}
	forms := map[string]int{}
	for _, v := range vf.Verify {
		switch {
		case strings.HasPrefix(v.Hash, "$shiro1$"):
			forms["shiro1"]++
		case strings.HasPrefix(v.Hash, "$PBKDF2"):
			forms["pbkdf2"]++
		default:
			forms["hex"]++
		}
		if !auth.VerifyPassword(v.Password, v.Hash) {
			t.Errorf("%s (%s) does not verify", v.Name, v.Source)
		}
		if auth.VerifyPassword(v.Password, changed(t, v.Hash)) {
			t.Errorf("%s verifies with one byte of the digest changed", v.Name)
		}
		if auth.VerifyPassword(v.Password+"x", v.Hash) {
			t.Errorf("%s verifies with a longer password", v.Name)
		}
		if auth.VerifyPassword(strings.ToUpper(v.Password), v.Hash) && strings.ToUpper(v.Password) != v.Password {
			t.Errorf("%s verifies with the password in upper case", v.Name)
		}
	}
	if forms["shiro1"] < 4 || forms["pbkdf2"] < 2 || forms["hex"] < 2 {
		t.Errorf("forms covered: %v", forms)
	}
}

func TestPasswordRefusals(t *testing.T) {
	// the positive side: a password at the limit verifies and counts a digest
	limit := vectorNamed(t, "shiro1-1024-bytes")
	before := auth.HashRuns()
	if len(limit.Password) != auth.MaxPasswordBytes || !auth.VerifyPassword(limit.Password, limit.Hash) ||
		auth.HashRuns() != before+1 {
		t.Fatalf("a password of %d bytes: verify or digest count wrong", len(limit.Password))
	}
	for _, v := range loadVectors(t).Refuse {
		before := auth.HashRuns()
		if auth.VerifyPassword(v.Password, v.Hash) {
			t.Errorf("%s verifies (%s)", v.Name, v.Why)
		}
		if strings.Contains(v.Why, "without hashing") && auth.HashRuns() != before {
			t.Errorf("%s ran %d digests (%s)", v.Name, auth.HashRuns()-before, v.Why)
		}
	}
}

// Stored forms that are no hash of the old product are refused without a
// digest: dispatch is by prefix, else a hex string of 32 or 40 digits.
func TestPasswordDispatch(t *testing.T) {
	good := vectorNamed(t, "shiro1-sha512").Hash
	salt := strings.Split(good, "$")[4]
	pbkdf := vectorNamed(t, "pbkdf2-sha256").Hash
	pbkdfParts := strings.Split(pbkdf, "$")
	refused := []string{
		"",
		"$shiro2$argon2id$v=19$t=1,m=65536,p=4$c2FsdA$aGFzaA",
		"$shiro1$SHA-512$1024$" + salt,
		"$shiro1$SHA-3$1024$" + salt + "$" + salt,
		"$shiro1$SHA-512$0$" + salt + "$" + salt,
		"$shiro1$SHA-512$-1$" + salt + "$" + salt,
		"$shiro1$SHA-512$x$" + salt + "$" + salt,
		"$shiro1$SHA-512$1024$!!!$" + salt,
		"$shiro1$SHA-512$1024$" + salt + "$" + salt,
		"$PBKDF2WithHmacSHA512$key_iteration=1024,key_len=128$" + pbkdfParts[3] + "$" + pbkdfParts[4],
		"$PBKDF2WithHmacSHA256$key_iteration=10000,key_len=100$" + pbkdfParts[3] + "$" + pbkdfParts[4],
		"$PBKDF2WithHmacSHA256$key_iteration=10000,key_len=128$" + pbkdfParts[3] + "$" + pbkdfParts[4],
		"$PBKDF2WithHmacSHA256$key_iteration=0,key_len=256$" + pbkdfParts[3] + "$" + pbkdfParts[4],
		"$PBKDF2WithHmacSHA256$key_len=256$" + pbkdfParts[3] + "$" + pbkdfParts[4],
		"$PBKDF2WithHmacSHA256$key_iteration=10000,key_len=256$" + pbkdfParts[3],
		strings.Repeat("a", 39),
		strings.Repeat("z", 40),
		strings.Repeat("a", 41),
		strings.Repeat("0", 64),
	}
	for _, stored := range refused {
		before := auth.HashRuns()
		if auth.VerifyPassword("demo-admin-pass-1", stored) {
			t.Errorf("%q verifies", stored)
		}
		// a digest of the wrong length is refused before the hash as well
		if auth.HashRuns() != before {
			t.Errorf("%q ran a digest", stored)
		}
	}
	// the hex form ignores letter case
	sha1 := vectorNamed(t, "legacy-sha1")
	if !auth.VerifyPassword(sha1.Password, strings.ToUpper(sha1.Hash)) {
		t.Error("an upper case hex digest does not verify")
	}
}

// The PBKDF2 form names its key length, and each block of the key costs the
// full round count, so the length has a bound as the rounds have one
// (decision N22): MaxKeyBits. A longer key is refused before any digest.
func TestPasswordKeyLength(t *testing.T) {
	if auth.MaxKeyBits != 512 {
		t.Errorf("MaxKeyBits %d, want 512", auth.MaxKeyBits)
	}
	// at the bound: written by Python hashlib.pbkdf2_hmac, sha256, 1000
	// rounds, 64 bytes, the salt from the name of the vector
	const atBound = "$PBKDF2WithHmacSHA256$key_iteration=1000,key_len=512$1WdVcGdDOSmddAvRrEwP+w==$" +
		"L0HTKEfFUocn9lr84IUCTDTIJsFHAcLcY7x0gooFFhIL/x9RTAhJAIyfIrTKKVDTlGrCPk6bC0MYWjm8xGBfBg=="
	before := auth.HashRuns()
	if !auth.VerifyPassword("demo-keylen-pass-1", atBound) || auth.HashRuns() != before+1 {
		t.Fatal("a key of 512 bits does not verify with one digest")
	}
	salt := strings.Split(atBound, "$")[3]
	for _, bits := range []int{520, 1024, 8 << 20} {
		digest := base64.StdEncoding.EncodeToString(make([]byte, bits/8))
		stored := fmt.Sprintf("$PBKDF2WithHmacSHA1$key_iteration=1,key_len=%d$%s$%s", bits, salt, digest)
		before := auth.HashRuns()
		if auth.VerifyPassword("demo-keylen-pass-1", stored) || auth.HashRuns() != before {
			t.Errorf("a key of %d bits: verified or ran %d digests", bits, auth.HashRuns()-before)
		}
	}
}

func FuzzVerifyPassword(f *testing.F) {
	f.Add("admin123", "$shiro1$SHA-512$1024$zjU1u+Zg9UNwuB+HEawvtA==$IzF/OWzJxrqvB5FCe/2+UcZhhZYM2pTu0TEz7Ybnk65AbbEdUk9ntdtBzkN8P3gZby2qz6MHKqAe8Cjai9c4Gg==")
	f.Add("admin123", "$PBKDF2WithHmacSHA1$key_iteration=1,key_len=8$AA==$AA==")
	f.Add("admin123", "f865b53623b121fd34ee5426c792e5c33af8c227")
	f.Add("x", "$shiro1$MD5$1$$")
	f.Add("x", "$PBKDF2WithHmacSHA1$$$$")
	f.Fuzz(func(t *testing.T, password, stored string) {
		auth.VerifyPassword(password, stored)
	})
}

// Review 3-63, point c: the stored form sets the cost of a check, so the
// digests one check runs have an upper bound, auth.MaxHashWork: the rounds of
// a Shiro 1 form, the rounds times the blocks of a PBKDF2 key. The bound is
// 200 times the most the old product writes by default (PBKDF2 SHA-256,
// 10000 rounds, one block) and above the rounds OWASP names for PBKDF2. A
// form above it is refused without a digest, and the load names its user.
func TestPasswordFormCostBound(t *testing.T) {
	if auth.MaxHashWork != 2_000_000 {
		t.Fatalf("MaxHashWork %d, want 2000000", auth.MaxHashWork)
	}
	salt := base64.StdEncoding.EncodeToString(make([]byte, 16))
	sha512 := base64.StdEncoding.EncodeToString(make([]byte, 64))
	key := func(bits int) string { return base64.StdEncoding.EncodeToString(make([]byte, bits/8)) }
	over := []string{
		fmt.Sprintf("$shiro1$SHA-512$%d$%s$%s", auth.MaxHashWork+1, salt, sha512),
		fmt.Sprintf("$PBKDF2WithHmacSHA256$key_iteration=%d,key_len=256$%s$%s", auth.MaxHashWork+1, salt, key(256)),
		// 4 blocks of 160 bits for a key of 512 bits
		fmt.Sprintf("$PBKDF2WithHmacSHA1$key_iteration=%d,key_len=512$%s$%s", auth.MaxHashWork/4+1, salt, key(512)),
		fmt.Sprintf("$PBKDF2WithHmacSHA256$key_iteration=%d,key_len=512$%s$%s", auth.MaxHashWork/2+1, salt, key(512)),
	}
	for _, stored := range over {
		before := auth.HashRuns()
		if auth.VerifyPassword("demo-cost-pass-1", stored) || auth.HashRuns() != before {
			t.Errorf("%.60s: verified or ran %d digests", stored, auth.HashRuns()-before)
		}
	}
	snap := fixture(t)
	snap.Users = append(snap.Users, model.User{ID: "demo-costly", Status: "active", Source: auth.SourceLocal, PasswordHash: over[0]})
	e := newEnv(t, snap)
	if logs := e.logs.String(); !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "demo-costly") ||
		!strings.Contains(logs, "costs more than") {
		t.Fatalf("no warning about the costly form: %q", logs)
	}
	if strings.Count(e.logs.String(), "costs more than") != 1 {
		t.Fatalf("a warning for a form within the bound: %q", e.logs.String())
	}
}
