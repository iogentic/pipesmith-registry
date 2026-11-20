package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// Task 13 of the phase 5 plan, groups with proxy members. A file comes from
// the first member that has it, in member order, across hosted and proxy
// members and a group inside a group; "has it" for a proxy member is a
// cached copy that is fresh, or one fetched now. A member that a routing
// rule blocks, that is not ready or that failed is skipped (decision 5.9),
// and its remote sees no request for a blocked path. The routing rule of a
// proxy member holds for every read through a group, its cache included
// (finding of the security assessment of 2026-11-19 on 11df359). Merged
// maven-metadata.xml and merged npm documents hold the versions of every
// member, the proxy members asked through their freshness rules, at most 4
// at once. Decision 5.19 holds through a group in both positions of the
// switch.

// metadataOf is a maven-metadata.xml of com.example:demo-lib with versions.
func metadataOf(versions ...string) string {
	var b strings.Builder
	for _, v := range versions {
		b.WriteString("<version>" + v + "</version>")
	}
	return "<metadata><groupId>com.example</groupId><artifactId>demo-lib</artifactId><versioning><versions>" + b.String() +
		"</versions></versioning></metadata>\n"
}

const groupMetadataPath = "com/example/demo-lib/maven-metadata.xml"

func TestGroupFileComesFromTheFirstMemberThatHasIt(t *testing.T) {
	hosted := func(path, data string) testkit.BlobSpec { return spec("demo-raw", path, []byte(data), "text/plain") }
	u := startUpstream(t, upstreamSetup{upstream: true,
		proxies: []proxyDef{{name: "demo-raw-a", format: "raw"}, {name: "demo-raw-b", format: "raw"},
			{name: "demo-raw-off", format: "raw", attributes: map[string]any{"httpclient": map[string]any{"blocked": true}}}},
		repos: []repoDef{
			{name: "demo-raw-inner", format: "raw", kind: "group", members: []string{"demo-raw-b"}},
			{name: "demo-raw-group", format: "raw", kind: "group", members: []string{"demo-raw-off", "demo-raw-a", "demo-raw", "demo-raw-inner"}},
		},
		blobs: []testkit.BlobSpec{hosted("docs/both.txt", "hosted bytes"), hosted("docs/hosted.txt", "hosted only"),
			hosted("docs/failing.txt", "hosted when a fails")}})
	a, b, off := remoteBase("demo-raw-a"), remoteBase("demo-raw-b"), remoteBase("demo-raw-off")
	u.remote.Script(a+"docs/both.txt", testkit.OK("bytes of a"))
	u.remote.Script(a+"docs/failing.txt", testkit.ServerError())
	u.remote.Script(a+"docs/down.txt", testkit.ServerError())
	u.remote.Script(b+"docs/inner.txt", testkit.OK("bytes of b"))
	u.remote.Script(off+"docs/both.txt", testkit.OK("bytes of a blocked proxy"))
	for _, c := range []struct {
		path, want string
		status     int
	}{
		{"docs/both.txt", "bytes of a", 200},
		{"docs/hosted.txt", "hosted only", 200},
		{"docs/inner.txt", "bytes of b", 200},
		{"docs/failing.txt", "hosted when a fails", 200},
		{"docs/down.txt", "Not found\n", 404},
		{"docs/nowhere.txt", "Not found\n", 404},
	} {
		r := u.get("demo-raw-group", c.path)
		if r.status != c.status || string(r.body) != c.want {
			t.Errorf("%s: %d %q, want %d %q", c.path, r.status, r.body, c.status, c.want)
		}
	}
	for _, c := range []struct {
		path string
		want int
	}{
		{a + "docs/both.txt", 1}, {a + "docs/hosted.txt", 1}, {a + "docs/inner.txt", 1}, {a + "docs/failing.txt", 1},
		{a + "docs/down.txt", 1}, {b + "docs/inner.txt", 1}, {b + "docs/hosted.txt", 0}, {b + "docs/down.txt", 1},
		{off + "docs/both.txt", 0},
	} {
		if n := u.remote.Count(c.path); n != c.want {
			t.Errorf("%s: %d requests at the remote, want %d", c.path, n, c.want)
		}
	}
	// the copy of a stays in its cache, and the next read asks nothing
	u.remote.Reset()
	if r := u.get("demo-raw-group", "docs/both.txt"); r.status != 200 || string(r.body) != "bytes of a" {
		t.Fatalf("again: %d %q", r.status, r.body)
	}
	if row := u.row("demo-raw-a", "docs/both.txt"); row == nil || row.Store != "local-write" {
		t.Fatalf("the fetch through the group is not in the cache of its member: %+v", row)
	}
	if n := len(u.remote.Requests()); n != 0 {
		t.Fatalf("%d requests for a fresh copy: %+v", n, u.remote.Requests())
	}
}

