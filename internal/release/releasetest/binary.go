package releasetest

import (
	"bytes"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// answerMark ends a copy of a test executable that answers `version` and
// `schema`: the bytes after it up to the final newline are the version it
// names, then a space and its schema line, or no space when it does not know
// the command schema.
const answerMark = "\x00psr-test-answer="

// DefaultSchema is the schema line of Answering.
const DefaultSchema = `{"catalog":2,"state":1}`

// AnswerVersion makes a copy of the running test executable that carries an
// answer mark behave as a release binary: called first in TestMain, it
// prints "pipesmith-registry <version>" for the argument version and the
// schema line for the argument schema, and exits. Without a schema line it
// answers schema as an older release does, with a usage error and exit
// code 2.
func AnswerVersion() {
	if len(os.Args) != 2 || (os.Args[1] != "version" && os.Args[1] != "schema") {
		return
	}
	v, ok := ownAnswer()
	if !ok {
		return
	}
	version, schema, known := strings.Cut(v, " ")
	switch {
	case os.Args[1] == "version":
		fmt.Printf("pipesmith-registry %s\n", version)
	case known:
		fmt.Println(schema)
	default:
		fmt.Fprintln(os.Stderr, "error: pipesmith-registry: argument command: invalid choice: 'schema'")
		os.Exit(2)
	}
	os.Exit(0)
}

func ownAnswer() (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(exe) // #nosec G304 -- the running test executable.
	if err != nil || len(data) < 128 || data[len(data)-1] != '\n' {
		return "", false
	}
	tail := string(data[len(data)-128:])
	i := strings.LastIndex(tail, answerMark)
	if i < 0 {
		return "", false
	}
	return strings.TrimSuffix(tail[i+len(answerMark):], "\n"), true
}

var (
	exeOnce  sync.Once
	exeBytes []byte
	exeErr   error
)

// Executable is the bytes of the running test executable.
func Executable(t testing.TB) []byte {
	t.Helper()
	exeOnce.Do(func() {
		var exe string
		if exe, exeErr = os.Executable(); exeErr == nil {
			exeBytes, exeErr = os.ReadFile(exe) // #nosec G304 -- the running test executable.
		}
	})
	if exeErr != nil {
		t.Fatal(exeErr)
	}
	return exeBytes
}

// Answering is the test executable with the answer mark of v: a binary
// with the build information of the test executable that answers v and
// DefaultSchema.
func Answering(t testing.TB, v string) []byte { return AnsweringSchema(t, v, DefaultSchema) }

// AnsweringSchema answers v and the schema line schema; an empty schema
// answers the command schema with a usage error.
func AnsweringSchema(t testing.TB, v, schema string) []byte {
	t.Helper()
	exe := Executable(t)
	mark := answerMark + v
	if schema != "" {
		mark += " " + schema
	}
	out := make([]byte, 0, len(exe)+len(mark)+1)
	return append(append(out, exe...), mark+"\n"...)
}

// Component is a component of a CycloneDX bill of materials.
type Component struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Purl    string `json:"purl,omitempty"`
}

// BillOf names every dependency module of bin, a replaced module by its
// replacement, and one component that is no Go module; the main module is
// left out, as in a bill of Syft.
func BillOf(t testing.TB, bin []byte) []Component {
	t.Helper()
	info, err := buildinfo.Read(bytes.NewReader(bin))
	if err != nil {
		t.Fatal(err)
	}
	comps := []Component{{Type: "library", Name: "pipesmith-registry-proof", Version: "0.1.0", Purl: "pkg:pypi/pipesmith-registry-proof@0.1.0"}}
	for _, d := range info.Deps {
		m := d
		if d.Replace != nil {
			m = d.Replace
		}
		comps = append(comps, Component{Type: "library", Name: m.Path, Version: m.Version, Purl: "pkg:golang/" + m.Path + "@" + m.Version})
	}
	return comps
}

// BillJSON is a CycloneDX document with comps.
func BillJSON(t testing.TB, comps []Component) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"bomFormat": "CycloneDX", "specVersion": "1.7", "version": 1, "components": comps})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
