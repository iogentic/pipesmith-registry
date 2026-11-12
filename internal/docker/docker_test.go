package docker_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/mirosso25/pipesmith-registry/internal/docker"
	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
)

// The vectors come from tools/vectors/docker.py, which runs docker.py of the
// reference over the grammar cases of tests/test_docker.py and invented ones.
type vectorFile struct {
	Python    string `json:"python"`
	Constants struct {
		APIVersionHeader string `json:"API_VERSION_HEADER"`
		APIVersion       string `json:"API_VERSION"`
		DigestHeader     string `json:"DIGEST_HEADER"`
		Schema1Signed    string `json:"SCHEMA1_SIGNED"`
		MaxNameLength    int    `json:"MAX_NAME_LENGTH"`
		MaxManifestBytes int    `json:"MAX_MANIFEST_BYTES"`
	} `json:"constants"`
	ParseV2 []struct {
		Path   string `json:"path_b64"`
		Result *struct {
			Kind      string `json:"kind"`
			Name      string `json:"name"`
			Reference string `json:"reference"`
		} `json:"result"`
		Error *struct {
			Code       string `json:"code"`
			Message    string `json:"message"`
			ValueError bool   `json:"value_error"`
		} `json:"error"`
	} `json:"parse_v2"`
	IsDigest []struct {
		Text   string `json:"text_b64"`
		Result bool   `json:"result"`
	} `json:"is_digest"`
	DigestOf []struct {
		Data      string `json:"data_b64"`
		Algorithm string `json:"algorithm"`
		Digest    string `json:"digest"`
	} `json:"digest_of"`
	TagNames []struct {
		Names []string `json:"names_b64"`
		Tags  []string `json:"tags"`
	} `json:"tag_names"`
	ErrorBody []struct {
		Code    string `json:"code_b64"`
		Message string `json:"message_b64"`
		Body    string `json:"body_b64"`
	} `json:"error_body"`
	Referenced []struct {
		Name    string   `json:"name"`
		Data    string   `json:"data_b64"`
		Digests []string `json:"digests"`
		Error   *string  `json:"error"`
	} `json:"referenced_digests"`
	Schema1 []struct {
		Name        string  `json:"name"`
		Data        string  `json:"data_b64"`
		ContentType *string `json:"content_type"`
		Result      bool    `json:"result"`
	} `json:"is_schema1_signed"`
}

func load(t *testing.T) *vectorFile {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/docker.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.ParseV2) < 50 || len(v.Referenced) < 10 || len(v.Schema1) < 50 {
		t.Fatal("vector file holds too few cases")
	}
	return &v
}

func unb64(t *testing.T, s string) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestConstants(t *testing.T) {
	c := load(t).Constants
	if docker.APIVersionHeader != c.APIVersionHeader || docker.APIVersion != c.APIVersion ||
		docker.DigestHeader != c.DigestHeader || docker.Schema1Signed != c.Schema1Signed ||
		docker.MaxNameLength != c.MaxNameLength || docker.MaxManifestBytes != c.MaxManifestBytes {
		t.Errorf("constants differ from %+v", c)
	}
}

func TestParseV2Vectors(t *testing.T) {
	v := load(t)
	for _, c := range v.ParseV2 {
		path := string(unb64(t, c.Path))
		got, err := docker.ParseV2(path)
		switch {
		case c.Error != nil:
			var pe *docker.PathError
			if !errors.As(err, &pe) || pe.Code != c.Error.Code || pe.Error() != c.Error.Message {
				t.Errorf("ParseV2(%q) = %+v %v, want %s %q", path, got, err, c.Error.Code, c.Error.Message)
			}
			if !pyjson.IsValueError(err) {
				t.Errorf("ParseV2(%q): %v is no ValueError", path, err)
			}
			if got != nil {
				t.Errorf("ParseV2(%q) returned %+v with the error", path, got)
			}
		case c.Result == nil:
			if got != nil || err != nil {
				t.Errorf("ParseV2(%q) = %+v %v, want no registry call", path, got, err)
			}
		default:
			want := docker.V2Path{Kind: c.Result.Kind, Name: c.Result.Name, Reference: c.Result.Reference}
			if err != nil || got == nil || *got != want {
				t.Errorf("ParseV2(%q) = %+v %v, want %+v", path, got, err, want)
			}
		}
	}
}