// The routing rule of a proxy member holds for every read through a group,
// with upstream on and off: a path it blocks is never served from the cache
// of that member, for files, merged maven-metadata.xml, npm documents and
// tarballs, and Docker manifests, blobs and tag lists; the next member
// answers, and the remote of the blocked member sees no request.
func TestGroupAppliesTheRoutingRuleOfAProxyMember(t *testing.T) {
	image := dockerImage("demo/blocked", "1.0", []byte("a blocked layer"))
	npmData := tarballOf(t, "@demo-blocked/pkg", "1.0.0")
	npmDoc := npmDocument(t, "@demo-blocked/pkg",
		npmVersion{version: "1.0.0", tarball: "https://registry.example.test/@demo-blocked/pkg/-/pkg-1.0.0.tgz", data: npmData})
	cached := append(blobsOf("demo-docker-proxy", image),
		spec("demo-raw-proxy", "docs/blocked/a.txt", []byte("cached under a blocked path"), "text/plain"),
		spec("demo-raw-proxy", "docs/blocked/only.txt", []byte("only in the blocked cache"), "text/plain"),
		spec("demo-raw", "docs/blocked/a.txt", []byte("bytes of the hosted member"), "text/plain"),
		spec("demo-maven-proxy", "com/example/blocked/maven-metadata.xml", []byte(metadataOf("9.9")), "application/xml"),
		spec("demo-maven", "com/example/blocked/maven-metadata.xml", []byte(metadataOf("1.0")), "application/xml"),
		spec("demo-npm-proxy", "@demo-blocked/pkg", npmDoc, "application/json"),
		spec("demo-npm-proxy", "@demo-blocked/pkg/-/pkg-1.0.0.tgz", npmData, "application/x-tgz"))
	routing := &model.Routing{
		Rules: []model.RoutingRule{{Name: "demo-block-files", Mode: "BLOCK", Matchers: []string{"^/docs/blocked/.*"}},
			{Name: "demo-block-maven", Mode: "BLOCK", Matchers: []string{"^/com/example/blocked/.*"}},
			{Name: "demo-block-npm", Mode: "BLOCK", Matchers: []string{"^/@demo-blocked/.*"}},
			{Name: "demo-block-docker", Mode: "BLOCK", Matchers: []string{"^/v2/demo/blocked/.*"}}},
		Links: map[string]string{"demo-raw-proxy": "demo-block-files", "demo-maven-proxy": "demo-block-maven",
			"demo-npm-proxy": "demo-block-npm", "demo-docker-proxy": "demo-block-docker"},
	}
	_, digest, blobs := imageParts(image, "demo/blocked", "1.0")
	for _, upstream := range []bool{false, true} {
		u := startUpstream(t, upstreamSetup{upstream: upstream, blobs: cached,
			proxies: []proxyDef{{name: "demo-raw-proxy", format: "raw"}, {name: "demo-maven-proxy", format: "maven2"},
				{name: "demo-npm-proxy", format: "npm"}, {name: "demo-docker-proxy", format: "docker"}},
			repos: []repoDef{
				{name: "demo-npm", format: "npm", kind: "hosted"}, {name: "demo-docker", format: "docker", kind: "hosted"},
				{name: "demo-raw-group", format: "raw", kind: "group", members: []string{"demo-raw-proxy", "demo-raw"}},
				{name: "demo-maven-group", format: "maven2", kind: "group", members: []string{"demo-maven-proxy", "demo-maven"}},
				{name: "demo-npm-group", format: "npm", kind: "group", members: []string{"demo-npm-proxy", "demo-npm"}},
				{name: "demo-docker-group", format: "docker", kind: "group", members: []string{"demo-docker-proxy", "demo-docker"}},
			},
			tune: func(o *appOptions) { o.routing = routing }})
		for _, c := range []struct {
			repo, path string
			status     int
			not        string // bytes of the blocked cache, which the answer must not hold
		}{
			{"demo-raw-group", "docs/blocked/a.txt", 200, "cached under a blocked path"},
			{"demo-raw-group", "docs/blocked/only.txt", 404, "only in the blocked cache"},
			{"demo-maven-group", "com/example/blocked/maven-metadata.xml", 200, "9.9"},
			{"demo-npm-group", "@demo-blocked/pkg", 404, `"1.0.0"`},
			{"demo-npm-group", "@demo-blocked/pkg/-/pkg-1.0.0.tgz", 404, string(npmData)},
			{"demo-docker-group", "v2/demo/blocked/manifests/1.0", 404, "schemaVersion"},
			{"demo-docker-group", "v2/demo/blocked/manifests/" + digest, 404, "schemaVersion"},
			{"demo-docker-group", "v2/demo/blocked/blobs/" + blobs[0], 404, string(image[dockerBlobPrefix+blobs[0]].data)},
			{"demo-docker-group", "v2/demo/blocked/tags/list", 404, `"1.0"`},
		} {
			r := u.get(c.repo, c.path)
			if r.status != c.status || strings.Contains(string(r.body), c.not) {
				t.Errorf("upstream %v, %s/%s: %d %q", upstream, c.repo, c.path, r.status, r.body)
			}
		}
		if r := u.get("demo-raw-group", "docs/blocked/a.txt"); string(r.body) != "bytes of the hosted member" {
			t.Errorf("upstream %v: the next member does not answer: %q", upstream, r.body)
		}
		if r := u.get("demo-maven-group", "com/example/blocked/maven-metadata.xml"); !strings.Contains(string(r.body), "<version>1.0</version>") {
			t.Errorf("upstream %v: the merge lacks the hosted member: %q", upstream, r.body)
		}
		if n := len(u.remote.Requests()); n != 0 {
			t.Errorf("upstream %v: %d requests at the remotes of blocked members: %+v", upstream, n, u.remote.Requests())
		}
	}
}

