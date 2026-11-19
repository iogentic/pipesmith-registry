package channel

import (
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
)

// ErrBill refuses a bill of materials that does not name the modules of
// the binary (U3).
var ErrBill = errors.New("bill of materials")

// billComponent is a component of a CycloneDX bill, as Syft writes it.
type billComponent struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Purl    string `json:"purl"`
}

// billModules is the set of name and version of the Go module components:
// those whose purl starts with pkg:golang/.
type billModules map[[2]string]bool

func modulesOf(comps []billComponent) billModules {
	out := billModules{}
	for _, c := range comps {
		if strings.HasPrefix(c.Purl, "pkg:golang/") {
			out[[2]string{c.Name, c.Version}] = true
		}
	}
	return out
}

// checkBill checks that every module debug/buildinfo reads from the binary
// at bin is a Go module component of bill with the same name and version,
// and returns how many modules it checked.
func checkBill(bin string, bill []byte) (int, error) {
	var doc struct {
		Components []billComponent `json:"components"`
	}
	if err := json.Unmarshal(bill, &doc); err != nil {
		return 0, fmt.Errorf("%w: %s is no CycloneDX object: %v", ErrBill, BillFile, err)
	}
	info, err := buildinfo.ReadFile(bin)
	if err != nil {
		return 0, fmt.Errorf("%w: no build information in the binary: %v", ErrBill, err)
	}
	return billCovers(info, modulesOf(doc.Components))
}

// billCovers applies the rule to build information: each of its Deps, a
// replaced module by its replacement, the main module left out.
func billCovers(info *debug.BuildInfo, mods billModules) (int, error) {
	n := 0
	for _, d := range info.Deps {
		m := d
		if d.Replace != nil {
			m = d.Replace
		}
		if !mods[[2]string{m.Path, m.Version}] {
			return n, fmt.Errorf("%w: the binary holds the module %s %s, which %s does not name as a Go module component with that version", ErrBill, m.Path, m.Version, BillFile)
		}
		n++
	}
	return n, nil
}
