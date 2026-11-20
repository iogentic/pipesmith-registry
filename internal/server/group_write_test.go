package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Task 13 of the phase 5 plan, the write to a group (decision 5.16). A group
// may name one writable member (group.groupWriteMember). A write to the
// group then runs the write handler of phase 4 with that member as target;
// without the attribute the refusal of phase 4 stays, byte for byte (the
// vectors refusals_raw_maven2.json and refusals_npm_docker.json). The
// permission checked is that of the group, and its action follows the path
// in the member: add for a new path, edit for a path the member holds,
// delete for a delete. Every address an answer names carries the group, so
// a client that wrote through the group stays on it.

// The users of these tests: grouper holds every action on the groups and
// none on the members, adder holds add and read on the groups, editor edit
// and read without add, and member holds every action on the members and
// none on the groups.
const (
	asGrouper  = "demo-grouper:grouper-pw"
	asAdderOf  = "demo-group-adder:adder-pw"
	asEditorOf = "demo-group-editor:editor-pw"
	asMember   = "demo-member:member-pw"
)

// groupWrites is a server with writes on, a hosted target and a second
// hosted member per format, a group of each with its writable member, and a
// raw group without one.
func groupWrites(t *testing.T) *upstreamApp {
	t.Helper()
	writable := func(name, format string, members []string, target string) repoDef {
		return repoDef{name: name, format: format, kind: "group", members: members,
			attributes: map[string]any{"group": map[string]any{"memberNames": toAny(members), "groupWriteMember": target}}}
	}
	var repos []repoDef
	for _, format := range []string{"raw", "maven2", "npm", "docker"} {
		short := map[string]string{"maven2": "maven"}[format]
		if short == "" {
			short = format
		}
		target, second := "demo-"+short+"-target", "demo-"+short+"-second"
		repos = append(repos, repoDef{name: target, format: format, kind: "hosted"}, repoDef{name: second, format: format, kind: "hosted"},
			writable("demo-"+short+"-group", format, []string{second, target}, target))
	}
	repos = append(repos, repoDef{name: "demo-raw-plain", format: "raw", kind: "group", members: []string{"demo-raw-target"}})
	u := startUpstream(t, upstreamSetup{writes: true, repos: repos})
	var groups, members []string
	for _, short := range []string{"raw", "maven", "npm", "docker"} {
		groups = append(groups, "demo-"+short+"-group")
		members = append(members, "demo-"+short+"-target", "demo-"+short+"-second")
	}
	u.sec.addBasic(asGrouper, subject("demo-grouper", "", grants(groups, "add,edit,delete,read,browse")...))
	u.sec.addBasic(asAdderOf, subject("demo-group-adder", "", grants(groups, "add,read,browse")...))
	u.sec.addBasic(asEditorOf, subject("demo-group-editor", "", grants(groups, "edit,read,browse")...))
	u.sec.addBasic(asMember, subject("demo-member", "", grants(members, "add,edit,delete,read,browse")...))
	return u
}

func toAny(names []string) []any {
	out := make([]any, len(names))
	for i, n := range names {
		out[i] = n
	}
	return out
}

// grants are view permissions of the repositories with the actions given.
func grants(repos []string, actions string) []string {
	var out []string
	for _, r := range repos {
		out = append(out, "nexus:repository-view:*:"+r+":"+actions)
	}
	return out
}

func (u *upstreamApp) send(method, target string, body []byte, user string, headers ...string) *answer {
	u.t.Helper()
	return do(u.t, u.addr(), method, target, body, append([]string{"basic", user}, headers...)...)
}

