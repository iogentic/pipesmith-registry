package server

import (
	"encoding/json"
	"math/big"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"github.com/mirosso25/pipesmith-registry/internal/index"
	"github.com/mirosso25/pipesmith-registry/internal/maven"
)

// The vectors come from tools/vectors/server.py, which runs the helpers of
// server.py and maven.py of the reference and the query parser of Starlette.

type outcome struct {
	Value   json.RawMessage `json:"value"`
	Error   *string         `json:"error"`
	Message string          `json:"message"`
}

type vectorFile struct {
	Python    string `json:"python"`
	Constants struct {
		RepositoryPrefix  string   `json:"repository_prefix"`
		BrowsePrefix      string   `json:"browse_prefix"`
		Allow             string   `json:"allow"`
		ChunkBytes        int      `json:"chunk_bytes"`
		Text              string   `json:"text"`
		Octet             string   `json:"octet"`
		JSONType          string   `json:"json_type"`
		LocalHostNames    []string `json:"local_host_names"`
		DockerBlobPrefix  string   `json:"docker_blob_prefix"`
		FileAccess        []string `json:"file_access"`
		CoordinateFormats []string `json:"coordinate_formats"`
		Forbidden         string   `json:"forbidden"`
	} `json:"constants"`
	Latin1Space []int             `json:"latin1_space"`
	Latin1Digit []int             `json:"latin1_digit"`
	Phrases     map[string]string `json:"phrases"`
	ParseRange  []struct {
		Header string `json:"header"`
		Size   int64  `json:"size"`
		outcome
	} `json:"parse_range"`
	IfRange []struct {
		Header     *string `json:"header"`
		Etag       string  `json:"etag"`
		ModifiedMS *int64  `json:"modified_ms"`
		outcome
	} `json:"if_range"`
	NoneMatch []struct {
		Header string `json:"header"`
		Etag   string `json:"etag"`
		outcome
	} `json:"none_match"`
	NotModified []struct {
		IfNoneMatch     *string `json:"if_none_match"`
		IfModifiedSince *string `json:"if_modified_since"`
		Etag            string  `json:"etag"`
		ModifiedMS      *int64  `json:"modified_ms"`
		outcome
	} `json:"not_modified"`
	ParseHTTPDate []struct {
		Text string `json:"text"`
		outcome
	} `json:"parse_http_date"`
	HTTPDate []struct {
		MS int64 `json:"ms"`
		outcome
	} `json:"http_date"`
	LocalHost []struct {
		Value *string `json:"value"`
		Local bool    `json:"local"`
	} `json:"local_host"`
	Target []struct {
		Raw    string  `json:"raw"`
		Prefix string  `json:"prefix"`
		Error  *string `json:"error"`
		Target *struct {
			Repo      string `json:"repo"`
			Path      string `json:"path"`
			Directory bool   `json:"directory"`
			Bare      bool   `json:"bare"`
		} `json:"target"`
	} `json:"target"`
	URL []struct {
		Prefix, Repo, Path, URL string
	} `json:"url"`
	Quote []struct {
		Text  string `json:"text"`
		Slash string `json:"slash"`
		None  string `json:"none"`
	} `json:"quote"`
	Paths []struct {
		Path        string    `json:"path"`
		ContentType string    `json:"content_type"`
		Checksum    []*string `json:"checksum"`
		Metadata    bool      `json:"metadata"`
		Coordinates *struct {
			GroupID    string  `json:"group_id"`
			ArtifactID string  `json:"artifact_id"`
			Version    string  `json:"version"`
			Classifier *string `json:"classifier"`
			Extension  string  `json:"extension"`
		} `json:"coordinates"`
	} `json:"paths"`
	HeaderSafe []struct {
		Value *string `json:"value"`
		Safe  bool    `json:"safe"`
	} `json:"header_safe"`
	APIPath []struct {
		Value  *string `json:"value"`
		Path   *string `json:"path"`
		Status int     `json:"status"`
		Error  *string `json:"error"`
	} `json:"api_path"`
	IntParam []struct {
		Query   string      `json:"query"`
		Name    string      `json:"name"`
		Default int64       `json:"default"`
		Low     int64       `json:"low"`
		High    *int64      `json:"high"`
		Value   json.Number `json:"value"`
		Status  int         `json:"status"`
		Error   *string     `json:"error"`
	} `json:"int_param"`
	Query []struct {
		Query string            `json:"query"`
		Items [][2]string       `json:"items"`
		Last  map[string]string `json:"last"`
	} `json:"query"`
	AccessPath []struct {
		Raw   string `json:"raw"`
		Query string `json:"query"`
		Line  string `json:"line"`
		Route string `json:"route"`
	} `json:"access_path"`
	FileBody []struct {
		Stored      int64   `json:"stored"`
		Real        int64   `json:"real"`
		SHA1        *string `json:"sha1"`
		ContentType *string `json:"content_type"`
		Path        string  `json:"path"`
		Etag        string  `json:"etag"`
		BodyType    string  `json:"body_type"`
		Size        int64   `json:"size"`
		ModifiedMS  *int64  `json:"modified_ms"`
	} `json:"file_body"`
}

