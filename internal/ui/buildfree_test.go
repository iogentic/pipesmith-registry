package ui

import (
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// The interface has no build step (D4 of the phases overview): below web
// there is no file of a package manager or bundler and no node_modules.

// buildToolFiles are the files and folders of fsys that a package manager
// or a bundler leaves.
func buildToolFiles(fsys fs.FS) []string {
	var found []string
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && d.Name() == "node_modules":
			found = append(found, name)
			return fs.SkipDir
		case !d.IsDir() && (buildToolNames[d.Name()] || bundlerConfig.MatchString(d.Name())):
			found = append(found, name)
		}
		return nil
	})
	if err != nil {
		found = append(found, err.Error())
	}
	sort.Strings(found)
	return found
}

// buildToolNames are the manifests, lock files and settings of package
// managers and bundlers.
var buildToolNames = map[string]bool{
	"package.json": true, "package-lock.json": true, "npm-shrinkwrap.json": true, "yarn.lock": true,
	"pnpm-lock.yaml": true, "pnpm-workspace.yaml": true, "bun.lock": true, "bun.lockb": true, "deno.lock": true,
	".npmrc": true, ".yarnrc": true, ".yarnrc.yml": true, ".parcelrc": true, ".babelrc": true, ".babelrc.json": true,
	"turbo.json": true, "tsconfig.json": true, "jsconfig.json": true,
}

// bundlerConfig is the configuration file of a bundler or a task runner.
var bundlerConfig = regexp.MustCompile(`(?i)^((webpack|rollup|vite|esbuild|snowpack|parcel|babel|rspack|tsup|postcss|tailwind)\.config|gulpfile|gruntfile)\.([cm]?[jt]s|json)$`)

var (
	// hostAfterScheme is the authority of an address with a scheme, its
	// slashes plain or escaped as in JSON.
	hostAfterScheme = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*:(?:\\?/){2}([^\s/\\"'\x60<>()?#,;]*)`)
	// hostOfRelative is the authority of an address without a scheme, as
	// //host/path, in quotes, a template literal or a CSS url.
	hostOfRelative = regexp.MustCompile(`(?:^|[\s"'(=,\x60])(?:\\?/){2}([^\s/\\"'\x60<>()?#,;]*)`)
	hostName       = regexp.MustCompile(`(?i)^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*$`)
	ipv6Literal    = regexp.MustCompile(`^\[[0-9A-Fa-f:.]+\]$`)
)

// hostOf is the host of an authority: the user information left out, an
// IPv6 literal with its brackets, a name without its port. An address
// without a scheme names a host only with a dot in it or as an IPv6
// literal, so a comment such as //word is no host.
func hostOf(authority string, relative bool) (string, bool) {
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		authority = authority[i+1:]
	}
	if strings.HasPrefix(authority, "[") {
		end := strings.Index(authority, "]")
		if end < 0 || !ipv6Literal.MatchString(authority[:end+1]) {
			return "", false
		}
		return authority[:end+1], true
	}
	host, _, _ := strings.Cut(authority, ":")
	if !hostName.MatchString(host) || (relative && !strings.Contains(host, ".")) {
		return "", false
	}
	return host, true
}

// reservedHost tells whether host lies below a name reserved for examples
// and tests by RFC 2606.
func reservedHost(host string) bool {
	host = strings.ToLower(host)
	for _, name := range []string{"example.com", "example.net", "example.org"} {
		if host == name || strings.HasSuffix(host, "."+name) {
			return true
		}
	}
	for _, top := range []string{".test", ".example", ".invalid", ".localhost"} {
		if strings.HasSuffix(host, top) {
			return true
		}
	}
	return false
}

// otherHosts are the host names that a file of fsys names, as
// "<path>: <host>".
func otherHosts(fsys fs.FS) []string {
	var found []string
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, re := range []*regexp.Regexp{hostAfterScheme, hostOfRelative} {
			for _, m := range re.FindAllSubmatch(data, -1) {
				if host, ok := hostOf(string(m[1]), re == hostOfRelative); ok && !reservedHost(host) && !seen[host] {
					seen[host] = true
					found = append(found, path.Clean(name)+": "+host)
				}
			}
		}
		return nil
	})
	if err != nil {
		found = append(found, err.Error())
	}
	sort.Strings(found)
	return found
}

