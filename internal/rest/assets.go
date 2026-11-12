package rest

import (
	"encoding/base64"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/maven"
)

// AssetID is the id of the file path of a repository: base64url without
// padding of "<repository>:<path>". It can be read back without a catalog;
// a repository name holds no colon.
func AssetID(repo, path string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(repo + ":" + path))
}

// DecodeAssetID is the repository and path of an id, false for an id that
// AssetID cannot have made.
func DecodeAssetID(id string) (string, string, bool) {
	data, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil || !utf8.Valid(data) {
		return "", "", false
	}
	repo, path, found := strings.Cut(string(data), ":")
	if !found || repo == "" || path == "" || strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return "", "", false
	}
	return repo, path, true
}

// assetXO is AssetXO. Values this server does not hold are null:
// lastModified, lastDownloaded, uploader and uploaderIp. The checksum map
// holds the SHA-1 of the index, the only digest it has.
type assetXO struct {
	DownloadURL    string            `json:"downloadUrl"`
	Path           string            `json:"path"`
	ID             string            `json:"id"`
	Repository     string            `json:"repository"`
	Format         string            `json:"format"`
	Checksum       map[string]string `json:"checksum"`
	ContentType    string            `json:"contentType"`
	LastModified   *string           `json:"lastModified"`
	LastDownloaded *string           `json:"lastDownloaded"`
	Uploader       *string           `json:"uploader"`
	UploaderIP     *string           `json:"uploaderIp"`
	FileSize       int64             `json:"fileSize"`
	BlobCreated    *string           `json:"blobCreated"`
	BlobStoreName  string            `json:"blobStoreName"`
}

// assetXO is the AssetXO of the winning row a of a path; requested is the
// repository the id and the download address name, a.Repo the member that
// holds it.
func (env *Env) assetXO(requested, format string, a *index.Asset) assetXO {
	checksum := map[string]string{}
	if a.SHA1 != nil && *a.SHA1 != "" {
		checksum["sha1"] = strings.ToLower(*a.SHA1)
	}
	contentType := maven.ContentTypeFor(a.Path)
	if a.ContentType != nil && *a.ContentType != "" {
		contentType = *a.ContentType
	}
	created := timestamp(time.UnixMilli(a.CreatedMS))
	return assetXO{
		DownloadURL:   env.url(requested) + "/" + pathURL(a.Path),
		Path:          a.Path,
		ID:            AssetID(requested, a.Path),
		Repository:    a.Repo,
		Format:        format,
		Checksum:      checksum,
		ContentType:   contentType,
		FileSize:      a.Size,
		BlobCreated:   &created,
		BlobStoreName: a.Store,
	}
}

// Asset is GET /v1/assets/{id}: 422 for an id this server cannot have
// made, 404 for a path no member holds. Of a group the first member that
// holds the path wins, as on the read path. The server checks the view
// permission of the repository of the id first.
func Asset(env *Env, id string) (Answer, error) {
	repo, path, valid := DecodeAssetID(id)
	if !valid {
		return refuse(http.StatusUnprocessableEntity, "the asset id is malformed")
	}
	r, found := env.Repositories.Get(repo)
	if !found {
		return refuse(http.StatusNotFound, "asset not found")
	}
	for _, member := range env.Repositories.Resolve(repo) {
		a, err := env.Index.Lookup(member.Name, path)
		if err != nil {
			return Answer{}, err
		}
		if a != nil {
			return ok(env.assetXO(repo, r.Format, a))
		}
	}
	return refuse(http.StatusNotFound, "asset not found")
}
