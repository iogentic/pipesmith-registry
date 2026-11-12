package server

import (
	"math/big"
	"net/http"

	"github.com/mirosso25/pipesmith-registry/internal/listing"
	"github.com/mirosso25/pipesmith-registry/internal/repos"
)

// The read bounds of server.py: what the read path reads into memory, and
// the children of one listing page. Variables, so a test can cross them.
var (
	maxMetadataBytes int64 = 16 * 1024 * 1024  // one member's maven-metadata.xml in a group merge
	maxDocumentBytes int64 = 16 * 1024 * 1024  // a stored npm package document
	maxTarballBytes  int64 = 64 * 1024 * 1024  // one tarball read to build a document
	maxPackageBytes  int64 = 256 * 1024 * 1024 // the tarballs of one package read in one request
	listingLimit           = 10_000
)

// listing is _listing: one page of at most listingLimit children of a
// directory URL; ?offset= selects a later page.
func (s *Server) listing(w http.ResponseWriter, req *request, repo repos.Repo, members []repos.Repo, directory string) error {
	offset, apiErr := intParam(req.params(), "offset", 0, 0, nil)
	if apiErr != nil {
		writeText(w, req, apiErr.status, "Bad request: "+apiErr.message)
		return nil
	}
	if directory != "" {
		isDir, err := s.rp.anyIsDir(members, directory)
		if err != nil {
			return err
		}
		if !isDir {
			writeText(w, req, http.StatusNotFound, "Not found")
			return nil
		}
	}
	children, more, err := s.rp.page(members, directory, listingLimit, clampInt(offset))
	if err != nil {
		return err
	}
	var next *big.Int
	if more {
		next = new(big.Int).Add(offset, big.NewInt(int64(len(children))))
	}
	page, err := listing.Render(repo.Name, directory, children, offset, next)
	if err != nil {
		return err
	}
	writeBody(w, req, http.StatusOK, [][2]string{{"Content-Type", htmlType}}, []byte(page))
	return nil
}
