package server

import "net/http"

// markDownload notes a download of decision L11 of the phase 7 plan on a
// tracked request: a GET answered with 200, or with 206 from byte 0. The
// same download of a stored file sets the last download time of its asset
// (decision A19).
func markDownload(w http.ResponseWriter, req *request, b *body, status int, start int64) {
	if req.head || req.r.Method != http.MethodGet {
		return
	}
	if status == http.StatusOK || status == http.StatusPartialContent && start == 0 {
		if n := noteOf(req.r); n != nil {
			n.download = true
		}
		if rec, ok := w.(*recorder); ok && b.repo != "" {
			rec.s.noteDownload(b.repo, b.path)
		}
	}
}