func TestIsDigestVectors(t *testing.T) {
	for _, c := range load(t).IsDigest {
		text := string(unb64(t, c.Text))
		if got := docker.IsDigest(text); got != c.Result {
			t.Errorf("IsDigest(%q) = %v, want %v", text, got, c.Result)
		}
	}
}

func TestDigestOfVectors(t *testing.T) {
	for _, c := range load(t).DigestOf {
		data := unb64(t, c.Data)
		got, err := docker.DigestOfAlgorithm(data, c.Algorithm)
		if err != nil || got != c.Digest {
			t.Errorf("digest_of(%q, %s) = %s %v, want %s", data, c.Algorithm, got, err, c.Digest)
		}
		if c.Algorithm == "sha256" && docker.DigestOf(data) != c.Digest {
			t.Errorf("DigestOf(%q) = %s", data, docker.DigestOf(data))
		}
	}
	if _, err := docker.DigestOfAlgorithm(nil, "demo-hash"); err == nil || err.Error() != "unsupported hash type demo-hash" {
		t.Errorf("unknown algorithm: %v", err)
	}
}

func TestTagNamesVectors(t *testing.T) {
	for _, c := range load(t).TagNames {
		var names []string
		for _, n := range c.Names {
			names = append(names, string(unb64(t, n)))
		}
		got := docker.TagNames(names)
		if got == nil || !reflect.DeepEqual(got, append([]string{}, c.Tags...)) {
			t.Errorf("TagNames(%q) = %#v, want %q", names, got, c.Tags)
		}
	}
}

func TestErrorBodyVectors(t *testing.T) {
	for _, c := range load(t).ErrorBody {
		code, message := string(unb64(t, c.Code)), string(unb64(t, c.Message))
		if got, want := docker.ErrorBody(code, message), unb64(t, c.Body); string(got) != string(want) {
			t.Errorf("ErrorBody(%q, %q) = %q, want %q", code, message, got, want)
		}
	}
}

func TestReferencedDigestsVectors(t *testing.T) {
	for _, c := range load(t).Referenced {
		got, err := docker.ReferencedDigests(unb64(t, c.Data))
		if c.Error != nil {
			if err == nil || err.Error() != *c.Error || !pyjson.IsValueError(err) {
				t.Errorf("%s: %v %v, want %q", c.Name, got, err, *c.Error)
			}
			continue
		}
		if err != nil || got == nil || !reflect.DeepEqual(got, append([]string{}, c.Digests...)) {
			t.Errorf("%s: %#v %v, want %q", c.Name, got, err, c.Digests)
		}
	}
}

func TestIsSchema1SignedVectors(t *testing.T) {
	for _, c := range load(t).Schema1 {
		if got := docker.IsSchema1Signed(unb64(t, c.Data), c.ContentType); got != c.Result {
			ct := "None"
			if c.ContentType != nil {
				ct = *c.ContentType
			}
			t.Errorf("%s with %q: %v, want %v", c.Name, ct, got, c.Result)
		}
	}
}

func TestPathErrorKinds(t *testing.T) {
	_, err := docker.ParseV2("Demo/manifests/1")
	var pe *docker.PathError
	if !errors.As(err, &pe) || pe.Code != "NAME_INVALID" {
		t.Fatalf("%v", err)
	}
	// the positive twin: a valid path gives no error at all
	if call, err := docker.ParseV2("demo/manifests/1"); err != nil || call.Kind != "manifest" {
		t.Errorf("%+v %v", call, err)
	}
}
