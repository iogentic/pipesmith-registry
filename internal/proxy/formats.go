package proxy

import (
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/docker"
	"github.com/iogentic/pipesmith-registry/internal/maven"
	"github.com/iogentic/pipesmith-registry/internal/npm"
	"github.com/iogentic/pipesmith-registry/internal/policy"
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// format is what a format of the old product adds to the fetch of its
// proxies. A format without an entry in formats does not fetch: its proxies
// serve their cache only.
type format struct {
	// fetchable reports whether a path may go to the remote at all; a path it
	// refuses answers 404 with no request.
	fetchable func(repo repos.Repo, path string) bool
	// metadata reports a path whose freshness follows the metadata age; every
	// other path follows the content age (section Settings of the phase 5
	// plan).
	metadata func(path string) bool
	// artifact reports a path whose new bytes the .sha1 of the remote must
	// describe (decision 5.10); nil for a format without checksum files.
	artifact func(path string) bool
	// permanent reports a path whose address is its digest: a cached copy is
	// never checked again, whatever the ages say (section Settings of the
	// phase 5 plan); nil for none.
	permanent func(path string) bool
	// tokens is the token flow of the registry protocol (tokens.go).
	tokens bool
}

// formats are the formats whose proxies fetch.
var formats = map[string]format{
	"raw":    {fetchable: func(repos.Repo, string) bool { return true }, metadata: func(string) bool { return false }},
	"maven2": {fetchable: mavenFetchable, metadata: policy.MavenMetadata, artifact: mavenArtifact},
	"npm":    {fetchable: npmFetchable, metadata: npmDocument},
	"docker": {fetchable: dockerFetchable, metadata: dockerMetadata, permanent: dockerPermanent, tokens: true},
}

// DockerBlobPrefix is the shared path of the blobs of a Docker repository,
// which every image name of it shares, as the write path stores them.
const DockerBlobPrefix = "v2/-/blobs/"

// dockerCall is the registry call of a path of a Docker proxy: a manifest
// by tag or by digest, the tag list, or a blob under its shared path.
func dockerCall(path string) (kind, reference string, ok bool) {
	if digest, isBlob := strings.CutPrefix(path, DockerBlobPrefix); isBlob {
		return "blob", digest, docker.IsDigest(digest)
	}
	rest, ok := strings.CutPrefix(path, "v2/")
	if !ok {
		return "", "", false
	}
	call, err := docker.ParseV2(rest)
	if err != nil || call == nil || call.Kind != "manifest" && call.Kind != "tags" {
		return "", "", false
	}
	return call.Kind, call.Reference, true
}

// dockerFetchable is a path a Docker proxy asks its remote for.
func dockerFetchable(_ repos.Repo, path string) bool {
	_, _, ok := dockerCall(path)
	return ok
}

// dockerMetadata is a manifest by tag or the tag list: they follow the
// metadata age.
func dockerMetadata(path string) bool {
	kind, reference, ok := dockerCall(path)
	return ok && (kind == "tags" || kind == "manifest" && !docker.IsDigest(reference))
}

// dockerPermanent is a manifest by digest or a blob.
func dockerPermanent(path string) bool {
	kind, reference, ok := dockerCall(path)
	return ok && (kind == "blob" || kind == "manifest" && docker.IsDigest(reference))
}

// npmDocument is the path of a package document: <name> or @scope/<name>.
func npmDocument(path string) bool {
	_, ok := npm.PackageName(path)
	return ok
}

// npmFetchable is a path an npm proxy asks its remote for: a package
// document or a tarball, <name>/-/<file>.tgz. A version document is answered
// from the package document, and the calls below "-" never go out
// (decision 5.18).
func npmFetchable(_ repos.Repo, path string) bool {
	if npmDocument(path) {
		return true
	}
	head, file, ok := strings.Cut(path, "/"+npm.TarballDirectory+"/")
	return ok && npmDocument(head) && strings.HasSuffix(file, ".tgz") && !strings.Contains(file, "/")
}

// mavenArtifact is a maven2 path that is neither metadata nor a checksum
// file itself: a jar, a pom, a signature, any other file of the layout.
func mavenArtifact(path string) bool {
	_, checksum := maven.SplitChecksum(path)
	return checksum == "" && !policy.MavenMetadata(path)
}

// ageOf is the age in minutes that applies to a path of a proxy: the
// metadata age for a metadata path of its format, else the content age.
func ageOf(repo repos.Repo, settings repos.ProxySettings, path string) int64 {
	if f, ok := formats[repo.Format]; ok && f.metadata(path) {
		return settings.MetadataMaxAge
	}
	return settings.ContentMaxAge
}

// permanent reports a cached path of a proxy that is never checked again.
func permanent(repo repos.Repo, path string) bool {
	f, ok := formats[repo.Format]
	return ok && f.permanent != nil && f.permanent(path)
}

// Fetches reports whether a proxy of the format asks its remote.
func Fetches(name string) bool {
	_, ok := formats[name]
	return ok
}

// fetchable reports whether a path of a proxy may go to its remote: a path
// of a format that fetches, without a control character, that the rules of
// the format let through.
func fetchable(repo repos.Repo, path string) bool {
	f, ok := formats[repo.Format]
	return ok && path != "" && !strings.ContainsFunc(path, isControl) && f.fetchable(repo, path)
}

// mavenFetchable is the version and the layout policy of a maven2 proxy
// (ruling P5-3 of the phase 5 session): a path outside them is never asked
// for.
func mavenFetchable(repo repos.Repo, path string) bool {
	settings := repo.WriteSettings()
	return policy.Version(settings.VersionPolicy, path) == nil && policy.Layout(settings.LayoutPolicy, path) == nil
}
