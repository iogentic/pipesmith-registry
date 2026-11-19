// Command forgestub serves a directory of release folders in the URL layout
// of the generic package registry of a Forgejo instance, on loopback only.
// It is a test tool and is never shipped.
//
// usage: forgestub -dir DIR -owner OWNER [-port PORT] [-token-file FILE]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/iogentic/pipesmith-registry/tools/forgestub/forge"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, nil); err != nil {
		fmt.Fprintf(os.Stderr, "forgestub: %v\n", err)
		os.Exit(2)
	}
}

// run serves until ctx ends. ready, when set, receives the address.
func run(ctx context.Context, args []string, out io.Writer, ready chan<- string) error {
	fs := flag.NewFlagSet("forgestub", flag.ContinueOnError)
	fs.SetOutput(out)
	dir := fs.String("dir", "", "directory of release folders <dir>/<version>/<file>")
	owner := fs.String("owner", "", "owner of the package")
	port := fs.Int("port", 0, "port on 127.0.0.1; 0 takes a free one")
	tokenFile := fs.String("token-file", "", "file whose first line is the token every request must carry")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" || *owner == "" || fs.NArg() != 0 {
		return errors.New("usage: forgestub -dir DIR -owner OWNER [-port PORT] [-token-file FILE]")
	}
	if *port < 0 || *port > 65535 {
		return fmt.Errorf("port %d out of range", *port)
	}
	if info, err := os.Stat(*dir); err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not a directory", *dir)
	}
	token := ""
	if *tokenFile != "" {
		data, err := os.ReadFile(*tokenFile)
		if err != nil {
			return err
		}
		token, _, _ = strings.Cut(string(data), "\n")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: forge.New(forge.Options{Dir: *dir, Owner: *owner, Token: token}),
		ReadHeaderTimeout: 10 * time.Second}
	addr := "http://" + ln.Addr().String()
	fmt.Fprintf(out, "forgestub: serving %s for owner %s at %s\n", *dir, *owner, addr)
	if ready != nil {
		ready <- addr
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}
