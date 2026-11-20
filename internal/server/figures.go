package server

import "net/http"

// markDownload notes a download of decision L11 of the phase 7 plan on a
// tracked request: a GET answered with 200, or with 206 from byte 0.
func markDownload(req *request, status int, start int64) {
	if req.head || req.r.Method != http.MethodGet {
		return
	}
	if status == http.StatusOK || status == http.StatusPartialContent && start == 0 {
		if n := noteOf(req.r); n != nil {
			n.download = true
		}
	}
}
