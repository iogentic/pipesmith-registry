package release

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// unitValues reads the values of key in the [Service] section of a unit
// file of release/, in their order.
func unitValues(t *testing.T, file, key string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "release", file))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var values []string
	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && section == "[Service]" && k == key {
			values = append(values, v)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return values
}

// The server unit gives a start UnitStartSeconds, for a start that copies
// and migrates large data files before it reports ready, and a restart of
// the update waits that long on top of the drain (finding A-7). The start
// time on a Linux host is not measured; the value is a bound chosen wide.
func TestUnitStartTimeout(t *testing.T) {
	if got := unitValues(t, "pipesmith-registry.service", "TimeoutStartSec"); len(got) != 1 || got[0] != "900" {
		t.Fatalf("TimeoutStartSec %q, want 900", got)
	}
	if UnitStartSeconds != 900 {
		t.Fatalf("UnitStartSeconds %d", UnitStartSeconds)
	}
	cfg := &UpdateConfig{DrainSeconds: 300, drainGiven: true}
	if got, want := cfg.restartTimeout(), (300+UnitStartSeconds)*time.Second; got < want {
		t.Fatalf("restart bound %v, below the drain and the start of the unit, %v", got, want)
	}
}

// The update unit runs as root with a read only file system except the
// install root and the data directory, which update writes, and without
// access to kernel settings, devices and other namespaces (finding A-18).
// It names no User=: with User=root and these settings systemd in the Ubuntu
// container of tests/update/run.sh fails the start with "Failed to keep
// CAP_SYS_ADMIN" (status 217/USER), without the line it runs as root. That
// systemd accepts the file and the update still runs is not checked here;
// tests/update/run.sh runs it in a container with systemd.
func TestUpdateUnitHardening(t *testing.T) {
	const unit = "pipesmith-registry-update.service"
	want := map[string]string{"ProtectSystem": "strict", "ReadWritePaths": "/opt/pipesmith-registry /var/lib/pipesmith-registry",
		"PrivateTmp": "yes", "NoNewPrivileges": "yes", "ProtectHome": "yes", "PrivateDevices": "yes", "ProtectKernelTunables": "yes",
		"ProtectKernelModules": "yes", "ProtectKernelLogs": "yes", "ProtectControlGroups": "yes", "ProtectClock": "yes",
		"ProtectHostname": "yes", "RestrictNamespaces": "yes", "RestrictRealtime": "yes", "RestrictSUIDSGID": "yes",
		"LockPersonality": "yes", "MemoryDenyWriteExecute": "yes", "SystemCallArchitectures": "native",
		"RestrictAddressFamilies": "AF_UNIX AF_INET AF_INET6", "UMask": "0022"}
	for key, value := range want {
		if got := unitValues(t, unit, key); len(got) != 1 || got[0] != value {
			t.Errorf("%s %q, want %q", key, got, value)
		}
	}
	if got := unitValues(t, unit, "User"); len(got) != 0 {
		t.Errorf("User %q, want none: the unit runs as root", got)
	}
	if got := unitValues(t, unit, "Environment"); len(got) != 1 || got[0] != "PIPESMITH_REGISTRY_DATA=/var/lib/pipesmith-registry" {
		t.Errorf("Environment %q", got)
	}
}
