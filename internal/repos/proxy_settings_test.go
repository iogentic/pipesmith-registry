package repos_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// The vectors of testdata/vectors/proxy_settings.json are written by hand;
// the file names its sources. Attributes are decoded as internal/state
// decodes them, with json.Number, so 1440.0 reaches the reader as stored.
type settingsVectors struct {
	Sources  []string       `json:"sources"`
	Defaults wantDefaults   `json:"defaults"`
	Proxy    []proxyVector  `json:"proxy"`
	Group    groupSelection `json:"group"`
}

type wantDefaults struct {
	ContentMaxAge  int64 `json:"content_max_age"`
	MetadataMaxAge int64 `json:"metadata_max_age"`
	NotFoundCache  bool  `json:"not_found_cache"`
	NotFoundTTL    int64 `json:"not_found_ttl"`
	Blocked        bool  `json:"blocked"`
	AutoBlock      bool  `json:"auto_block"`
	Retries        int   `json:"retries"`
	TimeoutSeconds int   `json:"timeout_seconds"`
}

type vectorRepo struct {
	Name        string   `json:"name"`
	Format      string   `json:"format"`
	Type        string   `json:"type"`
	RemoteURL   *string  `json:"remote_url"`
	Members     []string `json:"members"`
	RoutingRule string   `json:"routing_rule"`
}

func (v vectorRepo) repo(attrs map[string]any) repos.Repo {
	return repos.Repo{Name: v.Name, Format: v.Format, Type: v.Type, Members: v.Members, Defined: true,
		RemoteURL: v.RemoteURL, Attributes: attrs, RoutingRule: v.RoutingRule}
}

type wantProxy struct {
	RemoteURL      string `json:"remote_url"`
	ContentMaxAge  int64  `json:"content_max_age"`
	MetadataMaxAge int64  `json:"metadata_max_age"`
	NotFoundCache  bool   `json:"not_found_cache"`
	NotFoundTTL    int64  `json:"not_found_ttl"`
	Blocked        bool   `json:"blocked"`
	AutoBlock      bool   `json:"auto_block"`
	Auth           struct {
		Type       string `json:"type"`
		Username   string `json:"username"`
		Preemptive bool   `json:"preemptive"`
		Built      bool   `json:"built"`
	} `json:"auth"`
	Retries         int    `json:"retries"`
	TimeoutSeconds  int    `json:"timeout_seconds"`
	UserAgentSuffix string `json:"user_agent_suffix"`
	RoutingRule     string `json:"routing_rule"`
}

type proxyVector struct {
	Name       string          `json:"name"`
	Repo       vectorRepo      `json:"repo"`
	Attributes json.RawMessage `json:"attributes"`
	Want       *wantProxy      `json:"want"`
	Error      string          `json:"error"`
}

type groupSelection struct {
	Repositories []vectorRepo `json:"repositories"`
	Cases        []struct {
		Name       string          `json:"name"`
		Group      vectorRepo      `json:"group"`
		Attributes json.RawMessage `json:"attributes"`
		Want       *struct {
			Members        []string `json:"members"`
			WritableMember string   `json:"writable_member"`
		} `json:"want"`
		Error string `json:"error"`
	} `json:"cases"`
}

