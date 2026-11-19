package policy

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

type vectorFile struct {
	Write []struct {
		Policy  string `json:"policy"`
		Exists  bool   `json:"exists"`
		Exempt  bool   `json:"exempt"`
		Refused string `json:"refused"`
	} `json:"write"`
	MavenMetadata []struct {
		Path   string `json:"path"`
		Exempt bool   `json:"exempt"`
	} `json:"maven_metadata"`
	Latest []struct {
		Path   string `json:"path"`
		Latest bool   `json:"latest"`
	} `json:"latest"`
	Version []struct {
		Policy  string `json:"policy"`
		Path    string `json:"path"`
		Refused string `json:"refused"`
	} `json:"version"`
	Layout []struct {
		Path  string `json:"path"`
		Maven bool   `json:"maven"`
	} `json:"layout"`
	ContentType []struct {
		Path    string `json:"path"`
		HeadHex string `json:"head_hex"`
		Type    string `json:"type"`
	} `json:"content_type"`
	Signatures []struct {
		Path    string `json:"path"`
		HeadHex string `json:"head_hex"`
		Refused bool   `json:"refused"`
	} `json:"signatures"`
}

func load(t *testing.T) *vectorFile {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/policy.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Write) < 9 || len(v.Version) < 9 || len(v.Layout) != 40 || len(v.ContentType) < 10 || len(v.Signatures) < 10 ||
		len(v.MavenMetadata) < 5 || len(v.Latest) < 5 {
		t.Fatalf("vector file too short: %d write, %d version, %d layout", len(v.Write), len(v.Version), len(v.Layout))
	}
	return &v
}

// rule is the rule of a refusal, "" for none.
func rule(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("error %v is no *Refusal", err)
	}
	if r.Message == "" || r.Error() != r.Message {
		t.Fatalf("refusal %+v without a message", r)
	}
	return string(r.Rule)
}

func head(t *testing.T, text string) []byte {
	t.Helper()
	data, err := hex.DecodeString(text)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestWritePolicies(t *testing.T) {
	for _, c := range load(t).Write {
		if got := rule(t, Write(c.Policy, c.Exists, c.Exempt)); got != c.Refused {
			t.Errorf("Write(%q, exists %v, exempt %v): %q, want %q", c.Policy, c.Exists, c.Exempt, got, c.Refused)
		}
	}
}

func TestExemptions(t *testing.T) {
	v := load(t)
	for _, c := range v.MavenMetadata {
		if got := MavenMetadata(c.Path); got != c.Exempt {
			t.Errorf("MavenMetadata(%q) = %v", c.Path, got)
		}
	}
	for _, c := range v.Latest {
		if got := LatestTag(c.Path); got != c.Latest {
			t.Errorf("LatestTag(%q) = %v", c.Path, got)
		}
	}
}

func TestVersionPolicies(t *testing.T) {
	for _, c := range load(t).Version {
		if got := rule(t, Version(c.Policy, c.Path)); got != c.Refused {
			t.Errorf("Version(%q, %q): %q, want %q", c.Policy, c.Path, got, c.Refused)
		}
	}
}

func TestLayoutPolicies(t *testing.T) {
	maven, other := 0, 0
	for _, c := range load(t).Layout {
		if got := MavenPath(c.Path); got != c.Maven {
			t.Errorf("MavenPath(%q) = %v", c.Path, got)
		}
		want := ""
		if !c.Maven {
			want = string(RuleLayout)
			other++
		} else {
			maven++
		}
		if got := rule(t, Layout(Strict, c.Path)); got != want {
			t.Errorf("Layout(STRICT, %q): %q, want %q", c.Path, got, want)
		}
		if got := rule(t, Layout("strict", c.Path)); got != want {
			t.Errorf("Layout(strict, %q): %q, want %q", c.Path, got, want)
		}
		for _, permissive := range []string{Permissive, "permissive", ""} {
			if got := rule(t, Layout(permissive, c.Path)); got != "" {
				t.Errorf("Layout(%q, %q): %q", permissive, c.Path, got)
			}
		}
	}
	if maven < 15 || other < 15 {
		t.Fatalf("%d Maven paths, %d others", maven, other)
	}
}

func TestContentTypes(t *testing.T) {
	for _, c := range load(t).ContentType {
		if got := ContentType(c.Path, head(t, c.HeadHex)); got != c.Type {
			t.Errorf("ContentType(%q, %s) = %q, want %q", c.Path, c.HeadHex, got, c.Type)
		}
	}
}

func TestSignatures(t *testing.T) {
	for _, c := range load(t).Signatures {
		want := ""
		if c.Refused {
			want = string(RuleContent)
		}
		if got := rule(t, Content(c.Path, head(t, c.HeadHex))); got != want {
			t.Errorf("Content(%q, %s): %q, want %q", c.Path, c.HeadHex, got, want)
		}
	}
	if HeadBytes < 8 {
		t.Fatalf("HeadBytes %d", HeadBytes)
	}
}
