package server

import (
	"crypto/md5"  // #nosec G501 -- the MD5 checksum file of a Maven client, not a security use.
	"crypto/sha1" // #nosec G505 -- the SHA-1 checksum file of a Maven client, not a security use.
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"hash"
	"io"
	"net/http"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/maven"
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// maxChecksumBytes is how much of a checksum file the comparison of W7
// reads; a digest in hex is at most 128 characters.
const maxChecksumBytes = 1024

// writeMaven takes a write to a hosted maven2 repository (task 9 of the
// phase 4 plan): the PUT of every file of a deploy, the artifacts, their
// checksum files and maven-metadata.xml of every level, each stored as the
// client sends it (W15), 201. The policies of the repository decide in
// ingest. DELETE by path answers as for raw (write_files.go).
func (s *Server) writeMaven(w *recorder, req *request, st *state, repo repos.Repo, t *target) error {
	switch req.r.Method {
	case http.MethodPut:
		result, err := s.putFile(w, req, repo, t)
		if err != nil || result == nil {
			return err
		}
		if err := s.compareChecksum(st, repo, result); err != nil {
			logAt(slogWarn, serverLogger, "the checksum file %s of repository %s is stored; it could not be compared: %v",
				result.asset.Path, repo.Name, err)
		}
		created(w, req)
		return nil
	case http.MethodDelete:
		_, err := s.deleteFile(w, req, repo, t)
		return err
	}
	methodRefused(w, req, fileWriteMethods)
	return nil
}

// checksumHash is the hash of a checksum suffix of the Maven layout.
func checksumHash(algorithm string) hash.Hash {
	switch algorithm {
	case "md5":
		return md5.New() // #nosec G401 -- see the imports.
	case "sha1":
		return sha1.New() // #nosec G401 -- see the imports.
	case "sha256":
		return sha256.New()
	}
	return sha512.New()
}

// compareChecksum is W7 of the phase 4 plan: a checksum file that a client
// sends is stored as sent; when its digest disagrees with the file it names,
// a warning names the repository and the path. A checksum file of a file
// the repository does not hold is not compared.
func (s *Server) compareChecksum(st *state, repo repos.Repo, written *ingestResult) error {
	base, algorithm := maven.SplitChecksum(written.asset.Path)
	if algorithm == "" {
		return nil
	}
	sentRow, f, _, err := s.rp.open(st, repo.Name, written.asset.Path)
	if err != nil || sentRow == nil {
		return err
	}
	text, err := readUpTo(f, maxChecksumBytes)
	if err != nil {
		return err
	}
	fields := strings.Fields(string(text))
	row, file, _, err := s.rp.open(st, repo.Name, base)
	if err != nil || row == nil {
		return err
	}
	defer file.Close()
	var actual string
	if algorithm == "sha1" && row.SHA1 != nil && *row.SHA1 != "" {
		actual = strings.ToLower(*row.SHA1)
	} else {
		h := checksumHash(algorithm)
		if _, err := io.Copy(h, file); err != nil {
			return err
		}
		actual = hex.EncodeToString(h.Sum(nil))
	}
	if len(fields) == 0 || !strings.EqualFold(fields[0], actual) {
		logAt(slogWarn, serverLogger, "the checksum file %s of repository %s does not hold the %s of %s; it is stored as sent",
			written.asset.Path, repo.Name, algorithm, base)
	}
	return nil
}
