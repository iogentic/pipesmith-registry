package blobwrite

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobprops"
)

// vectorFile is testdata/vectors/blobwrite.json.
type vectorFile struct {
	Properties []struct {
		Name  string            `json:"name"`
		Items map[string]string `json:"items"`
		Lines []string          `json:"lines"`
	} `json:"properties"`
}

func loadVectors(t *testing.T) *vectorFile {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/blobwrite.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Properties) != 6 {
		t.Fatalf("vector file: %d properties cases, want 6", len(v.Properties))
	}
	return &v
}

// The entry lines equal the golden lines byte for byte, and the parser of
// the read path gives every key and value back.
func TestEntryLinesVectors(t *testing.T) {
	for _, c := range loadVectors(t).Properties {
		got, err := EntryLines(c.Items)
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		want := strings.Join(c.Lines, "\n") + "\n"
		if string(got) != want {
			t.Errorf("%s:\n%s\nwant\n%s", c.Name, got, want)
			continue
		}
		for i, b := range got {
			if b > 0x7e || b < 0x20 && b != '\n' {
				t.Errorf("%s: byte %#x at %d is not printable ASCII", c.Name, b, i)
				break
			}
		}
		back, err := blobprops.Load(string(got))
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		if !reflect.DeepEqual(back, c.Items) {
			t.Errorf("%s: read back %q, want %q", c.Name, back, c.Items)
		}
	}
}

// A whole file: the time stamp of the writer and the date of
// Date.toString, both in UTC, then the entry lines.
func TestPropertiesFile(t *testing.T) {
	items := map[string]string{
		"@BlobStore.blob-name":     "/docs/readme.txt",
		"@BlobStore.content-type":  "text/plain",
		"@BlobStore.created-by":    "demo-user",
		"@BlobStore.created-by-ip": "192.0.2.10",
		"@Bucket.repo-name":        "demo-raw",
		"creationTime":             "1794996930123",
		"sha1":                     "62cdb7020ff920e5aa642c3d4066950dd1f01f4d",
		"size":                     "3",
	}
	now := time.Date(2026, 11, 8, 10, 15, 30, 123_456_789, time.FixedZone("CET", 3600))
	got, err := Properties(items, now)
	if err != nil {
		t.Fatal(err)
	}
	lines, _ := EntryLines(items)
	want := "#2026-11-08 09:15:30,123+0000\n#Sun Nov 08 09:15:30 UTC 2026\n" + string(lines)
	if string(got) != want {
		t.Fatalf("%s\nwant\n%s", got, want)
	}
	props, err := blobprops.Parse(got)
	if err != nil || props == nil {
		t.Fatalf("%+v %v", props, err)
	}
	if props.Repo != "demo-raw" || props.Path != "docs/readme.txt" || props.Size != 3 || props.CreatedMS != 1794996930123 ||
		props.SHA1 == nil || *props.SHA1 != items["sha1"] || props.ContentType == nil || *props.ContentType != "text/plain" ||
		props.Deleted || props.IsCopy {
		t.Fatalf("%+v", props)
	}
}

// Go strings may hold bytes that are no UTF-8; Java strings cannot.
func TestEntryLinesRefuseInvalidText(t *testing.T) {
	for _, items := range []map[string]string{
		{"key": "bad \xff byte"},
		{"bad \xc3 key": "value"},
		{"surrogate": "\xed\xa0\x80"},
	} {
		if got, err := EntryLines(items); err == nil {
			t.Errorf("%q: %q, want an error", items, got)
		}
		if got, err := Properties(items, time.Unix(0, 0)); err == nil {
			t.Errorf("%q: %q, want an error", items, got)
		}
	}
	got, err := EntryLines(map[string]string{})
	if err != nil || len(got) != 0 {
		t.Fatalf("%q %v", got, err)
	}
}
