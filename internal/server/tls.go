package server

import (
	"crypto/tls"
	"fmt"
	"net/http"
)

// LoadTLS reads the certificate chain and the private key of the TLS
// listeners, PEM files read once at start (decision E9 of the execution
// plan). Every listener speaks TLS 1.2 or newer and offers HTTP/1.1 only,
// because the gate in front of net/http reads HTTP/1.1.
func LoadTLS(certFile, keyFile string) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("TLS certificate %s with key %s: %w", certFile, keyFile, err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, NextProtos: []string{"http/1.1"}}, nil
}

// withTLS gives a request that came over a TLS listener of Serve its
// connection state. The gate sits between the TLS connection and net/http,
// so net/http does not see a *tls.Conn and leaves Request.TLS nil.
func withTLS(r *http.Request) *http.Request {
	if r.TLS != nil {
		return r
	}
	g, ok := r.Context().Value(gateKey{}).(*gateConn)
	if !ok {
		return r
	}
	conn, ok := g.Conn.(*tls.Conn)
	if !ok {
		return r
	}
	state := conn.ConnectionState()
	r = r.WithContext(r.Context()) // a shallow copy, so the request of net/http stays as it is
	r.TLS = &state
	return r
}

// baseScheme is the scheme of a request before X-Forwarded-Proto, as
// uvicorn sets it for its TLS listener.
func baseScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}
