package admin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorePathVectors(t *testing.T) {
	var file struct {
		Family string `json:"family"`
		Note   string `json:"note"`
		Setup  struct {
			Dirs  []string          `json:"dirs"`
			Files []string          `json:"files"`
			Links map[string]string `json:"links"`
		} `json:"setup"`
		Taken []string `json:"taken"`
		Cases []struct {
			Row      string         `json:"row"`
			Op       string         `json:"op"` // path, delete
			Name     string         `json:"name"`
			Path     *string        `json:"path"`
			Want     string         `json:"want"`
			Attached bool           `json:"attached"`
			Names    []string       `json:"names"`
			Refusals *[]wantRefusal `json:"refusals"`
		} `json:"cases"`
	}
	readJSON(t, "storepaths", &file)
	counts := tally{}
	for i, c := range file.Cases {
		label := fmt.Sprintf("case %d (%s %s %s)", i, c.Row, c.Op, c.Name)
		var err error
		switch c.Op {
		case "path":
			data := t.TempDir()
			for _, d := range file.Setup.Dirs {
				if err := os.MkdirAll(filepath.Join(data, d), 0o750); err != nil {
					t.Fatal(err)
				}
			}
			for _, f := range file.Setup.Files {
				if err := os.WriteFile(filepath.Join(data, f), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for link, target := range file.Setup.Links {
				if err := os.Symlink(target, filepath.Join(data, link)); err != nil {
					t.Fatal(err)
				}
			}
			var taken []string
			for _, p := range file.Taken {
				taken = append(taken, filepath.Join(data, p))
			}
			var sent *string
			if c.Path != nil {
				p := strings.ReplaceAll(*c.Path, "{data}", data)
				sent = &p
			}
			var got string
			got, err = StorePath(data, c.Name, sent, taken)
			if err == nil {
				real, _ := filepath.EvalSymlinks(data)
				if want := filepath.Join(real, c.Want); got != want {
					t.Errorf("%s: the path is %q, want %q", label, got, want)
				}
			}
		case "delete":
			err = CheckStoreProtected(c.Name, c.Attached)
		case "name":
			err = CheckCreate(KindBlobStore, c.Name, Taken(c.Names, c.Name))
		default:
			t.Fatalf("%s: unknown op", label)
		}
		checkRefusals(t, label, err, c.Refusals)
		if c.Refusals != nil {
			counts.add(c.Row, len(*c.Refusals) > 0)
		}
	}
	counts.check(t, "storepaths", "ST1", "ST2", "ST14")
}