// Merged maven-metadata.xml and merged npm documents hold the versions of
// every member: a hosted member, and proxy members asked through their
// freshness rules; a second read within the metadata age asks nothing.
func TestGroupMergesTheVersionsOfEveryMember(t *testing.T) {
	widget := tarballOf(t, "widget", "1.0.0")
	u := startUpstream(t, upstreamSetup{upstream: true,
		proxies: []proxyDef{{name: "demo-maven-a", format: "maven2"}, {name: "demo-maven-b", format: "maven2"},
			{name: "demo-npm-a", format: "npm"}},
		repos: []repoDef{
			{name: "demo-npm", format: "npm", kind: "hosted"},
			{name: "demo-maven-group", format: "maven2", kind: "group", members: []string{"demo-maven", "demo-maven-a", "demo-maven-b"}},
			{name: "demo-npm-group", format: "npm", kind: "group", members: []string{"demo-npm", "demo-npm-a"}},
		},
		blobs: []testkit.BlobSpec{spec("demo-maven", groupMetadataPath, []byte(metadataOf("1.0")), "application/xml"),
			spec("demo-npm", "widget", npmDocument(t, "widget", npmVersion{version: "1.0.0", tarball: "https://registry.example.test/widget/-/widget-1.0.0.tgz", data: widget}), "application/json"),
			spec("demo-npm", "widget/-/widget-1.0.0.tgz", widget, "application/x-tgz")}})
	u.remote.Script(remoteBase("demo-maven-a")+groupMetadataPath, testkit.OK(metadataOf("2.0")))
	u.remote.Script(remoteBase("demo-maven-b")+groupMetadataPath, testkit.OK(metadataOf("3.0")))
	u.remote.Script(remoteBase("demo-npm-a")+"widget", testkit.OK(string(npmDocument(t, "widget",
		npmVersion{version: "2.0.0", tarball: "https://registry.example.test/widget/-/widget-2.0.0.tgz", data: []byte("two")}))))
	for i := 0; i < 2; i++ {
		r := u.get("demo-maven-group", groupMetadataPath)
		for _, v := range []string{"1.0", "2.0", "3.0"} {
			if r.status != 200 || !strings.Contains(string(r.body), "<version>"+v+"</version>") {
				t.Fatalf("read %d: no version %s in %d %q", i+1, v, r.status, r.body)
			}
		}
		if sha1 := u.get("demo-maven-group", groupMetadataPath+".sha1"); sha1.status != 200 || string(sha1.body) != sha1Of(string(r.body)) {
			t.Fatalf("read %d: the .sha1 of the merge: %d %q", i+1, sha1.status, sha1.body)
		}
		document := u.get("demo-npm-group", "widget")
		if versions, _ := npmVersionsOf(t, document); !slices.Equal(versions, []string{"1.0.0", "2.0.0"}) {
			t.Fatalf("read %d: npm versions %v", i+1, versions)
		}
	}
	for _, path := range []string{remoteBase("demo-maven-a") + groupMetadataPath, remoteBase("demo-maven-b") + groupMetadataPath,
		remoteBase("demo-npm-a") + "widget"} {
		if n := u.remote.Count(path); n != 1 {
			t.Errorf("%s: %d requests, want 1 within the metadata age", path, n)
		}
	}
}

