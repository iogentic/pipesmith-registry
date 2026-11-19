package server

import (
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// hiddenScanError stands for the error text of a failed scan.
const hiddenScanError = "the scan failed, the server log has the cause"

// hostPermission is the right to see the folders and the address of the
// host in /api/status, the right to read the blob store settings.
const hostPermission = "nexus:blobstores:read"

// statusView is the /api/status answer as the caller may see it. With
// security on, a subject without hostPermission gets the same keys with the
// folders, the bind address and the store settings as null, the Docker
// ports of the repositories it may browse, and no error text of a scan.
func (s *Server) statusView(req *request, status *pyjson.Object) *pyjson.Object {
	if s.sec == nil || s.sec.Permits(req.subject, hostPermission) {
		return status
	}
	status.Set("data_dir", nil).Set("bind", nil)
	if list, ok := status.Get("stores"); ok {
		for _, store := range list.([]any) {
			store.(*pyjson.Object).Set("root", nil).Set("repos", nil).Set("manifest", nil)
		}
	}
	if list, ok := status.Get("docker_ports"); ok {
		ports := []any{}
		for _, port := range list.([]any) {
			name, _ := port.(*pyjson.Object).Get("repo")
			if repo, ok := name.(string); ok && s.mayBrowse(req, repos.Repo{Name: repo, Format: "docker"}) {
				ports = append(ports, port)
			}
		}
		status.Set("docker_ports", ports)
	}
	if scan, ok := status.Get("scan"); ok {
		status.Set("scan", withoutScanErrors(scan.(*pyjson.Object)))
	}
	return status
}

// withoutScanErrors is a copy of the scan state with the error text of a
// failed scan replaced; the outcomes of the job stay as they are.
func withoutScanErrors(scan *pyjson.Object) *pyjson.Object {
	out := pyjson.NewObject()
	for key, value := range scan.All() {
		last, ok := value.(*pyjson.Object)
		if key != "last" || !ok {
			out.Set(key, value)
			continue
		}
		outcomes := pyjson.NewObject()
		for store, outcome := range last.All() {
			copied := pyjson.NewObject()
			for field, v := range outcome.(*pyjson.Object).All() {
				if text, ok := v.(string); ok && field == "error" && text != "cancelled" {
					v = hiddenScanError
				}
				copied.Set(field, v)
			}
			outcomes.Set(store, copied)
		}
		out.Set(key, outcomes)
	}
	return out
}
