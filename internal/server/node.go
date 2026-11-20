package server

import (
	"net/http"
	"net/url"

	"github.com/iogentic/pipesmith-registry/internal/rest"
)

// nodeXO is the answer of GET /v1/system/node, in the shape of the
// interface contract (S37).
type nodeXO struct {
	NodeID string `json:"nodeId"`
}

// nodeAnswer is GET /v1/system/node: the id of this server.
func (s *Server) nodeAnswer(w *recorder, req *request, _ []string) error {
	return s.restAnswer(w, req, func(*rest.Env, url.Values) (rest.Answer, error) {
		return rest.Answer{Status: http.StatusOK, Body: nodeXO{NodeID: s.nodeID()}}, nil
	})
}
