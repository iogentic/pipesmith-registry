package testkit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestZipFieldsRefuseValuesWithoutRoom(t *testing.T) {
	if got := u32(5); got != 5 {
		t.Fatalf("u32(5) = %d", got)
	}
	if got := u16(65535); got != 65535 {
		t.Fatalf("u16(65535) = %d", got)
	}
	for name, call := range map[string]func(){
		"u32 above 32 bits": func() { u32(1 << 32) },
		"u32 negative":      func() { u32(-1) },
		"u16 above 16 bits": func() { u16(1 << 16) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			call()
		}()
	}
}

// Files and directories of the helpers give no access to other users.
func TestWrittenTreeHasNarrowModes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tree")
	if _, err := MakeTree(root, map[string][]byte{"demo-raw/a/b.txt": []byte("demo")}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{root, filepath.Join(root, "demo-raw", "a"), filepath.Join(root, "demo-raw", "a", "b.txt")} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&0o027 != 0 {
			t.Errorf("%s mode %o, want no group write and no access for others", p, perm)
		}
	}
}