func loadSettingsVectors(t *testing.T) settingsVectors {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/proxy_settings.json")
	if err != nil {
		t.Fatal(err)
	}
	var v settingsVectors
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// stored decodes an attribute document with json.Number, as state does;
// null and an absent document are no attributes.
func stored(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestProxySettingsVectors(t *testing.T) {
	v := loadSettingsVectors(t)
	if len(v.Proxy) < 40 {
		t.Fatalf("%d proxy vectors, want at least 40", len(v.Proxy))
	}
	for _, c := range v.Proxy {
		got, err := c.Repo.repo(stored(t, c.Attributes)).ProxySettings()
		if c.Error != "" {
			if err == nil || err.Error() != c.Error {
				t.Errorf("%s: error %v, want %q", c.Name, err, c.Error)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		w := c.Want
		want := repos.ProxySettings{RemoteURL: w.RemoteURL, ContentMaxAge: w.ContentMaxAge, MetadataMaxAge: w.MetadataMaxAge,
			NotFoundCache: w.NotFoundCache, NotFoundTTL: w.NotFoundTTL, Blocked: w.Blocked, AutoBlock: w.AutoBlock,
			Auth:    repos.RemoteAuth{Type: w.Auth.Type, Username: w.Auth.Username, Preemptive: w.Auth.Preemptive},
			Retries: w.Retries, TimeoutSeconds: w.TimeoutSeconds, UserAgentSuffix: w.UserAgentSuffix, RoutingRule: w.RoutingRule}
		if got != want {
			t.Errorf("%s:\n got %+v\nwant %+v", c.Name, got, want)
		}
		if got.Auth.Built() != w.Auth.Built {
			t.Errorf("%s: Built %v, want %v", c.Name, got.Auth.Built(), w.Auth.Built)
		}
	}
}

// The defaults of the vector file are the exported ones, and they are those
// of decision 5.4 and of the documentation the file cites.
func TestProxySettingsDefaults(t *testing.T) {
	d := loadSettingsVectors(t).Defaults
	got := wantDefaults{ContentMaxAge: repos.DefaultContentMaxAge, MetadataMaxAge: repos.DefaultMetadataMaxAge,
		NotFoundCache: true, NotFoundTTL: repos.DefaultNotFoundTTL, Blocked: false, AutoBlock: true,
		Retries: repos.DefaultRetries, TimeoutSeconds: repos.DefaultTimeoutSeconds}
	if d != got || d.ContentMaxAge != 1440 || d.NotFoundTTL != 1440 || d.Retries != 2 || d.TimeoutSeconds != 20 {
		t.Fatalf("defaults %+v, exported %+v", d, got)
	}
	none, err := repos.Repo{Name: "demo", Format: "raw", Type: "proxy", RemoteURL: ptr("http://files.example.com/")}.ProxySettings()
	if err != nil {
		t.Fatal(err)
	}
	if none.NotFoundCache != d.NotFoundCache || none.AutoBlock != d.AutoBlock || none.Blocked != d.Blocked {
		t.Fatalf("%+v", none)
	}
	if repos.AgeNever != -1 || repos.AgeAlways != 0 {
		t.Fatalf("AgeNever %d, AgeAlways %d", repos.AgeNever, repos.AgeAlways)
	}
}

func ptr(s string) *string { return &s }

// A settings error names repository and key, keeps the error of the number
// it wraps and never repeats the value, so no user part of an address can
// reach a log line.
func TestProxySettingsErrors(t *testing.T) {
	r := repos.Repo{Name: "demo-half", Format: "maven2", Type: "proxy", RemoteURL: ptr("https://repo.example.com/"),
		Attributes: map[string]any{"proxy": map[string]any{"contentMaxAge": json.Number("1440.5")}}}
	_, err := r.ProxySettings()
	var settings *repos.SettingsError
	if !errors.As(err, &settings) || settings.Repository != "demo-half" || settings.Key != "proxy.contentMaxAge" ||
		!errors.Is(err, repos.ErrFraction) {
		t.Fatalf("%#v", err)
	}
	r.Attributes = map[string]any{"proxy": map[string]any{"remoteUrl": "https://demo-user:CANARY-URL-SECRET-5a21@repo.example.com/"}}
	_, err = r.ProxySettings()
	if err == nil || strings.Contains(err.Error(), "CANARY") || strings.Contains(err.Error(), "demo-user") {
		t.Fatalf("an address with a user part: %v", err)
	}
	// numbers of an in memory document, not decoded with json.Number
	r.Attributes = map[string]any{"proxy": map[string]any{"contentMaxAge": float64(-1), "metadataMaxAge": 60}}
	got, err := r.ProxySettings()
	if err != nil || got.ContentMaxAge != -1 || got.MetadataMaxAge != 60 {
		t.Fatalf("%+v %v", got, err)
	}
	r.Attributes = map[string]any{"proxy": map[string]any{"contentMaxAge": 1.5}}
	if _, err := r.ProxySettings(); !errors.Is(err, repos.ErrFraction) {
		t.Fatalf("a float with a fraction: %v", err)
	}
}

// WithRoutingRules gives each repository the name of its rule, adds no
// repository and leaves the repositories it is called on as they are.
func TestWithRoutingRules(t *testing.T) {
	rs := repos.New([]repos.Repo{{Name: "demo-proxy", Format: "maven2", Type: "proxy", Defined: true,
		RemoteURL: ptr("https://repo.example.com/")}}, []string{"discovered"}, nil)
	with := rs.WithRoutingRules(map[string]string{"demo-proxy": "demo-block-internal", "elsewhere": "demo-other"})
	proxy, _ := with.Get("demo-proxy")
	if proxy.RoutingRule != "demo-block-internal" {
		t.Fatalf("%+v", proxy)
	}
	if s, err := proxy.ProxySettings(); err != nil || s.RoutingRule != "demo-block-internal" {
		t.Fatalf("%+v %v", s, err)
	}
	if _, ok := with.Get("elsewhere"); ok {
		t.Fatal("a link without a repository became one")
	}
	if before, _ := rs.Get("demo-proxy"); before.RoutingRule != "" {
		t.Fatal("WithRoutingRules changed the repositories it was called on")
	}
	if !reflect.DeepEqual(repoNames(with.All()), repoNames(rs.All())) {
		t.Fatalf("%v", repoNames(with.All()))
	}
	// the attributes and the links come together in either order
	both := rs.WithAttributes([]model.Repository{{Name: "demo-proxy", Attributes: map[string]any{"x": true}}}).
		WithRoutingRules(map[string]string{"demo-proxy": "demo-rule"})
	if p, _ := both.Get("demo-proxy"); p.RoutingRule != "demo-rule" || p.Attributes["x"] != true {
		t.Fatalf("%+v", p)
	}
}

func repoNames(list []repos.Repo) []string {
	out := []string{}
	for _, r := range list {
		out = append(out, r.Name)
	}
	return out
}

func TestGroupSettingsVectors(t *testing.T) {
	v := loadSettingsVectors(t).Group
	for _, c := range v.Cases {
		var defined []repos.Repo
		for _, r := range v.Repositories {
			defined = append(defined, r.repo(nil))
		}
		defined = append(defined, c.Group.repo(nil))
		rs := repos.New(defined, nil, nil)
		if attrs := stored(t, c.Attributes); attrs != nil {
			rs = rs.WithAttributes([]model.Repository{{Name: c.Group.Name, Attributes: attrs}})
		}
		got, err := rs.GroupSettings(c.Group.Name)
		if c.Error != "" {
			if err == nil || err.Error() != c.Error {
				t.Errorf("%s: error %v, want %q", c.Name, err, c.Error)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		if !reflect.DeepEqual(got.Members, c.Want.Members) || got.WritableMember != c.Want.WritableMember {
			t.Errorf("%s: %+v, want %+v", c.Name, got, *c.Want)
		}
	}
	if _, err := repos.New(nil, nil, nil).GroupSettings("demo-missing"); err == nil ||
		err.Error() != `repository "demo-missing" does not exist` {
		t.Fatalf("a missing group: %v", err)
	}
}

// The server owned attribute remote_public_only makes the client of the
// proxy refuse every closed address (ruling ST11).
func TestProxySettingsPublicOnly(t *testing.T) {
	base := map[string]any{"proxy": map[string]any{"remoteUrl": "https://203.0.113.10/raw/"}}
	for _, c := range []struct {
		value any
		want  bool
	}{{nil, false}, {true, true}, {false, false}, {"true", false}} {
		attrs := map[string]any{"proxy": base["proxy"]}
		if c.value != nil {
			attrs[repos.PublicOnlyKey] = c.value
		}
		s, err := repos.Repo{Name: "demo-p", Format: "raw", Type: "proxy", Attributes: attrs}.ProxySettings()
		if err != nil || s.PublicOnly != c.want {
			t.Errorf("%v: %+v %v", c.value, s.PublicOnly, err)
		}
	}
}
