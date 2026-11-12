package listing_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/listing"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

// The vectors come from tools/vectors/listing.py, which calls render() of
// listing.py in the reference. Texts are base64 of their Python str form.

type renderCase struct {
	Name      string `json:"name"`
	Repo      string `json:"repo"`
	Directory string `json:"directory"`
	Children  []struct {
		Name      string `json:"name"`
		IsDir     bool   `json:"is_dir"`
		Size      *int64 `json:"size"`
		CreatedMS *int64 `json:"created_ms"`
	} `json:"children"`
	Offset     string  `json:"offset"`
	NextOffset *string `json:"next_offset"`
	Page       *string `json:"page"`
	Error      string  `json:"error"`
	Message    string  `json:"message"`
}

func load(t *testing.T) []renderCase {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/listing.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Render []renderCase `json:"render"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Render) < 20 {
		t.Fatalf("%d cases", len(v.Render))
	}
	return v.Render
}

func text(t *testing.T, s string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func number(t *testing.T, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("offset %q", s)
	}
	return n
}

func TestRender(t *testing.T) {
	pages, raised := 0, 0
	for _, c := range load(t) {
		var children []index.Child
		for _, child := range c.Children {
			children = append(children, index.Child{Name: text(t, child.Name), IsDir: child.IsDir, Size: child.Size,
				CreatedMS: child.CreatedMS})
		}
		var next *big.Int
		if c.NextOffset != nil {
			next = number(t, *c.NextOffset)
		}
		got, err := listing.Render(text(t, c.Repo), text(t, c.Directory), children, number(t, c.Offset), next)
		if c.Error != "" {
			raised++
			var pyErr *pyjson.Error
			if !errors.As(err, &pyErr) || pyErr.Class != c.Error || pyErr.Message != c.Message {
				t.Errorf("%s: got %v, want %s %q", c.Name, err, c.Error, c.Message)
			}
			continue
		}
		pages++
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		if want := text(t, *c.Page); got != want {
			t.Errorf("%s:\n got %q\nwant %q", c.Name, got, want)
		}
	}
	if pages < 20 || raised < 3 {
		t.Fatalf("%d pages, %d errors", pages, raised)
	}
}

func TestRenderLinksAreRelative(t *testing.T) {
	size, created := int64(1234), int64(1_700_000_000_000)
	page, err := listing.Render("demo-releases", "com/example",
		[]index.Child{{Name: "a", IsDir: true}, {Name: "demo-1.0.jar", Size: &size, CreatedMS: &created}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`href="../"`, `href="a/"`, `href="demo-1.0.jar"`, ">1234<", "2023-11-14 22:13:20 UTC"} {
		if !strings.Contains(page, want) {
			t.Errorf("no %s in the page", want)
		}
	}
	if strings.Contains(page, `href="/`) || strings.Index(page, `href="a/"`) > strings.Index(page, `href="demo-1.0.jar"`) {
		t.Errorf("absolute link or wrong order:\n%s", page)
	}
}
