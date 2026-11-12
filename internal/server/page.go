package server

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"net/http"
	"regexp"
	"strings"
)

const htmlType = "text/html; charset=utf-8"

// pageHTML is static/index.html of the reference with the names of this
// repository, written by tools/vectors/server_api.py --write-page.
//
//go:embed static/index.html
var pageHTML []byte

// pageHeaders are the headers of _page(): the policy allows exactly the
// inline script and style blocks of the page.
var pageHeaders = pageHeadersOf(string(pageHTML))

func pageHeadersOf(html string) [][2]string {
	sources := func(tag string) string {
		blocks := regexp.MustCompile(`(?s)<`+tag+`>(.*?)</`+tag+`>`).FindAllStringSubmatch(html, -1)
		var digests []string
		for _, block := range blocks {
			sum := sha256.Sum256([]byte(block[1]))
			digests = append(digests, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
		}
		if len(digests) == 0 {
			return "'none'"
		}
		return strings.Join(digests, " ")
	}
	policy := strings.Join([]string{
		"default-src 'none'",
		"script-src " + sources("script"),
		"style-src " + sources("style"),
		"connect-src 'self'",
		"img-src 'self' data:",
		"base-uri 'none'",
		"form-action 'none'",
		"frame-ancestors 'none'",
	}, "; ")
	return [][2]string{
		{"Content-Security-Policy", policy},
		{"X-Content-Type-Options", "nosniff"},
		{"Referrer-Policy", "no-referrer"},
		{"Cache-Control", "no-cache"},
		{"Content-Type", htmlType},
	}
}

// page is GET /: with security on the page of the interface.
func (s *Server) page(w *recorder, req *request, _ []string) error {
	if s.ui != nil {
		s.ui.Serve(w, req.r, "/")
		return nil
	}
	writeBody(w, req, http.StatusOK, pageHeaders, pageHTML)
	return nil
}