func walkedFiles(t *testing.T, fsys fs.FS) int {
	t.Helper()
	count := 0
	err := fs.WalkDir(fsys, ".", func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			count++
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return count
}

// The folder web on disk, dot files included, which embed leaves out.
func TestWebHoldsNoBuildTool(t *testing.T) {
	web := os.DirFS("web")
	if found := buildToolFiles(web); len(found) > 0 {
		t.Errorf("web holds files of a build step: %v", found)
	}
	embedded, err := fs.Sub(files, "web")
	if err != nil {
		t.Fatal(err)
	}
	if n, served := walkedFiles(t, web), walkedFiles(t, embedded); n == 0 || n != served {
		t.Errorf("the walk saw %d files of web, the binary embeds %d", n, served)
	} else {
		t.Logf("%d files of web, none of a build step", n)
	}
}

func TestBuildToolGuardFindsEachPlantedFile(t *testing.T) {
	planted := fstest.MapFS{
		"index.html":                        {Data: []byte("<!doctype html>")},
		"js/app.js":                         {Data: []byte("export {};")},
		"package.json":                      {Data: []byte("{}")},
		"js/package-lock.json":              {Data: []byte("{}")},
		"yarn.lock":                         {Data: []byte("")},
		"pnpm-lock.yaml":                    {Data: []byte("")},
		"vite.config.js":                    {Data: []byte("")},
		"css/webpack.config.mjs":            {Data: []byte("")},
		"rollup.config.ts":                  {Data: []byte("")},
		".parcelrc":                         {Data: []byte("{}")},
		"js/node_modules/left-pad/index.js": {Data: []byte("")},
		"babel.config.json":                 {Data: []byte("{}")},
		"js/.babelrc.json":                  {Data: []byte("{}")},
		"tsconfig.json":                     {Data: []byte("{}")},
		"js/jsconfig.json":                  {Data: []byte("{}")},
		"postcss.config.cjs":                {Data: []byte("")},
		"css/tailwind.config.js":            {Data: []byte("")},
	}
	want := []string{".parcelrc", "babel.config.json", "css/tailwind.config.js", "css/webpack.config.mjs",
		"js/.babelrc.json", "js/jsconfig.json", "js/node_modules", "js/package-lock.json", "package.json",
		"pnpm-lock.yaml", "postcss.config.cjs", "rollup.config.ts", "tsconfig.json", "vite.config.js", "yarn.lock"}
	if got := buildToolFiles(planted); !slices.Equal(got, want) {
		t.Errorf("found %v, want %v", got, want)
	}
	clean := fstest.MapFS{"index.html": {Data: []byte("x")}, "js/package.js": {Data: []byte("x")}, "js/config.js": {Data: []byte("x")}}
	if got := buildToolFiles(clean); len(got) > 0 {
		t.Errorf("found %v in a folder without a build step", got)
	}
}

// No file of web names a host: every request goes to the origin of the
// page. Host names below the names reserved for examples (RFC 2606) are
// placeholders of a form field and name no host.
func TestWebNamesNoOtherHost(t *testing.T) {
	if found := otherHosts(os.DirFS("web")); len(found) > 0 {
		t.Errorf("web names other hosts: %v", found)
	}
}

func TestHostGuardFindsEachPlantedHost(t *testing.T) {
	planted := fstest.MapFS{
		"index.html":   {Data: []byte(`<script src="https://cdn.jsdelivr.net/npm/x.js"></script><link href="//fonts.googleapis.com/css">`)},
		"css/base.css": {Data: []byte(`@import url(//cdn.other.org/a.css); body { background: url("http://203.0.113.9:8080/a.png"); }`)},
		"js/api.js":    {Data: []byte("fetch(`wss://events.other.net/live`); // a comment\nconst x = 'http://' + host;")},
		"js/form.js":   {Data: []byte(`placeholder: "https://repo.example.org/", note: "file:///<path>", re: /^https?:\/\/[^\s/]+/`)},
		"js/note.js":   {Data: []byte(`"starts with http:// or https://." // see https://docs.example.com/x`)},
		"assets/a.png": {Data: []byte("\x89PNG http://ns.other.com/xap/1.0/")},
		"js/local.js":  {Data: []byte(`"http://registry.test/" + "https://a.b.example/"`)},
		"js/user.js":   {Data: []byte(`const u = "https://a.example.com@other.net/x";`)},
		"js/v6.js":     {Data: []byte(`const u = "http://[2001:db8::1]:8080/x";`)},
		"js/tpl.js":    {Data: []byte("const u = `//cdn.tpl.org/${name}`; const v = `https://${host}/`;")},
		"js/esc.js":    {Data: []byte(`{"a": "https:\/\/cdn.escaped.org\/a.js", "b": "\/\/static.rel.org\/b.js", "c": s.replace(/\/\/+/g, "/")}`)},
	}
	want := []string{"assets/a.png: ns.other.com", "css/base.css: 203.0.113.9", "css/base.css: cdn.other.org",
		"index.html: cdn.jsdelivr.net", "index.html: fonts.googleapis.com", "js/api.js: events.other.net",
		"js/esc.js: cdn.escaped.org", "js/esc.js: static.rel.org", "js/tpl.js: cdn.tpl.org", "js/user.js: other.net",
		"js/v6.js: [2001:db8::1]"}
	if got := otherHosts(planted); !slices.Equal(got, want) {
		t.Errorf("found %v, want %v", got, want)
	}
}
