// Command githubstub serves a directory of release folders in the URL
// layout of the releases API of GitHub, on loopback only. It is a test tool
// and is never shipped.
//
// usage: githubstub -dir DIR -repo OWNER/REPO [-port PORT] [-limit N]
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

	"github.com/iogentic/pipesmith-registry/tools/githubstub/gh"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, nil); err != nil {
		fmt.Fprintf(os.Stderr, "githubstub: %v\n", err)
		os.Exit(2)
	}
}

const usage = "usage: githubstub -dir DIR -repo OWNER/REPO [-port PORT] [-limit N]"

// run serves until ctx ends. ready, when set, receives the address.
func run(ctx context.Context, args []string, out io.Writer, ready chan<- string) error {
	fs := flag.NewFlagSet("githubstub", flag.ContinueOnError)
	fs.SetOutput(out)
	dir := fs.String("dir", "", "directory of release folders <dir>/<version>/<file>")
	repo := fs.String("repo", "", "owner/repo of the releases")
	port := fs.Int("port", 0, "port on 127.0.0.1; 0 takes a free one")
	limit := fs.Int("limit", 0, "requests before the stub answers 403 rate limit; 0 is no limit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	owner, name, ok := strings.Cut(*repo, "/")
	if *dir == "" || !ok || owner == "" || name == "" || strings.Contains(name, "/") || fs.NArg() != 0 {
		return errors.New(usage)
	}
	if *port < 0 || *port > 65535 || *limit < 0 {
		return errors.New(usage)
	}
	if info, err := os.Stat(*dir); err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not a directory", *dir)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: gh.New(gh.Options{Dir: *dir, Owner: owner, Repo: name, Limit: *limit}),
		ReadHeaderTimeout: 10 * time.Second}
	addr := "http://" + ln.Addr().String()
	fmt.Fprintf(out, "githubstub: serving %s as %s at %s\n", *dir, *repo, addr)
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
