package manifest

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"
	"unicode/utf8"
)

// fuzzSeeds are the part texts of testdata/vectors/manifest.json: every
// parse line and every file of the read cases.
func fuzzSeeds(f *testing.F) [][]byte {
	f.Helper()
	data, err := os.ReadFile("../../testdata/vectors/manifest.json")
	if err != nil {
		f.Fatal(err)
	}
	var v struct {
		Parse []struct {
			Line string `json:"line"`
		} `json:"parse"`
		Read []struct {
			Files []struct {
				Data string `json:"data"`
			} `json:"files"`
		} `json:"read"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		f.Fatal(err)
	}
	var seeds [][]byte
	for _, c := range v.Parse {
		if b, err := base64.StdEncoding.DecodeString(c.Line); err == nil {
			seeds = append(seeds, b)
		}
	}
	for _, c := range v.Read {
		for _, file := range c.Files {
			if b, err := base64.StdEncoding.DecodeString(file.Data); err == nil {
				seeds = append(seeds, b)
			}
		}
	}
	if len(seeds) < 50 {
		f.Fatalf("%d seeds from the vectors", len(seeds))
	}
	return seeds
}

// FuzzReadPart reads a plain part as ReadRows does, through a small buffer
// so that long lines take the joining path of eachLine. The lines put
// together are the input, and every row holds a repository and a path of
// valid UTF-8.
func FuzzReadPart(f *testing.F) {
	for _, seed := range fuzzSeeds(f) {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var joined bytes.Buffer
		err := eachLine(bufio.NewReaderSize(bytes.NewReader(data), 16), func(line []byte) error {
			joined.Write(line)
			if i := bytes.IndexByte(line, '\n'); i >= 0 && i != len(line)-1 {
				t.Fatalf("a line holds a newline before its end: %q", line)
			}
			row, bad, err := parseLine(line)
			if err != nil {
				var e *Error
				if !errors.As(err, &e) || e.Class != "ValueError" {
					t.Fatalf("parseLine(%q): %v, want a ValueError", line, err)
				}
				return nil
			}
			if row != nil && bad {
				t.Fatalf("parseLine(%q) gives a row and calls the line malformed", line)
			}
			if row == nil {
				return nil
			}
			for _, field := range []string{row.Repo, row.Path} {
				if !utf8.ValidString(field) {
					t.Fatalf("parseLine(%q): field %q is no UTF-8", line, field)
				}
			}
			if row.Overflow && row.Size != 0 && row.CreatedMS != 0 {
				t.Fatalf("parseLine(%q): overflow with size %d and creation %d", line, row.Size, row.CreatedMS)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("eachLine: %v", err)
		}
		if !bytes.Equal(joined.Bytes(), data) {
			t.Fatalf("the lines give %q, want %q", joined.Bytes(), data)
		}
	})
}

// FuzzReadGzipPart reads a gzip part through the reader of ReadRows. It
// ends with lines or an error, and never with a panic.
func FuzzReadGzipPart(f *testing.F) {
	f.Add([]byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	for _, seed := range fuzzSeeds(f) {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		_, _ = w.Write(seed)
		_ = w.Close()
		f.Add(b.Bytes())
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = eachLine(bufio.NewReaderSize(io.LimitReader(newGzipReader(bytes.NewReader(data), "fuzz.tsv.gz"), 1<<22), 64*1024),
			func(line []byte) error {
				_, _, _ = parseLine(line)
				return nil
			})
	})
}
