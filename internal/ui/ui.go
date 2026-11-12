// Package ui serves the administration interface: the files of ui/web of
// the interface repository at commit 1e6d884a06f44de2a3c42b5ce91476b8540a80d0,
// taken over unchanged with git archive on 2026-11-12 and embedded in the
// binary. index.html is served at "/", the other files below /css/, /js/
// and /assets/, with the headers of the Serving section of the interface
// README. Nothing else is served from here.
package ui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
)

//go:embed web
var files embed.FS

// Policy is the content security policy of the page and its files.
const Policy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; " +
	"font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".png":  "image/png",
}

// folders are the URL folders below which the files are served.
var folders = []string{"/css/", "/js/", "/assets/"}

type file struct {
	data        []byte
	contentType string
	etag        string
}

// Handler serves the embedded files. It is safe for concurrent use.
type Handler struct {
	byPath map[string]file // URL path -> file
}

// New reads the embedded files once. A file of an extension without a
// content type is an error, so a new kind of file cannot go out untyped.
func New() (*Handler, error) {
	h := &Handler{byPath: map[string]file{}}
	err := fs.WalkDir(files, "web", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		target := strings.TrimPrefix(name, "web")
		if target == "/index.html" {
			target = "/"
		} else if !served(target) {
			return nil
		}
		contentType, ok := contentTypes[path.Ext(name)]
		if !ok {
			return fmt.Errorf("ui: %s has no content type", name)
		}
		data, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		h.byPath[target] = file{data: data, contentType: contentType, etag: `"` + hex.EncodeToString(sum[:16]) + `"`}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return h, nil
}

func served(target string) bool {
	for _, folder := range folders {
		if strings.HasPrefix(target, folder) {
			return true
		}
	}
	return false
}

// Has reports whether path is the page or one of its files.
func (h *Handler) Has(path string) bool {
	_, ok := h.byPath[path]
	return ok
}

// ServeHTTP is Serve for the path of the request URL.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Serve(w, r, r.URL.Path)
}

// Serve answers GET and HEAD of the page and its files at path, 304 for a
// matching If-None-Match, 405 for another method and 404 for another path.
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, path string) {
	f, ok := h.byPath[path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	header := w.Header()
	header.Set("Content-Security-Policy", Policy)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "same-origin")
	header.Set("Cache-Control", "no-cache")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		header.Set("Allow", "GET, HEAD")
		header.Set("Content-Length", "0")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	header.Set("ETag", f.etag)
	if matches(r.Header.Get("If-None-Match"), f.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	header.Set("Content-Type", f.contentType)
	header.Set("Content-Length", strconv.Itoa(len(f.data)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(f.data) // a client that went away gets nothing more
	}
}

// matches is the weak comparison of If-None-Match: one of the listed tags,
// or "*".
func matches(header, etag string) bool {
	for _, tag := range strings.Split(header, ",") {
		tag = strings.TrimPrefix(strings.TrimSpace(tag), "W/")
		if tag == etag || tag == "*" {
			return true
		}
	}
	return false
}
