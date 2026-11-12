package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/mirosso25/pipesmith-registry/internal/config"
	"github.com/mirosso25/pipesmith-registry/internal/index"
	"github.com/mirosso25/pipesmith-registry/internal/listeners"
	"github.com/mirosso25/pipesmith-registry/internal/model"
	"github.com/mirosso25/pipesmith-registry/internal/notify"
	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
)

const (
	// shutdownGrace is the time open requests get once the server stops.
	shutdownGrace = 5 * time.Second
	// keepAlive is the keep alive timeout of the Python server: an idle
	// connection, and a body nobody reads, get this long.
	keepAlive = 5 * time.Second
	// maxHeaderBytes is the head limit of the Python server's parser.
	maxHeaderBytes = 16 * 1024
)

// ServeOptions configure Serve.
type ServeOptions struct {
	// AccessLog receives the access lines; nil writes none.
	AccessLog io.Writer
	// Ready, when set, is called once every listener is open, with their
	// addresses: the main port first, then the Docker ports in port order.
	Ready func(addrs []net.Addr)
	// Security is Options.Security: nil serves as in step 2.
	Security model.Security
	// HostNames is Options.HostNames.
	HostNames []string
	// Notify tells the service manager READY=1 once every listener is
	// open, WATCHDOG=1 at its interval and STOPPING=1 when the stop begins;
	// nil tells nothing.
	Notify *notify.Notifier
}

// errorLog sends what net/http logs to the log of the server.
type errorLog struct{}

func (errorLog) Write(p []byte) (int, error) {
	logAt(slogWarn, serverLogger, "%s", bytes.TrimRight(p, "\n"))
	return len(p), nil
}

// newHTTPServer is the http.Server of one listener: the head of a request
// within 10 s and at most 16 KiB, keep alive for 5 s, as the Python server.
// A connection behind the gate finds its gateConn in the request context.
func newHTTPServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:                      h,
		ReadHeaderTimeout:            10 * time.Second,
		IdleTimeout:                  keepAlive,
		MaxHeaderBytes:               maxHeaderBytes,
		DisableGeneralOptionsHandler: true,
		ErrorLog:                     log.New(errorLog{}, "", 0),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if g, ok := c.(*gateConn); ok {
				return context.WithValue(ctx, gateKey{}, g)
			}
			return ctx
		},
	}
}

// openConns are the connections of all listeners that are not closed yet.
type openConns struct {
	mu  sync.Mutex
	set map[net.Conn]struct{}
}

func (o *openConns) track(c net.Conn, state http.ConnState) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch state {
	case http.StateNew:
		if o.set == nil {
			o.set = map[net.Conn]struct{}{}
		}
		o.set[c] = struct{}{}
	case http.StateHijacked, http.StateClosed:
		delete(o.set, c)
	}
}

// expire ends every read now: a head or a body still on its way is given
// up, as uvicorn closes a connection without an answer in progress when it
// stops. Answers in progress go on writing.
func (o *openConns) expire() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for c := range o.set {
		c.SetReadDeadline(time.Now())
	}
}

// tell sends a state to the service manager. A state that does not get
// there is a warning and changes nothing else.
func tell(n *notify.Notifier, state string) {
	if err := n.Send(state); err != nil {
		logAt(slogWarn, listenersLogger, "could not send %s to the service manager: %v", state, err)
	}
}

// startWatchdog starts the watchdog the environment asks for and returns
// its stop.
func startWatchdog(n *notify.Notifier) (stop func()) {
	if _, err := n.WatchdogInterval(); err != nil {
		logAt(slogWarn, listenersLogger, "%v", err)
	}
	return n.StartWatchdog(func(err error) {
		logAt(slogWarn, listenersLogger, "could not send %s to the service manager: %v", notify.Watchdog, err)
	})
}

func openIndexReader(path string) (listeners.IndexReader, error) {
	return index.Open(path)
}

// Serve is listeners.serve: one listener on the main port and one per
// Docker repository with a port, all opened before anything is served and
// all or none. It answers until ctx ends, then gives open requests
// shutdownGrace and closes the rest. Every refusal comes before a bind.
func Serve(ctx context.Context, cfg *config.Config, opts ServeOptions) error {
	if listeners.IsReserved(cfg.Port) {
		return &pyjson.Error{Class: "ValueError", Message: fmt.Sprintf("port %d is reserved on this machine", cfg.Port)}
	}
	repositories, err := listeners.LoadRepositories(cfg, openIndexReader)
	if err != nil {
		return err
	}
	dockerPorts, err := listeners.DockerPortMap(cfg, repositories)
	if err != nil {
		return err
	}
	ports := append([]int{cfg.Port}, listeners.SortedPorts(dockerPorts)...)
	opened, err := listeners.Open(cfg.Bind, ports)
	if err != nil {
		return err
	}
	defer func() {
		for _, l := range opened {
			l.Close()
		}
	}()
	srv, err := New(cfg, dockerPorts, Options{AccessLog: opts.AccessLog, Security: opts.Security, HostNames: opts.HostNames})
	if err != nil {
		return err
	}
	defer srv.Close()
	if tcp, ok := opened[0].Addr().(*net.TCPAddr); ok {
		srv.port = tcp.Port // the configuration may ask for port 0
	}
	logAt(slogInfo, listenersLogger, "listening on %s port %d", cfg.Bind, cfg.Port)
	for _, port := range listeners.SortedPorts(dockerPorts) {
		logAt(slogInfo, listenersLogger, "Docker repository %s listening on %s port %d", dockerPorts[port], cfg.Bind, port)
	}
	servers := make([]*http.Server, len(opened))
	stopped := make(chan error, len(opened))
	addrs := make([]net.Addr, len(opened))
	conns := &openConns{}
	for i, l := range opened {
		servers[i] = newHTTPServer(srv)
		servers[i].ConnState = conns.track
		addrs[i] = l.Addr()
		go func(hs *http.Server, l net.Listener) {
			stopped <- hs.Serve(&gateListener{Listener: l, marker: srv.marker, escape: srv.escape})
		}(servers[i], l)
	}
	if opts.Ready != nil {
		opts.Ready(addrs)
	}
	tell(opts.Notify, notify.Ready)
	stopWatchdog := startWatchdog(opts.Notify)
	var failure error
	select {
	case <-ctx.Done():
	case err := <-stopped:
		if !errors.Is(err, http.ErrServerClosed) {
			failure = err
		}
	}
	stopWatchdog()
	tell(opts.Notify, notify.Stopping)
	for _, hs := range servers {
		hs.SetKeepAlivesEnabled(false)
	}
	conns.expire()
	shutdown, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	var wg sync.WaitGroup
	for _, hs := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if hs.Shutdown(shutdown) != nil {
				hs.Close()
			}
		}()
	}
	wg.Wait()
	return failure
}
