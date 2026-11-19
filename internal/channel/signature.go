package channel

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/iogentic/pipesmith-registry/internal/release"
)

// verifySignature verifies the bundle over sums with the trusted key, as
// release.VerifyBundle does. When the signature does not verify and the key
// hint of the bundle names another key than the trusted one, the message
// says so. The hint is the base64 of the SHA-256 of the DER form of the
// public key, as cosign 3.1.4 writes it (seen in the vectors of
// tests/release/release_proof.py); it only words the message.
func verifySignature(trustedKey string, sums, bundle []byte) error {
	key, err := release.LoadTrustedKey(trustedKey)
	if err != nil {
		return err
	}
	err = release.VerifyBundle(key, sums, bundle)
	if err != release.ErrSignature { // nil, or a refusal that names its cause already
		return err
	}
	der, merr := x509.MarshalPKIXPublicKey(key)
	if merr != nil {
		return err
	}
	sum := sha256.Sum256(der)
	trusted := base64.StdEncoding.EncodeToString(sum[:])
	var doc struct {
		VerificationMaterial struct {
			PublicKey struct {
				Hint string `json:"hint"`
			} `json:"publicKey"`
		} `json:"verificationMaterial"`
	}
	if json.Unmarshal(bundle, &doc) != nil {
		return err
	}
	switch hint := doc.VerificationMaterial.PublicKey.Hint; {
	case hint == trusted:
		return fmt.Errorf("%w; the bundle names the trusted key, so its signature is not the one that key made", err)
	case hint != "" && plainHint.MatchString(hint):
		return fmt.Errorf("%w; the bundle was signed with another key (key hint %s, the trusted key has %s)", err, hint, trusted)
	case hint != "":
		return fmt.Errorf("%w; the bundle names a key hint that is not shown, as it is no short base64 text; the trusted key has %s", err, trusted)
	}
	return err
}

// plainHint is a key hint that may be shown: at most 64 characters of the
// base64 alphabet. The forge writes the hint, so nothing else reaches a
// terminal.
var plainHint = regexp.MustCompile(`^[A-Za-z0-9+/=]{1,64}$`)
