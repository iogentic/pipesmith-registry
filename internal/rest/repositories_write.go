package rest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/proxy"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The writes of the repositories (leg 6-stores, contract S10 and S11):
// create, save, delete and rebuild index. A repository is a record of
// state.sqlite (ruling ST3); its attributes are the body of the contract
// without name, format, type, url, online and the routing rule, and with
// every secret set to null (A11). The upstream secret of a proxy lies in
// secrets/remote/<name> (ST9). Every change passes admin.Change.

// recordKeys are the keys of a body that the record holds in its own
// fields or does not keep.
var recordKeys = []string{"name", "format", "type", "url", "online", "routingRule", "routingRuleName"}

// decodeObject reads a JSON object with its numbers as json.Number.
func decodeObject(body []byte) (map[string]any, error) {
	var probe map[string]json.RawMessage
	if err := decode(body, '{', &probe); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, &admin.Refusal{Code: admin.CodeBodyInvalid, Field: "*", Message: "the body is no JSON object"}
	}
	return out, nil
}

// storeNames are the stores a repository may name: the write store, the
// attached ones and the stored ones.
func (w StoreWrite) storeNames(s *model.Snapshot) []string {
	names := append([]string{admin.WriteStore}, w.Attached...)
	for _, b := range s.BlobStores {
		names = append(names, b.Name)
	}
	return names
}

// writableNames are the write stores among storeNames (SR5): local-write
// and the created stores; an imported store is read only.
func (w StoreWrite) writableNames(s *model.Snapshot) []string {
	names := []string{admin.WriteStore}
	for _, b := range s.BlobStores {
		if admin.WritableStore(w.DataDir, b, w.Attached, s.BlobStores) {
			names = append(names, b.Name)
		}
	}
	return names
}

// withReadPath is the snapshot with a record for every repository of the
// read path that has none, so that the member rules see the repositories
// of repositories.json too.
func (env *Env) withReadPath(s *model.Snapshot) *model.Snapshot {
	c := *s
	c.Repositories = slices.Clone(s.Repositories)
	for _, r := range env.Repositories.All() {
		if admin.Exists(s, admin.KindRepository, r.Name) {
			continue
		}
		members := make([]any, len(r.Members))
		for i, m := range r.Members {
			members[i] = m
		}
		c.Repositories = append(c.Repositories, model.Repository{Name: r.Name, Format: r.Format, Type: r.Type, Online: true,
			Attributes: map[string]any{"group": map[string]any{"memberNames": members}}})
	}
	return &c
}

// storeCreatePermission is the permission a new blob store needs (SR6), the
// one of POST /v1/blobstores/file.
const storeCreatePermission = "nexus:blobstores:create"

// addStore adds the record of the new store of a create of a hosted
// repository to s (SR6): the name of storage.blobStoreName, checked as POST
// /v1/blobstores/file checks it (ST1, ST14). It gives the folder the store
// needs; the caller makes it once every check passed.
func (w StoreWrite) addStore(s *model.Snapshot, body map[string]any, ns *admin.NewStore) (string, error) {
	name, _ := textAt(body, "storage", "blobStoreName")
	if err := admin.CheckCreate(admin.KindBlobStore, name, w.storeExists(s, name)); err != nil {
		return "", err
	}
	sent := "<data>/stores/" + name
	if ns.Path != nil {
		sent = *ns.Path
	}
	if err := admin.CheckStoreCreate("file", sent, ns.SoftQuota); err != nil {
		return "", err
	}
	resolved, err := admin.StorePath(w.DataDir, name, ns.Path, w.takenPaths(s, name))
	if err != nil {
		return "", err
	}
	s.BlobStores = append(slices.Clone(s.BlobStores), model.BlobStore{Name: name, Type: "File", Path: resolved, SoftQuota: ns.SoftQuota})
	return resolved, nil
}