// At most 4 members are asked at once for a merged document: 6 proxy
// members whose remote takes 300 ms to answer see at most 4 requests in
// flight, and every one is asked. The test runs three times.
func TestGroupAsksAtMostFourMembersAtOnce(t *testing.T) {
	for run := 1; run <= 3; run++ {
		var proxies []proxyDef
		var maven, npm []string
		for i := 1; i <= 6; i++ {
			proxies = append(proxies, proxyDef{name: fmt.Sprintf("demo-maven-%d", i), format: "maven2"},
				proxyDef{name: fmt.Sprintf("demo-npm-%d", i), format: "npm"})
			maven, npm = append(maven, fmt.Sprintf("demo-maven-%d", i)), append(npm, fmt.Sprintf("demo-npm-%d", i))
		}
		u := startUpstream(t, upstreamSetup{upstream: true, proxies: proxies, repos: []repoDef{
			{name: "demo-maven-group", format: "maven2", kind: "group", members: maven},
			{name: "demo-npm-group", format: "npm", kind: "group", members: npm}}})
		for i := 1; i <= 6; i++ {
			version := fmt.Sprintf("%d.0", i)
			u.remote.Script(remoteBase(maven[i-1])+groupMetadataPath, testkit.SlowHeader(300*time.Millisecond, testkit.OK(metadataOf(version))))
			u.remote.Script(remoteBase(npm[i-1])+"widget", testkit.SlowHeader(300*time.Millisecond, testkit.OK(string(npmDocument(t, "widget",
				npmVersion{version: version + ".0", tarball: "https://registry.example.test/widget/-/widget-" + version + ".0.tgz", data: []byte(version)})))))
		}
		r := u.get("demo-maven-group", groupMetadataPath)
		for i := 1; i <= 6; i++ {
			if !strings.Contains(string(r.body), fmt.Sprintf("<version>%d.0</version>", i)) {
				t.Fatalf("run %d: version %d.0 is not in the merge: %d %q", run, i, r.status, r.body)
			}
		}
		if most := u.remote.MaxInFlight(); most != 4 {
			t.Fatalf("run %d: %d requests in flight at most for the maven group, want 4", run, most)
		}
		u.remote.Reset()
		if versions, _ := npmVersionsOf(t, u.get("demo-npm-group", "widget")); len(versions) != 6 {
			t.Fatalf("run %d: npm versions %v", run, versions)
		}
		if most := u.remote.MaxInFlight(); most != 4 {
			t.Fatalf("run %d: %d requests in flight at most for the npm group, want 4", run, most)
		}
		t.Logf("run %d: at most 4 of 6 members in flight for maven and for npm", run)
	}
}

