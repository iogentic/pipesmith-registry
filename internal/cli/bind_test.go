package cli

import (
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/state"
)

func TestServeNetworkBindNeedsUsersAndHostNames(t *testing.T) {
	dir := dataDir(t)
	// without state.sqlite a network bind keeps the refusal of the reference;
	// port 0 makes a missing check fail fast instead of serving
	code, _, errOut := run(t, "serve", "--port", "0", "--bind", "0.0.0.0", "--server-names", "registry.test")
	if code != 1 || errOut != "error: bind address '0.0.0.0' is not local; only 127.0.0.1, localhost, ::1 are allowed until users are imported (import), then with --server-names NAME[,NAME]\n" {
		t.Fatalf("%d %q", code, errOut)
	}
	st, err := state.Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	for _, c := range []struct {
		args []string
		text string
	}{
		{[]string{"serve", "--port", "0", "--bind", "0.0.0.0"},
			"error: bind address '0.0.0.0' is not local; name this server with --server-names NAME[,NAME]\n"},
		{[]string{"serve", "--port", "0", "--server-names", "bad name"}, "error: host name 'bad name' is not a DNS name or an IP address\n"},
		{[]string{"serve", "--port", "0", "--server-names", "registry.test:8081"}, "error: host name 'registry.test:8081' is not a DNS name or an IP address\n"},
		{[]string{"serve", "--port", "0", "--server-names", "a.test,,b.test"}, "error: host name '' is not a DNS name or an IP address\n"},
		{[]string{"serve", "--port", "0", "--server-names", ""}, "error: host name '' is not a DNS name or an IP address\n"},
	} {
		code, out, errOut := run(t, c.args...)
		if code != 1 || out != "" || errOut != c.text {
			t.Errorf("%v: %d %q %q", c.args, code, out, errOut)
		}
	}
}

func TestHostNames(t *testing.T) {
	got, err := parseHostNames("Registry.Test, 10.0.0.5,::1,[fd00::5]")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"registry.test", "10.0.0.5", "[::1]", "[fd00::5]"}
	if len(got) != len(want) {
		t.Fatalf("%q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%q, want %q", got, want)
		}
	}
}