// raw and maven2: a write through the group lands in its writable member
// only and is readable through the group; the action follows the path in
// the member; the user of the group is refused on the member, the user of
// the member on the group.
func TestGroupWriteGoesToItsWritableMember(t *testing.T) {
	u := groupWrites(t)
	for _, c := range []struct{ group, target, second, path string }{
		{"demo-raw-group", "demo-raw-target", "demo-raw-second", "docs/through-the-group.txt"},
		{"demo-maven-group", "demo-maven-target", "demo-maven-second", "com/example/demo-lib/2.0.0/demo-lib-2.0.0.jar"},
	} {
		body := []byte("written through " + c.group)
		if r := u.send("PUT", "/repository/"+c.group+"/"+c.path, body, asGrouper); r.status != http.StatusCreated {
			t.Fatalf("%s: PUT through the group: %d %q", c.group, r.status, r.body)
		}
		if u.row(c.target, c.path) == nil || u.row(c.second, c.path) != nil || u.row(c.group, c.path) != nil {
			t.Fatalf("%s: rows target %+v, second %+v, group %+v", c.group, u.row(c.target, c.path), u.row(c.second, c.path), u.row(c.group, c.path))
		}
		if r := u.send("GET", "/repository/"+c.group+"/"+c.path, nil, asGrouper); r.status != 200 || string(r.body) != string(body) {
			t.Fatalf("%s: read through the group: %d %q", c.group, r.status, r.body)
		}
		// the path is the member's now: a second write asks edit of the group
		if r := u.send("PUT", "/repository/"+c.group+"/"+c.path, body, asAdderOf); r.status != http.StatusForbidden {
			t.Fatalf("%s: a second write with add only: %d %q", c.group, r.status, r.body)
		}
		if r := u.send("PUT", "/repository/"+c.group+"/"+c.path+".new", body, asAdderOf); r.status != http.StatusCreated {
			t.Fatalf("%s: a new path with add only: %d %q", c.group, r.status, r.body)
		}
		// edit without add overwrites the path the member holds, and the user of the group keeps its edit right under the lock
		for _, user := range []string{asEditorOf, asGrouper} {
			again := []byte("written again by " + user)
			if r := u.send("PUT", "/repository/"+c.group+"/"+c.path, again, user); r.status != http.StatusCreated {
				t.Fatalf("%s: an overwrite by %s: %d %q", c.group, user, r.status, r.body)
			}
			if r := u.send("GET", "/repository/"+c.group+"/"+c.path, nil, asGrouper); string(r.body) != string(again) {
				t.Fatalf("%s: the overwrite by %s reads %q", c.group, user, r.body)
			}
		}
		if r := u.send("PUT", "/repository/"+c.target+"/"+c.path+".direct", body, asGrouper); r.status != http.StatusForbidden {
			t.Fatalf("%s: the user of the group on the member: %d %q", c.group, r.status, r.body)
		}
		if r := u.send("PUT", "/repository/"+c.group+"/"+c.path+".member", body, asMember); r.status != http.StatusForbidden {
			t.Fatalf("%s: the user of the member on the group: %d %q", c.group, r.status, r.body)
		}
		if r := u.send("DELETE", "/repository/"+c.group+"/"+c.path, nil, asAdderOf); r.status != http.StatusForbidden {
			t.Fatalf("%s: a delete with add only: %d %q", c.group, r.status, r.body)
		}
		if r := u.send("DELETE", "/repository/"+c.group+"/"+c.path, nil, asGrouper); r.status != http.StatusNoContent {
			t.Fatalf("%s: a delete through the group: %d %q", c.group, r.status, r.body)
		}
		if u.row(c.target, c.path) != nil {
			t.Fatalf("%s: the delete through the group left the row of the member", c.group)
		}
	}
}

// npm: a publish through the group lands in the writable member, and the
// tarball addresses of the document read through the group name the group.
// docker: every Location of an upload session and of a manifest carries the
// group, and the client that follows it stays on the group.
func TestGroupWriteNpmAndDockerStayOnTheGroup(t *testing.T) {
	u := groupWrites(t)
	data := npmTarballOf(t, "package through the group")
	if r := u.send("PUT", "/repository/demo-npm-group/@demo%2fwidget", npmPublishBody("@demo/widget", "1.0.0", data), asGrouper); r.status != http.StatusCreated {
		t.Fatalf("publish through the group: %d %q", r.status, r.body)
	}
	if u.row("demo-npm-target", "@demo/widget") == nil || u.row("demo-npm-target", "@demo/widget/-/widget-1.0.0.tgz") == nil ||
		u.row("demo-npm-second", "@demo/widget") != nil {
		t.Fatal("the publish did not land in the writable member alone")
	}
	document := u.send("GET", "/repository/demo-npm-group/@demo%2fwidget", nil, asGrouper)
	if !strings.Contains(string(document.body), "/repository/demo-npm-group/@demo/widget/-/widget-1.0.0.tgz") ||
		strings.Contains(string(document.body), "demo-npm-target") {
		t.Fatalf("the document through the group: %d %s", document.status, document.body)
	}
	if r := u.send("GET", "/repository/demo-npm-group/@demo/widget/-/widget-1.0.0.tgz", nil, asGrouper); r.status != 200 || !bytes.Equal(r.body, data) {
		t.Fatalf("the tarball through the group: %d", r.status)
	}

	layer, config := []byte("a layer pushed through the group"), []byte(`{"architecture":"amd64","os":"linux"}`)
	push := func(blob []byte) {
		start := u.send("POST", "/repository/demo-docker-group/v2/demo/app/blobs/uploads/", nil, asGrouper)
		location := start.header.Get("Location")
		if start.status != http.StatusAccepted || !strings.HasPrefix(location, "/repository/demo-docker-group/v2/demo/app/blobs/uploads/") {
			t.Fatalf("start of a session: %d %v", start.status, start.header)
		}
		status := u.send("GET", location, nil, asGrouper)
		if status.status != http.StatusNoContent || !strings.HasPrefix(status.header.Get("Location"), "/repository/demo-docker-group/") {
			t.Fatalf("the state of the session through the group: %d %v", status.status, status.header)
		}
		end := u.send("PUT", location+"?digest="+digestOf(blob), blob, asGrouper, "Content-Type", "application/octet-stream")
		if end.status != http.StatusCreated || end.header.Get("Location") != "/repository/demo-docker-group/v2/demo/app/blobs/"+digestOf(blob) {
			t.Fatalf("end of a session: %d %v %q", end.status, end.header, end.body)
		}
	}
	push(config)
	push(layer)
	manifest, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": dockerManifestType,
		"config": map[string]any{"mediaType": dockerConfigType, "size": len(config), "digest": digestOf(config)},
		"layers": []any{map[string]any{"mediaType": dockerLayerType, "size": len(layer), "digest": digestOf(layer)}}})
	put := u.send("PUT", "/repository/demo-docker-group/v2/demo/app/manifests/1.0", manifest, asGrouper, "Content-Type", dockerManifestType)
	if put.status != http.StatusCreated || put.header.Get("Location") != "/repository/demo-docker-group/v2/demo/app/manifests/"+digestOf(manifest) {
		t.Fatalf("manifest through the group: %d %v %q", put.status, put.header, put.body)
	}
	if u.row("demo-docker-target", "v2/demo/app/manifests/1.0") == nil || u.row("demo-docker-second", "v2/demo/app/manifests/1.0") != nil {
		t.Fatal("the manifest did not land in the writable member alone")
	}
	if r := u.send("GET", "/repository/demo-docker-group/v2/demo/app/manifests/1.0", nil, asGrouper); r.status != 200 || !bytes.Equal(r.body, manifest) {
		t.Fatalf("the manifest through the group: %d", r.status)
	}
}