// Decision 5.19 through a group, in both positions of the switch: the cached
// document of the proxy member names two versions and the cache holds the
// tarball of one. With upstream off the rule of the port holds; with
// upstream on both versions stay, and the missing tarball comes from the
// remote of the member with the digests of its document.
func TestGroupNpmVersionsWithoutATarball(t *testing.T) {
	one, two := tarballOf(t, "widget", "1.0.0"), tarballOf(t, "widget", "2.0.0")
	document := npmDocument(t, "widget",
		npmVersion{version: "1.0.0", tarball: "https://registry.example.test/widget/-/widget-1.0.0.tgz", data: one},
		npmVersion{version: "2.0.0", tarball: "https://registry.example.test/widget/-/widget-2.0.0.tgz", data: two})
	cached := []testkit.BlobSpec{spec("demo-npm-proxy", "widget", document, "application/json"),
		spec("demo-npm-proxy", "widget/-/widget-1.0.0.tgz", one, "application/x-tgz")}
	for _, c := range []struct {
		upstream bool
		want     []string
	}{{false, []string{"1.0.0"}}, {true, []string{"1.0.0", "2.0.0"}}} {
		u := startUpstream(t, upstreamSetup{upstream: c.upstream, blobs: cached, proxies: []proxyDef{{name: "demo-npm-proxy", format: "npm"}},
			repos: []repoDef{{name: "demo-npm", format: "npm", kind: "hosted"},
				{name: "demo-npm-group", format: "npm", kind: "group", members: []string{"demo-npm", "demo-npm-proxy"}}}})
		u.remote.Script(remoteBase("demo-npm-proxy")+"widget", testkit.NotModified())
		u.remote.Script(remoteBase("demo-npm-proxy")+"widget/-/widget-2.0.0.tgz", testkit.OK(string(two)))
		r := u.get("demo-npm-group", "widget")
		if versions, tarballs := npmVersionsOf(t, r); !slices.Equal(versions, c.want) ||
			tarballs["1.0.0"] != "http://"+u.addr()+"/repository/demo-npm-group/widget/-/widget-1.0.0.tgz" {
			t.Errorf("upstream %v: versions %v, tarballs %v", c.upstream, versions, tarballs)
		}
		got := u.get("demo-npm-group", "widget/-/widget-2.0.0.tgz")
		if c.upstream && (got.status != 200 || string(got.body) != string(two)) || !c.upstream && got.status != 404 {
			t.Errorf("upstream %v: the tarball of 2.0.0 through the group: %d", c.upstream, got.status)
		}
	}
}

// Docker through a group with a proxy member: a manifest, a blob and the tag
// list of an image only the remote of the member has, beside an image of
// the hosted member.
func TestGroupDockerWithAProxyMember(t *testing.T) {
	stored := dockerImage("demo/hosted", "1.0", []byte("a hosted layer"))
	u := startUpstream(t, upstreamSetup{upstream: true, blobs: blobsOf("demo-docker", stored),
		proxies: []proxyDef{{name: "demo-docker-proxy", format: "docker"}},
		repos: []repoDef{{name: "demo-docker", format: "docker", kind: "hosted"},
			{name: "demo-docker-group", format: "docker", kind: "group", members: []string{"demo-docker", "demo-docker-proxy"}}}})
	remote := dockerImage("demo/app", "2.0", []byte("a layer of the remote"))
	remoteImage(u, "demo-docker-proxy", "demo/app", remote, nil)
	u.remote.Script(remoteBase("demo-docker-proxy")+"v2/demo/app/tags/list", testkit.OK(`{"name":"demo/app","tags":["2.0","2.1"]}`))
	manifest, _, blobs := imageParts(remote, "demo/app", "2.0")
	if r := u.get("demo-docker-group", "v2/demo/app/manifests/2.0"); r.status != 200 || string(r.body) != string(manifest) {
		t.Fatalf("manifest through the group: %d %q", r.status, r.body)
	}
	for _, blob := range blobs {
		if r := u.get("demo-docker-group", "v2/demo/app/blobs/"+blob); r.status != 200 || string(r.body) != string(remote[dockerBlobPrefix+blob].data) {
			t.Fatalf("blob %s through the group: %d", blob, r.status)
		}
	}
	tags := u.get("demo-docker-group", "v2/demo/app/tags/list")
	var v struct{ Tags []string }
	if json.Unmarshal(tags.body, &v) != nil || !slices.Equal(v.Tags, []string{"2.0", "2.1"}) {
		t.Fatalf("tag list through the group: %d %q", tags.status, tags.body)
	}
	if r := u.get("demo-docker-group", "v2/demo/hosted/manifests/1.0"); r.status != 200 {
		t.Fatalf("the image of the hosted member: %d %q", r.status, r.body)
	}
	if n := u.remote.Count(remoteBase("demo-docker-proxy") + "v2/demo/hosted/manifests/1.0"); n != 0 {
		t.Fatalf("the hosted member had the manifest, the remote of the proxy member was asked %d times", n)
	}
	if r := u.get("demo-docker-group", "v2/demo/none/manifests/1.0"); r.status != http.StatusNotFound {
		t.Fatalf("a manifest nobody has: %d %q", r.status, r.body)
	}
}
