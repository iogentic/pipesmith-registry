package channel

import (
	"debug/buildinfo"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/release"
)

// billComponents is the module set of the components a test writes.
func billComponents(comps []component) billModules {
	var parsed []billComponent
	for _, c := range comps {
		parsed = append(parsed, billComponent{Name: c.Name, Version: c.Version, Purl: c.Purl})
	}
	return modulesOf(parsed)
}

// TestTestExecutableHasModules pins what the tests of this file rest on:
// debug/buildinfo reads the dependency modules of the test executable, and
// the blank import of helpers_test.go puts github.com/google/uuid among them.
func TestTestExecutableHasModules(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	info, err := buildinfo.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range info.Deps {
		found = found || d.Path == "github.com/google/uuid" && d.Version != ""
	}
	if !found || info.Main.Path == "" {
		t.Fatalf("main %+v, deps %d", info.Main, len(info.Deps))
	}
}

// stubFile is the path of a file of a published version in the stub.
func (r *rig) stubFile(version, file string) string {
	return filepath.Join(r.stubDir, version, file)
}

func TestChecksumChangedByte(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	path := r.stubFile("0.2.0", platformBinary())
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0x01
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	r.start()
	r.refused(release.ErrDigest)
}

func TestPlatformWithoutLine(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0", sumsEdit: func(sums string) string {
		var keep []string
		for _, line := range strings.SplitAfter(sums, "\n") {
			if !strings.HasSuffix(line, "  "+platformBinary()+"\n") {
				keep = append(keep, line)
			}
		}
		return strings.Join(keep, "")
	}})
	r.start()
	err := r.refused(release.ErrNotListed)
	if !strings.Contains(err.Error(), platformBinary()) {
		t.Fatalf("message %v", err)
	}
	for _, path := range r.requested() {
		if strings.HasSuffix(path, "/"+platformBinary()) {
			t.Fatal("a binary without a line was downloaded")
		}
	}
}

func TestBillOtherDigest(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	if err := os.WriteFile(r.stubFile("0.2.0", BillFile), []byte(`{"components": []}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r.start()
	err := r.refused(ErrBill)
	if !strings.Contains(err.Error(), "SHA-256 of "+BillFile) {
		t.Fatalf("message %v", err)
	}
}

func TestBillWithoutLine(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0", sumsEdit: func(sums string) string {
		var keep []string
		for _, line := range strings.SplitAfter(sums, "\n") {
			if !strings.HasSuffix(line, "  "+BillFile+"\n") {
				keep = append(keep, line)
			}
		}
		return strings.Join(keep, "")
	}})
	r.start()
	err := r.refused(ErrBill)
	if !strings.Contains(err.Error(), "no line") {
		t.Fatalf("message %v", err)
	}
	for _, path := range r.requested() {
		if strings.HasSuffix(path, "/"+BillFile) {
			t.Fatal("a bill without a line was downloaded")
		}
	}
}

// billWith is the bill of the test executable with each component passed
// through change; a nil result drops it.
func billWith(t *testing.T, change func(component) *component) []component {
	t.Helper()
	var out []component
	for _, c := range billOf(t, binaryAnswering(t, "0.2.0")) {
		if n := change(c); n != nil {
			out = append(out, *n)
		}
	}
	return out
}

func TestBillRefusals(t *testing.T) {
	for name, change := range map[string]func(component) *component{
		"module the bill does not name": func(c component) *component {
			if c.Name == "github.com/google/uuid" {
				return nil
			}
			return &c
		},
		"module with another version": func(c component) *component {
			if c.Name == "github.com/google/uuid" {
				c.Version = "v1.5.0"
				c.Purl = "pkg:golang/github.com/google/uuid@v1.5.0"
			}
			return &c
		},
		"module named by a component that is no Go module": func(c component) *component {
			if c.Name == "github.com/google/uuid" {
				c.Purl = "pkg:pypi/uuid@" + c.Version
			}
			return &c
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.publish(published{version: "0.2.0", bill: billWith(t, change)})
			r.start()
			err := r.refused(ErrBill)
			if !strings.Contains(err.Error(), "github.com/google/uuid") {
				t.Fatalf("message %v", err)
			}
		})
	}
}

func TestBillNoBuildInformation(t *testing.T) {
	r := newRig(t)
	bin := []byte("#!/bin/sh\necho pipesmith-registry 0.2.0\n")
	r.publish(published{version: "0.2.0", bin: bin, bill: []component{}})
	r.start()
	err := r.refused(ErrBill)
	if !strings.Contains(err.Error(), "build information") {
		t.Fatalf("message %v", err)
	}
}

func TestBillMalformed(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0", edit: func(files map[string][]byte) { files[BillFile] = []byte("[1, 2]") }})
	r.start()
	r.refused(ErrBill)
}

func TestBillRule(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Path: "example.com/main", Version: "(devel)"},
		Deps: []*debug.Module{
			{Path: "example.com/a", Version: "v1.0.0"},
			{Path: "example.com/b", Version: "v2.0.0", Replace: &debug.Module{Path: "example.com/fork", Version: "v2.0.1"}},
		},
	}
	good := []component{
		{Name: "example.com/a", Version: "v1.0.0", Purl: "pkg:golang/example.com/a@v1.0.0"},
		{Name: "example.com/fork", Version: "v2.0.1", Purl: "pkg:golang/example.com/fork@v2.0.1"},
	}
	if n, err := billCovers(info, billComponents(good)); err != nil || n != 2 {
		t.Fatalf("good bill: %d %v", n, err)
	}
	for name, comps := range map[string][]component{
		"the replaced module instead of its replacement": {good[0], {Name: "example.com/b", Version: "v2.0.0", Purl: "pkg:golang/example.com/b@v2.0.0"}},
		"another name in the purl only":                  {good[0], {Name: "example.com/forked", Version: "v2.0.1", Purl: "pkg:golang/example.com/fork@v2.0.1"}},
	} {
		if _, err := billCovers(info, billComponents(comps)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	local := &debug.BuildInfo{Deps: []*debug.Module{{Path: "example.com/a", Version: "v1.0.0", Replace: &debug.Module{Path: "../a"}}}}
	if _, err := billCovers(local, billComponents(good)); err == nil {
		t.Error("a module replaced by a directory passed")
	}
}

// TestBillOfARealBuild runs the bill rule on the three binaries of a
// release that tools/release.sh built, against its sbom.cdx.json.
func TestBillOfARealBuild(t *testing.T) {
	dir := os.Getenv("PSR_RELEASE_DIR")
	if dir == "" {
		t.Skip("PSR_RELEASE_DIR is not set; it names a release folder of tools/release.sh such as work/release-proof/a/0.1.0-dev")
	}
	bill, err := os.ReadFile(filepath.Join(dir, BillFile))
	if err != nil {
		t.Skipf("no %s in %s: %v", BillFile, dir, err)
	}
	for _, platform := range [][2]string{{"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}} {
		name := release.BinaryName(platform[0], platform[1])
		n, err := checkBill(filepath.Join(dir, name), bill)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if n < 5 {
			t.Errorf("%s: %d modules", name, n)
		}
		t.Logf("%s: %d modules named in the bill", name, n)
	}
}
