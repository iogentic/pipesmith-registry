package export

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

type nameVectors struct {
	ArchiveNames []struct{ Repository, Folder, Want string } `json:"archive_names"`
	ImageNames   []struct{ Image, Reference, Want string }   `json:"image_names"`
	Entries      []struct {
		Name string
		OK   bool
	} `json:"entries"`
	InvalidUTF8 []string `json:"invalid_utf8_entries"`
	NpmEntries  []struct {
		Path string
		Want *string
	} `json:"npm_entries"`
	Stored []struct {
		Name   string
		Stored bool
	} `json:"stored"`
	Limits []struct {
		MaxFiles int64   `json:"max_files"`
		MaxBytes int64   `json:"max_bytes"`
		Sizes    []int64 `json:"sizes"`
		Error    *struct {
			Files, Bytes int64
			Complete     bool
		} `json:"error"`
	} `json:"limits"`
}

func loadNameVectors(t *testing.T) *nameVectors {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/export/names.json")
	if err != nil {
		t.Fatal(err)
	}
	var v nameVectors
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return &v
}

func TestArchiveNames(t *testing.T) {
	v := loadNameVectors(t)
	if len(v.ArchiveNames) == 0 || len(v.ImageNames) == 0 {
		t.Fatal("no vectors")
	}
	for _, c := range v.ArchiveNames {
		if got := ArchiveName(c.Repository, c.Folder); got != c.Want {
			t.Errorf("ArchiveName(%q, %q) = %q, want %q", c.Repository, c.Folder, got, c.Want)
		}
	}
	for _, c := range v.ImageNames {
		if got := ImageArchiveName(c.Image, c.Reference); got != c.Want {
			t.Errorf("ImageArchiveName(%q, %q) = %q, want %q", c.Image, c.Reference, got, c.Want)
		}
	}
}

func TestEntryNames(t *testing.T) {
	v := loadNameVectors(t)
	accepted, refused := 0, 0
	for _, c := range v.Entries {
		err := CheckEntryName(c.Name)
		if c.OK {
			accepted++
			if err != nil {
				t.Errorf("CheckEntryName(%q) = %v, want nil", c.Name, err)
			}
			continue
		}
		refused++
		var bad *BadNameError
		if !errors.As(err, &bad) || bad.Name != c.Name {
			t.Errorf("CheckEntryName(%q) = %v, want a BadNameError", c.Name, err)
		}
	}
	for _, h := range v.InvalidUTF8 {
		raw, err := hex.DecodeString(h)
		if err != nil {
			t.Fatal(err)
		}
		refused++
		if CheckEntryName(string(raw)) == nil {
			t.Errorf("CheckEntryName(%x) accepts invalid UTF-8", raw)
		}
	}
	if accepted < 10 || refused < 10 {
		t.Fatalf("%d accepted and %d refused rows", accepted, refused)
	}
}

func TestNpmEntryNames(t *testing.T) {
	v := loadNameVectors(t)
	tarballs, others := 0, 0
	for _, c := range v.NpmEntries {
		if c.Want == nil {
			others++
		} else {
			tarballs++
		}
		got, ok := NpmEntryName(c.Path)
		switch {
		case c.Want == nil && ok:
			t.Errorf("NpmEntryName(%q) = %q, want no tarball", c.Path, got)
		case c.Want != nil && (!ok || got != *c.Want):
			t.Errorf("NpmEntryName(%q) = %q %v, want %q", c.Path, got, ok, *c.Want)
		}
	}
	if tarballs == 0 || others == 0 {
		t.Fatalf("%d tarball and %d other rows", tarballs, others)
	}
}

// UniqueNames finds the first name an unpack cannot hold beside the others:
// a name that repeats, or a file whose name is a folder of another entry.
func TestUniqueNames(t *testing.T) {
	refused := map[string][]string{
		"a":       {"a", "a/b"},
		"a/b":     {"a/b/c", "x/a/b", "a/b"},
		"same":    {"same", "other", "same"},
		"p/q":     {"p/q", "p/q/r/s"},
		"scope/x": {"scope/x/1.tgz", "scope/x"},
	}
	for want, names := range refused {
		if got, ok := UniqueNames(names); ok || got != want {
			t.Errorf("UniqueNames(%q) = %q %v, want %q", names, got, ok, want)
		}
	}
	accepted := [][]string{{"a", "ab", "a-b/c", "a.b"}, {"widget/widget-1.2.3.tgz", "widget/widget-1.3.0.tgz", "@demo/util/util-2.0.0.tgz"},
		{"x/y", "x/z"}, {"only"}}
	for _, names := range accepted {
		if got, ok := UniqueNames(names); !ok {
			t.Errorf("UniqueNames(%q) refuses %q", names, got)
		}
	}
}

func TestStoredWithoutCompression(t *testing.T) {
	v := loadNameVectors(t)
	stored, compressed := 0, 0
	for _, c := range v.Stored {
		if c.Stored {
			stored++
		} else {
			compressed++
		}
		if got := Stored(c.Name); got != c.Stored {
			t.Errorf("Stored(%q) = %v, want %v", c.Name, got, c.Stored)
		}
	}
	if stored == 0 || compressed == 0 {
		t.Fatalf("%d stored and %d compressed rows", stored, compressed)
	}
}

func TestLimits(t *testing.T) {
	v := loadNameVectors(t)
	passed, over := 0, 0
	for i, c := range v.Limits {
		tally := NewTally(Limits{MaxFiles: c.MaxFiles, MaxBytes: c.MaxBytes})
		for _, size := range c.Sizes {
			if !tally.Add(size) {
				break
			}
		}
		err := tally.Err()
		if c.Error == nil {
			passed++
			if err != nil {
				t.Errorf("row %d: %v, want nil", i, err)
			}
			continue
		}
		over++
		var limit *LimitError
		if !errors.As(err, &limit) {
			t.Errorf("row %d: %v, want a LimitError", i, err)
			continue
		}
		want := LimitError{Files: c.Error.Files, Bytes: c.Error.Bytes, MaxFiles: c.MaxFiles, MaxBytes: c.MaxBytes, Complete: c.Error.Complete}
		if *limit != want {
			t.Errorf("row %d: %+v, want %+v", i, *limit, want)
		}
	}
	if passed == 0 || over == 0 {
		t.Fatalf("%d rows within and %d above the limits", passed, over)
	}
}

// TestLimitAtTheBound is the bound itself: the limit passes, one file or one
// byte above fails.
func TestLimitAtTheBound(t *testing.T) {
	limits := Limits{MaxFiles: 20_000, MaxBytes: 2 << 30}
	at := NewTally(limits)
	for i := 0; i < 19_999; i++ {
		at.Add(0)
	}
	at.Add(2 << 30)
	if err := at.Err(); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	files := NewTally(limits)
	for i := 0; i < 20_001; i++ {
		files.Add(1)
	}
	if files.Err() == nil {
		t.Fatal("one file above the limit passed")
	}
	bytes := NewTally(limits)
	bytes.Add(2<<30 + 1)
	if bytes.Err() == nil {
		t.Fatal("one byte above the limit passed")
	}
	if d := DefaultLimits(); d != limits {
		t.Fatalf("DefaultLimits() = %+v", d)
	}
}