// Without a writable member a write to a group keeps the refusal of phase
// 4, the same as to a group of no attributes; the REST upload to a group
// keeps its 400.
func TestGroupWriteWithoutAWritableMember(t *testing.T) {
	u := groupWrites(t)
	plain := u.send("PUT", "/repository/demo-raw-plain/docs/a.txt", []byte("a"), asAdmin)
	if plain.status != http.StatusMethodNotAllowed || string(plain.body) != "Method not allowed: this server is read only\n" ||
		plain.header.Get("Allow") != "GET, HEAD" {
		t.Fatalf("a group without a writable member: %d %v %q", plain.status, plain.header, plain.body)
	}
	if u.row("demo-raw-target", "docs/a.txt") != nil {
		t.Fatal("the refused write landed in a member")
	}
	r := u.send("POST", "/service/rest/v1/components?repository=demo-raw-group", []byte("--x--\r\n"), asAdmin,
		"Content-Type", "multipart/form-data; boundary=x")
	if r.status != http.StatusBadRequest {
		t.Fatalf("the REST upload to a group: %d %q", r.status, r.body)
	}
}

// npm and docker: the user of the group is refused on the member, and a
// user without the permission of the group is refused on the group; neither
// refusal leaves a row in a member.
func TestGroupWriteNpmAndDockerRefusals(t *testing.T) {
	u := groupWrites(t)
	data := npmTarballOf(t, "refused package")
	for _, c := range []struct{ repo, user, what string }{
		{"demo-npm-target", asGrouper, "the user of the group on the member"},
		{"demo-npm-group", asMember, "the user of the member on the group"},
	} {
		if r := u.send("PUT", "/repository/"+c.repo+"/@demo%2frefused", npmPublishBody("@demo/refused", "1.0.0", data), c.user); r.status != http.StatusForbidden {
			t.Errorf("npm, %s: %d %q", c.what, r.status, r.body)
		}
	}
	for _, member := range []string{"demo-npm-target", "demo-npm-second"} {
		if u.row(member, "@demo/refused") != nil {
			t.Errorf("npm: a refused publish left a row in %s", member)
		}
	}
	blob := []byte("a refused layer")
	for _, c := range []struct{ repo, user, what string }{
		{"demo-docker-target", asGrouper, "the user of the group on the member"},
		{"demo-docker-group", asMember, "the user of the member on the group"},
	} {
		start := u.send("POST", "/repository/"+c.repo+"/v2/demo/refused/blobs/uploads/", nil, c.user)
		if start.status != http.StatusForbidden || start.header.Get("Location") != "" {
			t.Errorf("docker, start of a session, %s: %d %v %q", c.what, start.status, start.header, start.body)
		}
		whole := u.send("POST", "/repository/"+c.repo+"/v2/demo/refused/blobs/uploads/?digest="+digestOf(blob), blob, c.user,
			"Content-Type", "application/octet-stream")
		if whole.status != http.StatusForbidden {
			t.Errorf("docker, a blob in one request, %s: %d %v %q", c.what, whole.status, whole.header, whole.body)
		}
		manifest := u.send("PUT", "/repository/"+c.repo+"/v2/demo/refused/manifests/1.0", []byte(`{"schemaVersion":2}`), c.user,
			"Content-Type", dockerManifestType)
		if manifest.status != http.StatusForbidden {
			t.Errorf("docker, a manifest, %s: %d %v %q", c.what, manifest.status, manifest.header, manifest.body)
		}
	}
	for _, member := range []string{"demo-docker-target", "demo-docker-second"} {
		if u.row(member, "v2/demo/refused/blobs/"+digestOf(blob)) != nil || u.row(member, "v2/demo/refused/manifests/1.0") != nil {
			t.Errorf("docker: a refused write left a row in %s", member)
		}
	}
}
