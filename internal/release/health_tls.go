package release

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
)

// healthPin is the certificate the server presents, from the file a key
// names; the health wait over TLS trusts that certificate alone.
type healthPin struct {
	path string
	key  string // such as tls_cert of config.json
	der  []byte
}

// loadHealthPin reads the first CERTIFICATE block of the PEM file path.
func loadHealthPin(path, key string) (*healthPin, error) {
	refuse := func(reason string) error {
		return fmt.Errorf("%w: the certificate of the health wait %s (%s): %s", ErrConfig, path, key, reason)
	}
	f, err := os.Open(path) // #nosec G304 -- the certificate file the operator configured.
	if err != nil {
		return nil, refuse(err.Error())
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, refuse(err.Error())
	}
	if len(data) > maxFile {
		return nil, refuse(fmt.Sprintf("larger than %d bytes", maxFile))
	}
	for rest := data; ; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			return nil, refuse("no CERTIFICATE block")
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return nil, refuse(err.Error())
		}
		return &healthPin{path: path, key: key, der: block.Bytes}, nil
	}
}

// errOtherCert marks a server that presents another certificate than the pin.
var errOtherCert = errors.New("the server presents another certificate than")

// tlsConfig compares the first certificate the server presents with the pin,
// byte for byte; no host name and no chain is checked.
func (p *healthPin) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // #nosec G402 -- VerifyConnection compares the certificate with the configured one.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || !bytes.Equal(cs.PeerCertificates[0].Raw, p.der) {
				return fmt.Errorf("%w %s (%s)", errOtherCert, p.path, p.key)
			}
			return nil
		},
	}
}

// explain names a server that answered a request over TLS in plain HTTP.
func explain(err error) error {
	if errors.Is(err, http.ErrSchemeMismatch) {
		return fmt.Errorf("the server did not speak TLS: %w", err)
	}
	return err
}
