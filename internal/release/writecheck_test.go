package release

import (
	"errors"
	"testing"
)

// The write check of the data directory makes a hard link and a rename as
// well, the two steps the way back needs there, and leaves nothing behind
// when one fails (finding A-16).
func TestWriteCheckLinksAndRenames(t *testing.T) {
	migrates := answering("0.2.0", `{"catalog":2,"state":2}`)
	for point, words := range map[string]string{"write-check-link": "hard link", "write-check-rename": "rename"} {
		t.Run(point, func(t *testing.T) {
			r := newRig(t, "0.1.0")
			r.dataFiles(1, 1)
			seen := false
			r.u.hook = func(p string) error {
				if p == point {
					seen = true
					return errors.New("not supported")
				}
				return nil
			}
			r.refused(ErrSpace, func() error { return r.u.Update(r.release(migrates), false) }, "cannot write", words, "not supported")
			if !seen {
				t.Fatalf("no point %s", point)
			}
		})
	}
}