func loadVectors(t *testing.T) *vectorFile {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/server.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.ParseRange) < 200 || len(v.Target) < 50 || len(v.IfRange) < 25 || len(v.ParseHTTPDate) < 100 {
		t.Fatalf("vector file too small: %d range, %d target cases", len(v.ParseRange), len(v.Target))
	}
	return &v
}

// latin1 is the header value of a latin-1 text: one byte per character.
func latin1(t *testing.T, s string) string {
	t.Helper()
	b := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 0xff {
			t.Fatalf("%q is not latin-1", s)
		}
		b = append(b, byte(r))
	}
	return string(b)
}

func TestConstants(t *testing.T) {
	v := loadVectors(t)
	c := v.Constants
	if c.RepositoryPrefix != repositoryPrefix || c.BrowsePrefix != browsePrefix || c.Allow != allow ||
		c.ChunkBytes != chunkBytes || c.Text != textType || c.Octet != octet || c.JSONType != jsonType ||
		!slices.Equal(c.LocalHostNames, localHostNames[:]) || c.DockerBlobPrefix != dockerBlobPrefix ||
		!slices.Equal(c.FileAccess, fileAccess[:]) || !slices.Equal(c.CoordinateFormats, coordinateFormats[:]) ||
		c.Forbidden != forbidden {
		t.Fatalf("constants differ from the reference: %+v", c)
	}
}

func TestLatin1Classes(t *testing.T) {
	v := loadVectors(t)
	var space, digit []int
	for c := 0; c < 256; c++ {
		if latin1Space(byte(c)) {
			space = append(space, c)
		}
		if latin1Digit(byte(c)) {
			digit = append(digit, c)
		}
	}
	if !slices.Equal(space, v.Latin1Space) || !slices.Equal(digit, v.Latin1Digit) {
		t.Fatalf("space %v digit %v, want %v %v", space, digit, v.Latin1Space, v.Latin1Digit)
	}
}

func TestPhrases(t *testing.T) {
	v := loadVectors(t)
	for code, want := range v.Phrases {
		n, _ := strconv.Atoi(code)
		if got := statusPhrase(n); got != want {
			t.Errorf("phrase of %d: %q, want %q", n, got, want)
		}
	}
	if statusPhrase(599) != "" || statusPhrase(416) != "Requested Range Not Satisfiable" {
		t.Errorf("phrase of 599 %q, of 416 %q", statusPhrase(599), statusPhrase(416))
	}
}

func TestParseRangeVectors(t *testing.T) {
	v := loadVectors(t)
	failures := 0
	for _, c := range v.ParseRange {
		first, last, kind, err := parseRange(latin1(t, c.Header), c.Size)
		if c.Error != nil || err != nil {
			failures++
			if c.Error == nil || err == nil || PyClass(err) != *c.Error || err.Error() != c.Message {
				t.Errorf("parse_range(%.30q, %d): err %v, want %v %s", c.Header, c.Size, err, deref(c.Error), c.Message)
			}
			continue
		}
		var got string
		switch kind {
		case rangeIgnored:
			got = "null"
		case rangeUnsatisfiable:
			got = `"unsatisfiable"`
		default:
			got = "[" + strconv.FormatInt(first, 10) + ", " + strconv.FormatInt(last, 10) + "]"
		}
		var want, have any
		if err := json.Unmarshal(c.Value, &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(got), &have); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(have, want) {
			t.Errorf("parse_range(%q, %d) = %s, want %s", c.Header, c.Size, got, c.Value)
		}
	}
	if failures < 6 {
		t.Fatalf("%d ValueError cases, the generator writes 6", failures)
	}
}

func TestIfRangeVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.IfRange {
		var header *string
		if c.Header != nil {
			h := latin1(t, *c.Header)
			header = &h
		}
		var want bool
		if err := json.Unmarshal(c.Value, &want); err != nil {
			t.Fatal(err)
		}
		got, err := ifRangeAllows(header, c.Etag, c.ModifiedMS)
		if err != nil || got != want {
			t.Errorf("if_range(%v, %q, %v) = %v, %v; want %v", deref(c.Header), c.Etag, deref64(c.ModifiedMS), got, err, want)
		}
	}
}

