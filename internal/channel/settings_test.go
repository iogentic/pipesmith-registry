package channel

import (
	"errors"
	"strings"
	"testing"
)

// No refusal of channel.server repeats user information, a password, a
// query or a fragment.
func TestServerRefusalsHideTheAddress(t *testing.T) {
	const secret, login = "canary-pw-51d0", "login-name-3e"
	for _, server := range []string{
		"https://" + login + ":" + secret + "@forge.example",
		"ftp://" + login + ":" + secret + "@forge.example",
		"https://" + login + ":" + secret + "@forge.example/api/v1",
		"https://forge.example/?token=" + secret,
		"https://forge.example/#" + secret,
		"https://" + login + ":" + secret + "@forge.example:99999",
		"https://" + login + ":" + secret + "@[::1",
		"https://" + login + ":" + secret + "@/path",
		"http://" + login + ":" + secret + "@forge.example",
		"https://" + login + ":" + secret + "@forge.example\x7f",
	} {
		r := newRig(t)
		r.publish(published{version: "0.2.0"})
		r.start()
		r.config(map[string]any{"trusted_key": r.trusted.Pub, "data_dir": r.data, "channel": map[string]any{"server": server, "owner": "acme"}})
		_, err := r.f.Fetch(Options{})
		if !errors.Is(err, ErrSettings) || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), login) {
			t.Errorf("%q: %v", server, err)
		}
		if len(r.stub.Requests()) != 0 {
			t.Errorf("%q: %d requests", server, len(r.stub.Requests()))
		}
	}
}
