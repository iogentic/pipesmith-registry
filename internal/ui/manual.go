package ui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// manualFiles is the manual set of docs/, copied by tools/sync_manual.py.
//
//go:embed manual
var manualFiles embed.FS

// manualPrefix is the URL folder of the manual.
const manualPrefix = "/manual/"

// manualSet is the rule of the manual set of docs/, which tools/sync_manual.py
// reads from this file and copies into manual/. "*.ext" is a file directly in
// docs/ with that ending, "dir/*.ext" a file directly in docs/dir/ with that
// ending, "dir/**" every file below docs/dir/. Names that start with "." or
// "_" are left out, as go:embed leaves them out.
var manualSet = []string{"*.html", "*.css", "*.js", "*.json", "assets/**", "pdf/*.pdf"}

var manualTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".png":  "image/png",
	".svg":  "image/svg+xml",
	".json": "application/json",
	".pdf":  "application/pdf",
}

// manualDownloads are the endings a browser saves instead of showing them.
var manualDownloads = map[string]bool{".pdf": true}

// NewManual reads the embedded manual once. Its files are served below
// /manual/ with the headers and the policy of the interface files,
// index.html at /manual/ itself, and /manual redirects to /manual/.
func NewManual() (*Handler, error) {
	h := &Handler{byPath: map[string]file{}, redirect: strings.TrimSuffix(manualPrefix, "/")}
	err := fs.WalkDir(manualFiles, "manual", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		target := manualPrefix + strings.TrimPrefix(name, "manual/")
		if target == manualPrefix+"index.html" {
			target = manualPrefix
		}
		contentType, ok := manualTypes[path.Ext(name)]
		if !ok {
			return fmt.Errorf("ui: %s has no content type", name)
		}
		data, err := fs.ReadFile(manualFiles, name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		f := file{data: data, contentType: contentType, etag: `"` + hex.EncodeToString(sum[:16]) + `"`}
		if manualDownloads[path.Ext(name)] {
			f.disposition = `attachment; filename="` + path.Base(name) + `"`
		}
		h.byPath[target] = f
		return nil
	})
	if err != nil {
		return nil, err
	}
	return h, nil
}