func deref(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func deref64(n *int64) any {
	if n == nil {
		return nil
	}
	return *n
}

func TestNoneMatchVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.NoneMatch {
		var want bool
		if err := json.Unmarshal(c.Value, &want); err != nil {
			t.Fatal(err)
		}
		if got := noneMatch(latin1(t, c.Header), c.Etag); got != want {
			t.Errorf("none_match(%q, %q) = %v, want %v", c.Header, c.Etag, got, want)
		}
	}
}

func TestNotModifiedVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.NotModified {
		h := http.Header{}
		if c.IfNoneMatch != nil {
			h["If-None-Match"] = []string{latin1(t, *c.IfNoneMatch)}
		}
		if c.IfModifiedSince != nil {
			h["If-Modified-Since"] = []string{latin1(t, *c.IfModifiedSince)}
		}
		got, err := notModified(h, c.Etag, c.ModifiedMS)
		if c.Error != nil {
			if !isOverflow(err) {
				t.Errorf("not_modified(%v, %v): err %v, want %s", deref(c.IfNoneMatch), deref(c.IfModifiedSince), err, *c.Error)
			}
			continue
		}
		var want bool
		if err := json.Unmarshal(c.Value, &want); err != nil {
			t.Fatal(err)
		}
		if err != nil || got != want {
			t.Errorf("not_modified(%v, %v, %q, %v) = %v, %v, want %v", deref(c.IfNoneMatch), deref(c.IfModifiedSince),
				c.Etag, deref64(c.ModifiedMS), got, err, want)
		}
	}
}

func TestParseHTTPDateVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.ParseHTTPDate {
		sec, ok, err := parseHTTPDate(latin1(t, c.Text))
		var want *int64
		if err := json.Unmarshal(c.Value, &want); c.Error == nil && err != nil {
			t.Fatal(err)
		}
		switch {
		case c.Error != nil:
			if !isOverflow(err) {
				t.Errorf("parse_http_date(%q) = %d, %v, %v; want %s", c.Text, sec, ok, err, *c.Error)
			}
		case want == nil:
			if ok || err != nil {
				t.Errorf("parse_http_date(%q) = %d, %v, %v; want None", c.Text, sec, ok, err)
			}
		case !ok || err != nil || sec != *want:
			t.Errorf("parse_http_date(%q) = %d, %v, %v; want %d", c.Text, sec, ok, err, *want)
		}
	}
}

func TestHTTPDateVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.HTTPDate {
		var want *string
		if err := json.Unmarshal(c.Value, &want); err != nil {
			t.Fatal(err)
		}
		got, ok := httpDate(c.MS)
		if (want == nil) != !ok || (want != nil && got != *want) {
			t.Errorf("http_date(%d) = %q, %v; want %v", c.MS, got, ok, deref(want))
		}
	}
}

func TestLocalHostVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.LocalHost {
		value := ""
		if c.Value != nil {
			value = latin1(t, *c.Value)
		}
		if got := localHost(value); got != c.Local {
			t.Errorf("local_host(%q) = %v, want %v", deref(c.Value), got, c.Local)
		}
	}
}

func TestTargetVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Target {
		got, err := parseTarget(c.Raw, c.Prefix)
		if c.Error != nil {
			if err == nil || err.Error() != *c.Error {
				t.Errorf("target(%q) = %+v, %v; want error %q", c.Raw, got, err, *c.Error)
			}
			continue
		}
		if err != nil {
			t.Errorf("target(%q): %v", c.Raw, err)
			continue
		}
		if c.Target == nil {
			if got != nil {
				t.Errorf("target(%q) = %+v, want None", c.Raw, got)
			}
			continue
		}
		if got == nil || got.repo != c.Target.Repo || got.path != c.Target.Path || got.directory != c.Target.Directory ||
			got.bare != c.Target.Bare {
			t.Errorf("target(%q) = %+v, want %+v", c.Raw, got, c.Target)
		}
	}
}

func TestURLAndQuoteVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.URL {
		if got := repoURL(c.Prefix, c.Repo, c.Path); got != c.URL {
			t.Errorf("url(%q, %q, %q) = %q, want %q", c.Prefix, c.Repo, c.Path, got, c.URL)
		}
	}
	for _, c := range v.Quote {
		if got := quote(c.Text, "/"); got != c.Slash {
			t.Errorf("quote(%q, '/') = %q, want %q", c.Text, got, c.Slash)
		}
		if got := quote(c.Text, ""); got != c.None {
			t.Errorf("quote(%q, '') = %q, want %q", c.Text, got, c.None)
		}
	}
}

func TestMavenPathVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Paths {
		if got := maven.ContentTypeFor(c.Path); got != c.ContentType {
			t.Errorf("content_type_for(%q) = %q, want %q", c.Path, got, c.ContentType)
		}
		base, suffix := maven.SplitChecksum(c.Path)
		wantSuffix := ""
		if c.Checksum[1] != nil {
			wantSuffix = *c.Checksum[1]
		}
		if base != *c.Checksum[0] || suffix != wantSuffix {
			t.Errorf("split_checksum(%q) = %q, %q; want %q, %q", c.Path, base, suffix, *c.Checksum[0], wantSuffix)
		}
		if got := maven.IsMetadataPath(c.Path); got != c.Metadata {
			t.Errorf("is_metadata_path(%q) = %v", c.Path, got)
		}
		got := maven.CoordinatesOf(c.Path)
		switch {
		case c.Coordinates == nil && got != nil:
			t.Errorf("coordinates(%q) = %+v, want None", c.Path, got)
		case c.Coordinates != nil && got == nil:
			t.Errorf("coordinates(%q) = None, want %+v", c.Path, c.Coordinates)
		case c.Coordinates != nil:
			w := c.Coordinates
			if got.GroupID != w.GroupID || got.ArtifactID != w.ArtifactID || got.Version != w.Version ||
				!reflect.DeepEqual(got.Classifier, w.Classifier) || got.Extension != w.Extension {
				t.Errorf("coordinates(%q) = %+v, want %+v", c.Path, got, w)
			}
		}
	}
}

func TestHeaderSafeAndAPIPathVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.HeaderSafe {
		if got := headerSafe(c.Value); got != c.Safe {
			t.Errorf("header_safe(%v) = %v", deref(c.Value), got)
		}
	}
	for _, c := range v.APIPath {
		got, err := apiPath(c.Value)
		if c.Error != nil {
			if err == nil || err.status != c.Status || err.message != *c.Error {
				t.Errorf("api_path(%v) = %q, %v; want %d %q", deref(c.Value), got, err, c.Status, *c.Error)
			}
			continue
		}
		if err != nil || got != *c.Path {
			t.Errorf("api_path(%v) = %q, %v; want %q", deref(c.Value), got, err, *c.Path)
		}
	}
}

func TestQueryAndIntParamVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Query {
		q := parseQuery(c.Query)
		items := [][2]string{}
		for _, p := range q {
			items = append(items, [2]string{p.name, p.value})
		}
		if !reflect.DeepEqual(items, c.Items) && !(len(items) == 0 && len(c.Items) == 0) {
			t.Errorf("query %q: %q, want %q", c.Query, items, c.Items)
		}
		for key, want := range c.Last {
			if got, ok := q.get(key); !ok || got != want {
				t.Errorf("query %q [%q] = %q, %v; want %q", c.Query, key, got, ok, want)
			}
		}
	}
	for _, c := range v.IntParam {
		got, err := intParam(parseQuery(c.Query), c.Name, c.Default, c.Low, c.High)
		if c.Error != nil {
			if err == nil || err.status != c.Status || err.message != *c.Error {
				t.Errorf("int_param(%q) = %v, %v; want %d %q", c.Query, got, err, c.Status, *c.Error)
			}
			continue
		}
		want, _ := new(big.Int).SetString(string(c.Value), 10)
		if err != nil || got.Cmp(want) != 0 {
			t.Errorf("int_param(%q) = %v, %v; want %v", c.Query, got, err, want)
		}
	}
}

func TestAccessPathVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.AccessPath {
		if got := routePath(c.Raw); got != c.Route {
			t.Errorf("route path of %q = %q, want %q", c.Raw, got, c.Route)
		}
		if got := accessPath(c.Raw, c.Query); got != c.Line {
			t.Errorf("access path of %q ? %q = %q, want %q", c.Raw, c.Query, got, c.Line)
		}
	}
}

func TestFileBodyVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.FileBody {
		asset := &index.Asset{Repo: "demo", Path: c.Path, Size: c.Stored, SHA1: c.SHA1, ContentType: c.ContentType,
			CreatedMS: 1_700_000_000_000}
		b := fileBody(asset, nil, c.Real)
		if b.etag != c.Etag || b.contentType != c.BodyType || b.size != c.Size || !reflect.DeepEqual(b.modifiedMS, c.ModifiedMS) {
			t.Errorf("file_body(%+v) = %+v", c, b)
		}
	}
}
