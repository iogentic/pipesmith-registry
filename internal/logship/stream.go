package logship

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"time"
)

// pastDeadline is a deadline that has passed: set on a connection, it ends
// the write that waits.
var pastDeadline = time.Unix(1, 0)

// link is the connection of a target that does not speak HTTP: syslog over
// udp, tcp or tls, gelf over udp or tcp. It stays open between batches; a
// failure closes it, and the next write dials again. Over tcp and tls a
// watcher reads the connection, as a receiver of these kinds sends no data:
// when the receiver ends the connection, the watcher closes it, and the next
// write dials again instead of writing into a connection that is gone.
type link struct {
	network string      // udp or tcp
	address string      // host:port
	tls     *tls.Config // set for the scheme tls

	conn    net.Conn
	gone    chan struct{} // closed by the watcher once the connection ended
	watched chan struct{} // closed once the watcher returned
}

// newLink is the link of the address of t. Over tls it trusts the roots of
// ca_file only, or else the roots of the system, needs TLS 1.2 at least and
// checks the host name of the address against the certificate.
func newLink(t Target) (*link, error) {
	u := t.endpoint()
	l := &link{network: u.Scheme, address: u.Host}
	if u.Scheme == "tls" {
		conf := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname()}
		if t.CAFile != "" {
			roots, err := rootsOf(t.CAFile)
			if err != nil {
				return nil, err
			}
			conf.RootCAs = roots
		}
		l.network, l.tls = "tcp", conf
	}
	return l, nil
}

// write sends data in one write: one datagram over udp, the next bytes of
// the stream over tcp and tls. It dials first when no connection is open.
// The deadline of ctx bounds the dial and the write, and the end of ctx ends
// a write that waits. A failed write closes the connection.
func (l *link) write(ctx context.Context, data []byte) error {
	if l.gone != nil {
		select {
		case <-l.gone:
			l.drop()
		default:
		}
	}
	if l.conn == nil {
		if err := l.dial(ctx); err != nil {
			return err
		}
	}
	conn := l.conn
	deadline, _ := ctx.Deadline() // the zero time, no deadline, for a ctx without one
	_ = conn.SetWriteDeadline(deadline)
	ended := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(ended)
		_ = conn.SetWriteDeadline(pastDeadline)
	})
	_, err := conn.Write(data)
	if !stop() {
		<-ended
	}
	if err != nil {
		l.drop()
		return writeFailure(ctx, err)
	}
	return nil
}

// dial opens the connection, with the TLS handshake for tls, and starts the
// watcher of a tcp or tls connection.
func (l *link) dial(ctx context.Context) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, l.network, l.address)
	if err != nil {
		return dialFailure(err, "dial")
	}
	if l.tls != nil {
		tc := tls.Client(conn, l.tls)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return dialFailure(err, "tls")
		}
		conn = tc
	}
	l.conn = conn
	if l.network == "tcp" {
		l.gone, l.watched = make(chan struct{}), make(chan struct{})
		go watch(conn, l.gone, l.watched)
	}
	return nil
}

// watch reads what the receiver sends until the connection ends, then
// closes gone and the connection.
func watch(conn net.Conn, gone, done chan struct{}) {
	defer close(done)
	_, _ = io.Copy(io.Discard, conn)
	close(gone)
	_ = conn.Close()
}

// close closes the connection, over tls with close_notify (RFC 5425, section
// 4.4), and waits for its watcher.
func (l *link) close() {
	if l.conn == nil {
		return
	}
	_ = l.conn.Close()
	l.forget()
}

// drop closes a connection that failed or ended. Over tls it sends no
// close_notify: a write that failed leaves the TLS state broken.
func (l *link) drop() {
	if l.conn == nil {
		return
	}
	if tc, ok := l.conn.(*tls.Conn); ok {
		_ = tc.NetConn().Close()
	} else {
		_ = l.conn.Close()
	}
	l.forget()
}

// forget waits for the watcher of the closed connection.
func (l *link) forget() {
	if l.watched != nil {
		<-l.watched
	}
	l.conn, l.gone, l.watched = nil, nil, nil
}

// dialFailure is the failure of a dial or of a TLS handshake: stopped or
// timeout when ctx ended, else class.
func dialFailure(err error, class string) *Failure {
	f := transportFailure(err)
	if f.Class != "stopped" && f.Class != "timeout" {
		f.Class = class
	}
	return f
}

// writeFailure is the failure of a write: stopped when the sender stopped,
// timeout when the deadline passed, else write.
func writeFailure(ctx context.Context, err error) *Failure {
	var ne net.Error
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return &Failure{Class: "stopped"}
	case ctx.Err() != nil || errors.As(err, &ne) && ne.Timeout():
		return &Failure{Class: "timeout"}
	}
	return &Failure{Class: "write"}
}