// makeFolder makes the folder of a new store and its missing parents. It
// gives the topmost folder it made, "" when the folder was there.
func makeFolder(dir string) (string, error) {
	top := ""
	for p := dir; ; p = filepath.Dir(p) {
		if _, err := os.Lstat(p); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		top = p
		if filepath.Dir(p) == p {
			break
		}
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	return top, nil
}

func textAt(m map[string]any, section, key string) (string, bool) { return valueText(m, section, key) }

// attributesOf are the attributes a record keeps of a body: every key but
// recordKeys and the server owned ones, the secrets set to null; the server
// owned attributes come from held, the stored attributes (ST13).
func attributesOf(body, held map[string]any) map[string]any {
	out := maps.Clone(body)
	for _, key := range recordKeys {
		delete(out, key)
	}
	if storage, _ := out["storage"].(map[string]any); storage != nil {
		if _, sent := storage["newBlobStore"]; sent {
			storage = maps.Clone(storage)
			delete(storage, "newBlobStore") // SR6: a part of the create, not of the record
			out["storage"] = storage
		}
	}
	for key := range admin.ServerAttributes {
		delete(out, key)
		if v, found := held[key]; found {
			out[key] = v
		}
	}
	return admin.Redact(admin.KindRepository, out, false)
}

// recordAttributes are the attributes of the record of a repository in the
// shape of the contract: httpclient of the import read as httpClient.
func recordAttributes(rec *model.Repository) map[string]any {
	if rec == nil {
		return nil
	}
	out := maps.Clone(rec.Attributes)
	if client, found := out["httpclient"]; found {
		if _, contract := out["httpClient"]; !contract {
			out["httpClient"] = client
		}
		delete(out, "httpclient")
	}
	return out
}

func recordOf(s *model.Snapshot, name string) *model.Repository {
	for i := range s.Repositories {
		if s.Repositories[i].Name == name {
			return &s.Repositories[i]
		}
	}
	return nil
}

func membersOf(body map[string]any) []string {
	group, _ := body["group"].(map[string]any)
	list, _ := group["memberNames"].([]any)
	out := []string{}
	for _, item := range list {
		if name, ok := item.(string); ok {
			out = append(out, name)
		}
	}
	return out
}

// checkBody checks the body of a create or a save of a repository of the
// format and type: the fields of A23, the store (ST2), the Docker port
// (ST7), the remote address (ST8, ST11), the members of a group, the
// routing rule and the upstream secret (ST9). stored is the record of a
// save and nil for a create; resolved gives the addresses of the remote
// host, resolved before the state write (ST11). The refusals of 400 travel
// together.
func (env *Env) checkBody(w StoreWrite, s *model.Snapshot, name, format, kind string, body map[string]any, stored *model.Repository,
	resolved admin.Lookup) (admin.UpstreamSecret, error) {
	storedName, held := "", recordAttributes(stored)
	if stored != nil {
		storedName = stored.Name
	}
	var errs []error
	errs = append(errs, admin.CheckPassThrough(s, admin.KindRepository, storedName, body),
		admin.CheckStorage(kind, body, held, w.storeNames(s), w.writableNames(s)), admin.CheckServerAttributes(kind, body, held, stored == nil),
		admin.CheckDockerPort(body, held))
	if _, err := admin.NewStoreOf(kind, body, stored == nil); err != nil {
		errs = append(errs, err)
	}
	if v, found := body["online"]; found && v != nil {
		if _, ok := v.(bool); !ok {
			errs = append(errs, &admin.Refusal{Code: admin.CodeBodyInvalid, Field: "online", Message: "the value of online is no boolean"})
		}
	}
	if rule, _ := body["routingRule"].(string); rule != "" && (env.Repositories == nil || !sameRule(env, name, rule)) {
		errs = append(errs, &admin.Refusal{Code: admin.CodeNotOffered, Field: "routingRule",
			Message: "the routing rule of a repository is not set over REST in this server; send routingRule as null"})
	}
	remote, _ := valueText(body, "proxy", "remoteUrl")
	var remoteRef *string
	if remote != "" {
		remoteRef = &remote
	}
	remoteErr := admin.CheckRemote(kind, remoteRef)
	errs = append(errs, remoteErr)
	if heldRemote, _ := valueText(held, "proxy", "remoteUrl"); remoteErr == nil && kind == "proxy" && remote != heldRemote {
		errs = append(errs, admin.CheckRemoteHost(remote, resolved, env.permits))
	}
	if kind == "group" {
		errs = append(errs, admin.CheckMembers(env.withReadPath(s), name, format, membersOf(body)))
	}
	secret := admin.UpstreamSecret{Action: admin.SecretKeep}
	if kind == "proxy" {
		var err error
		secret, err = admin.UpstreamSecretOf(body)
		errs = append(errs, err)
	}
	return secret, errors.Join(errs...)
}

// newFormat reports that no repository of the format exists: the first one
// makes the view privileges of the whole format grant (ST12).
func (env *Env) newFormat(s *model.Snapshot, format string) bool {
	return !slices.ContainsFunc(env.withReadPath(s).Repositories, func(r model.Repository) bool { return r.Format == format })
}

// addedMembers are the members of a group body that held does not name,
// mapped to their formats; a member that does not exist is left to
// CheckMembers.
func (env *Env) addedMembers(s *model.Snapshot, kind string, body map[string]any, held []string) map[string]string {
	out := map[string]string{}
	if kind != "group" {
		return out
	}
	all := env.withReadPath(s)
	for _, m := range membersOf(body) {
		if slices.Contains(held, m) {
			continue
		}
		if i := slices.IndexFunc(all.Repositories, func(r model.Repository) bool { return r.Name == m }); i >= 0 {
			out[m] = all.Repositories[i].Format
		}
	}
	return out
}

// fullPermission is the permission set of an administrator of everything.
const fullPermission = "nexus:*"

// resolveRemote resolves the host of the remote address of a proxy body
// before the state write (ST11, finding 11), when the rule of the host
// applies: a proxy whose remote a subject without the full permission set
// sets or changes. The answer is a lookup that gives the result for that
// host; nil when nothing was resolved.
func (env *Env) resolveRemote(w StoreWrite, kind string, body map[string]any, heldRemote string) admin.Lookup {
	remote, _ := valueText(body, "proxy", "remoteUrl")
	if kind != "proxy" || remote == "" || remote == heldRemote || env.permits(fullPermission) || w.Lookup == nil {
		return nil
	}
	u, err := url.Parse(remote)
	if err != nil {
		return nil
	}
	host := u.Hostname()
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	addrs, err := w.Lookup(host)
	return func(h string) ([]netip.Addr, error) {
		if h != host {
			return nil, errors.New("not resolved")
		}
		return addrs, err
	}
}

// setPublicOnly sets the server owned attribute repos.PublicOnlyKey of a
// proxy (ST11): a remote a subject without the full permission set sets or
// changes gets it, one the full permission set sets loses it, and an
// unchanged remote keeps the stored value.
func (env *Env) setPublicOnly(kind string, attributes, held map[string]any) {
	delete(attributes, repos.PublicOnlyKey)
	if kind != "proxy" {
		return
	}
	remote, _ := valueText(attributes, "proxy", "remoteUrl")
	heldRemote, _ := valueText(held, "proxy", "remoteUrl")
	switch {
	case held != nil && remote == heldRemote:
		if held[repos.PublicOnlyKey] == true {
			attributes[repos.PublicOnlyKey] = true
		}
	case !env.permits(fullPermission):
		attributes[repos.PublicOnlyKey] = true
	}
}

// sameRule reports whether the repository name names the rule already.
func sameRule(env *Env, name, rule string) bool {
	r, found := env.Repositories.Get(name)
	return found && r.RoutingRule == rule
}

func valueText(m map[string]any, section, key string) (string, bool) {
	sec, _ := m[section].(map[string]any)
	s, ok := sec[key].(string)
	return s, ok
}

// applySecret writes or removes the secret file of the proxy name inside
// the write of its record, after the checks (ST16, the form of D9 of leg
// 6-identity-b). It gives the undo that puts the file back as it was, for a
// state write that fails after it.
func applySecret(dataDir, name string, secret admin.UpstreamSecret) (func(), error) {
	if secret.Action == admin.SecretKeep {
		return func() {}, nil
	}
	old, err := proxy.ReadSecret(dataDir, name)
	had := err == nil
	if err != nil && !errors.Is(err, proxy.ErrNoSecret) {
		return nil, err
	}
	undo := func() {
		if had {
			_, _ = proxy.WriteSecret(dataDir, name, old) // the error of the state write is the one answered
		} else {
			_ = proxy.RemoveSecret(dataDir, name) // the error of the state write is the one answered
		}
	}
	if secret.Action == admin.SecretWrite {
		_, err = proxy.WriteSecret(dataDir, name, secret.Value)
	} else {
		err = proxy.RemoveSecret(dataDir, name)
	}
	if err != nil {
		undo()
		return nil, err
	}
	return undo, nil
}

// CreateRepository is POST /v1/repositories/{segment}/{type}: 201 without
// a body. A format or type outside A5 is 501; the creator holds nexus:* or
// the administration permission of every action the generated privileges
// of the new repository grant (ST10). A hosted repository writes into
// local-write.
func CreateRepository(env *Env, w StoreWrite, segment, kind string, body []byte) (Answer, error) {
	format, err := admin.CheckSegment(segment, kind)
	if err != nil {
		return refused(err)
	}
	b, err := decodeObject(body)
	if err != nil {
		return refused(err)
	}
	name, _ := b["name"].(string)
	newStore, err := admin.NewStoreOf(kind, b, true)
	if err != nil {
		return refused(err)
	}
	kinds := []state.Kind{state.KindRepositories}
	if newStore != nil {
		if !env.permits(storeCreatePermission) {
			return refused(&admin.Refusal{Code: admin.CodeGrantExceeds, Message: fmt.Sprintf(
				"a new blob store with the repository needs the permission %s, which the signed in user does not hold", storeCreatePermission)})
		}
		kinds = append(kinds, state.KindBlobStores)
	}
	resolved := env.resolveRemote(w, kind, b, "")
	var secret admin.UpstreamSecret
	var undo func()
	made := ""
	_, err = admin.Change(w.Store, kinds, func(s *model.Snapshot) error {
		made = ""
		var names []string
		for _, r := range env.withReadPath(s).Repositories {
			names = append(names, r.Name)
		}
		if err := admin.CheckCreate(admin.KindRepository, name, admin.Taken(names, name)); err != nil {
			return err
		}
		folder := ""
		if newStore != nil {
			var err error
			if folder, err = w.addStore(s, b, newStore); err != nil {
				return err
			}
		}
		var err error
		if secret, err = env.checkBody(w, s, name, format, kind, b, nil, resolved); err != nil {
			return err
		}
		if err := admin.CheckRepositoryGrant(format, name, env.newFormat(s, format), env.permits); err != nil {
			return err
		}
		if err := admin.CheckMemberGrant(name, env.addedMembers(s, kind, b, nil), env.permits); err != nil {
			return err
		}
		online, found := b["online"].(bool)
		attributes := attributesOf(b, nil)
		env.setPublicOnly(kind, attributes, nil)
		if kind == "hosted" {
			attributes["write_store"] = admin.CreateAttributes(kind, b)["write_store"] // SR5: the uploads land in the store it names
		}
		s.Repositories = append(slices.Clone(s.Repositories), model.Repository{Name: name, Format: format, Type: kind,
			Online: online || !found, Attributes: attributes})
		if folder != "" {
			if made, err = makeFolder(folder); err != nil {
				return err
			}
		}
		if secret.Action != admin.SecretWrite {
			secret = admin.UpstreamSecret{Action: admin.SecretRemove} // a create starts clean (ST15)
		}
		undo, err = applySecret(w.DataDir, name, secret)
		return err
	})
	if err != nil {
		if undo != nil {
			undo()
		}
		if made != "" {
			// SR6, review R5: the folders this create made go with it, from the topmost one; nothing else
			// writes into them before the record exists. The error of the change is the one answered.
			_ = os.RemoveAll(made)
		}
		return refused(err)
	}
	return Answer{Status: http.StatusCreated}, nil
}

// SaveRepository is PUT /v1/repositories/{segment}/{type}/{name}: 204.
// The name cannot change. A repository that repositories.json of the host
// defines is kept with 409, as its delete is (ruling ST18). A save without an authentication object keeps
// the stored one and the secret file (R9); the type none removes both.
func SaveRepository(env *Env, w StoreWrite, segment, kind, name string, body []byte) (Answer, error) {
	r, found := env.Repositories.Get(name)
	if !found || SegmentOf(r.Format) != segment || r.Type != kind {
		return refuse(http.StatusNotFound, "repository "+name+" not found as "+segment+" "+kind)
	}
	if r.Defined {
		return refused(&admin.Refusal{Code: admin.CodeRepositoryDefined, Message: fmt.Sprintf(
			"the repository %s is defined in repositories.json of the host; change it there", name)})
	}
	b, err := decodeObject(body)
	if err != nil {
		return refused(err)
	}
	var sent *string
	if v, ok := b["name"].(string); ok {
		sent = &v
	}
	if err := admin.CheckRename(admin.KindRepository, name, sent); err != nil {
		return refused(err)
	}
	heldRemote, _ := valueText(recordAttributes(env.record(name)), "proxy", "remoteUrl")
	resolved := env.resolveRemote(w, r.Type, b, heldRemote)
	var secret admin.UpstreamSecret
	var undo func()
	_, err = admin.Change(w.Store, []state.Kind{state.KindRepositories}, func(s *model.Snapshot) error {
		stored := recordOf(s, name)
		var err error
		if secret, err = env.checkBody(w, s, name, r.Format, r.Type, b, stored, resolved); err != nil {
			return err
		}
		if err := admin.CheckMemberGrant(name, env.addedMembers(s, r.Type, b, r.Members), env.permits); err != nil {
			return err
		}
		attributes := attributesOf(b, recordAttributes(stored))
		env.setPublicOnly(r.Type, attributes, recordAttributes(stored))
		client, _ := attributes["httpClient"].(map[string]any)
		heldClient, _ := recordAttributes(stored)["httpClient"].(map[string]any)
		switch {
		case secret.Action == admin.SecretRemove && client != nil:
			delete(client, "authentication")
		case secret.Action == admin.SecretKeep && heldClient != nil && heldClient["authentication"] != nil:
			if client == nil {
				client = map[string]any{}
				attributes["httpClient"] = client
			}
			if auth, _ := client["authentication"].(map[string]any); auth == nil {
				client["authentication"] = heldClient["authentication"]
			}
		}
		online, found := b["online"].(bool)
		rec := model.Repository{Name: name, Format: r.Format, Type: r.Type, Online: online || !found, Attributes: attributes}
		list := slices.Clone(s.Repositories)
		if i := slices.IndexFunc(list, func(x model.Repository) bool { return x.Name == name }); i >= 0 {
			list[i] = rec
		} else {
			list = append(list, rec)
		}
		s.Repositories = list
		undo, err = applySecret(w.DataDir, name, secret)
		return err
	})
	if err != nil {
		if undo != nil {
			undo()
		}
		return refused(err)
	}
	return noContent()
}

// SegmentOf is the REST path segment of a format.
func SegmentOf(format string) string { return admin.SegmentOf(format) }

// groupsOf maps each group of the read path to its members.
func (env *Env) groupsOf() map[string][]string {
	groups := map[string][]string{}
	for _, r := range env.Repositories.All() {
		if r.Type == "group" {
			groups[r.Name] = r.Members
		}
	}
	return groups
}

// DeleteRepository is DELETE /v1/repositories/{name}: 204 (A7, rulings
// ST5 and ST17). A member of a group is kept (409); so is a repository with
// rows in a store of the old product, which a scan of that store would
// bring back, and one that repositories.json of the host defines: the host
// removes those. The record goes offline first, so that no write lands
// while the content goes through w.Erase: the index and catalog rows, the
// delete mark of every blob in local-write, the proxy state. Then the
// upstream secret goes and the record last, in one write, so a crash leaves
// an offline record without content, never content without a record.
func DeleteRepository(env *Env, w StoreWrite, name string) (Answer, error) {
	r, found := env.Repositories.Get(name)
	if !found {
		return refuse(http.StatusNotFound, "repository "+name+" not found")
	}
	if r.Defined {
		return refused(&admin.Refusal{Code: admin.CodeRepositoryDefined, Message: fmt.Sprintf(
			"the repository %s is defined in repositories.json of the host; remove it there", name)})
	}
	if err := admin.CheckRepositoryDelete(name, env.groupsOf()); err != nil {
		return refused(err)
	}
	if w.OldRows != nil {
		old, err := w.OldRows(name)
		if err != nil {
			return Answer{}, err
		}
		if len(old) > 0 {
			return refused(&admin.Refusal{Code: admin.CodeRepositoryDefined, Message: fmt.Sprintf(
				"the repository %s has files in the store %s of the old product, which a scan brings back; the host removes it",
				name, strings.Join(old, ", "))})
		}
	}
	_, err := admin.Change(w.Store, []state.Kind{state.KindRepositories}, func(s *model.Snapshot) error {
		list := slices.Clone(s.Repositories)
		if i := slices.IndexFunc(list, func(x model.Repository) bool { return x.Name == name }); i >= 0 {
			list[i].Online = false
		}
		s.Repositories = list
		return admin.CheckRepositoryDelete(name, env.groupsOf())
	})
	if err != nil {
		return refused(err)
	}
	if w.Erase != nil {
		if err := w.Erase(name); err != nil {
			return refused(err)
		}
	}
	var undo func()
	_, err = admin.Change(w.Store, []state.Kind{state.KindRepositories}, func(s *model.Snapshot) error {
		s.Repositories = slices.DeleteFunc(slices.Clone(s.Repositories), func(x model.Repository) bool { return x.Name == name })
		var err error
		undo, err = applySecret(w.DataDir, name, admin.UpstreamSecret{Action: admin.SecretRemove})
		return err
	})
	if err != nil {
		if undo != nil {
			undo()
		}
		return refused(err)
	}
	return noContent()
}

// RebuildIndex is POST /v1/repositories/{name}/rebuild-index: for a hosted
// maven2 repository the maven metadata is written again from its files
// (ruling ST6, w.Rebuild); any other repository answers 204 with nothing
// done.
func RebuildIndex(env *Env, w StoreWrite, name string) (Answer, error) {
	r, found := env.Repositories.Get(name)
	if !found {
		return refuse(http.StatusNotFound, "repository "+name+" not found")
	}
	if r.Format != "maven2" || r.Type != "hosted" || w.Rebuild == nil {
		return noContent()
	}
	return w.Rebuild(r)
}
