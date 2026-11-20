package cli

import (
	"io"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/listeners"
	"github.com/iogentic/pipesmith-registry/internal/proxy"
	"github.com/iogentic/pipesmith-registry/internal/pystr"
)

// reposRemoteSecret is repos remote-secret <name> (decisions 5.12 and 5.13
// of the phase 5 plan): the secret half of the sign in of a proxy at its
// remote, the password of Basic or the bearer token, read from standard
// input and never from an argument, into secrets/remote/<name> of the data
// directory with mode 0600. Type and user name are attributes of the
// repository. Nothing of the input is printed. A running server takes a
// changed file at the next request of the proxy to its remote.
func (c *command) reposRemoteSecret() (int, error) {
	cfg, err := load()
	if err != nil {
		return 1, err
	}
	repositories, err := listeners.LoadRepositories(cfg, openIndexReader)
	if err != nil {
		return 1, err
	}
	name, _ := c.text("name")
	if repo, ok := repositories.Get(name); !ok || repo.Type != "proxy" {
		return 1, refuse("%s is no proxy repository; the secret of a remote belongs to a proxy, see: repos", pystr.Repr(name))
	}
	secret, err := c.remoteSecret(name)
	if err != nil {
		return 1, err
	}
	path, err := proxy.WriteSecret(cfg.DataDir, name, secret)
	if err != nil {
		return 1, err
	}
	c.print("wrote the secret of the remote of %s to %s; the proxy takes it at its next request to the remote", name, path)
	return 0, nil
}

// remoteSecret reads the secret from standard input: one line at a
// terminal, with echo off and the prompt on standard error, else the whole
// input with one line break at its end taken off.
func (c *command) remoteSecret(name string) (string, error) {
	var secret string
	if isTerminal(stdin) {
		line, err := askSecret(stdin, c.err)("secret of the remote of " + name + ": ")
		if err != nil {
			return "", err
		}
		secret = line
	} else {
		data, err := io.ReadAll(io.LimitReader(stdin, proxy.MaxSecretBytes+1))
		if err != nil {
			return "", err
		}
		if len(data) > proxy.MaxSecretBytes {
			return "", refuse("the secret on standard input has more than %d bytes", proxy.MaxSecretBytes)
		}
		secret = strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	}
	if secret == "" {
		return "", refuse("no secret on standard input; give it as its only line, for example: repos remote-secret %s < <file>", name)
	}
	return secret, nil
}
