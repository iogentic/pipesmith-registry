package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/rest"
)

// The status area of task 7.22 of the phase 7 plan, beside the three paths
// of step 2 and phase 1c (status, status/writable, status/check): the checks
// of the node and of the cluster of one node, and the requests of the last
// 12 months. Each is read with the permission of the health checks.

const (
	restBetaBase = "/service/rest/beta"
	atlasRead    = "nexus:atlas:read"
)

var statusAreaRoutes = []route{
	get(restBetaBase+"/status/check/cluster", perm(atlasRead), (*Server).clusterChecks),
	get(restBetaBase+"/status/check/"+segment, perm(atlasRead), (*Server).nodeChecks),
	get(restBase+"/monthly-metrics", perm(atlasRead), (*Server).monthlyMetrics),
}

// nodeCheck is one check in the answers of the beta paths.
type nodeCheck struct {
	Healthy bool   `json:"healthy"`
	Message string `json:"message"`
}

// nodeChecks is the answer for one node.
type nodeChecksXO struct {
	NodeID   string               `json:"nodeId"`
	Hostname string               `json:"hostname"`
	Results  map[string]nodeCheck `json:"results"`
}

// nodeID names this server: the id of package node, read at the start.
func (s *Server) nodeID() string { return s.node }

// checksOfNode are the checks of GET /v1/status/check as the beta paths
// carry them: healthy and message.
func (s *Server) checksOfNode(req *request) (nodeChecksXO, error) {
	env, err := s.restEnv(req)
	if err != nil {
		return nodeChecksXO{}, err
	}
	a, err := rest.StatusChecks(env)
	if err != nil {
		return nodeChecksXO{}, err
	}
	data, err := a.JSON()
	if err != nil {
		return nodeChecksXO{}, err
	}
	results := map[string]nodeCheck{}
	if err := json.Unmarshal(data, &results); err != nil {
		return nodeChecksXO{}, err
	}
	host, _ := os.Hostname() // a machine without a name answers with an empty name
	return nodeChecksXO{NodeID: s.nodeID(), Hostname: host, Results: results}, nil
}

func (s *Server) clusterChecks(w *recorder, req *request, _ []string) error {
	node, err := s.checksOfNode(req)
	if err != nil {
		return err
	}
	return s.writeAnswer(w, req, rest.Answer{Status: http.StatusOK, Body: []nodeChecksXO{node}})
}

func (s *Server) nodeChecks(w *recorder, req *request, match []string) error {
	id, err := url.PathUnescape(match[1])
	if err != nil || id != s.nodeID() {
		s.message(w, req, http.StatusNotFound, "no node with this id")
		return nil
	}
	node, err := s.checksOfNode(req)
	if err != nil {
		return err
	}
	return s.writeAnswer(w, req, rest.Answer{Status: http.StatusOK, Body: node})
}

// monthXO is one month of GET /v1/monthly-metrics.
type monthXO struct {
	RequestCount              int64  `json:"requestCount"`
	ComponentCount            int64  `json:"componentCount"`
	MetricDate                string `json:"metricDate"`
	PercentageChangeRequest   string `json:"percentageChangeRequest"`
	PercentageChangeComponent string `json:"percentageChangeComponent"`
}

// monthlyMetrics is the last 12 months up to the current one, the oldest
// first, from request_days (L12). The server keeps no count of components
// per month, so componentCount is 0 and its change "N/A".
func (s *Server) monthlyMetrics(w *recorder, req *request, _ []string) error {
	now := s.figures.Now().UTC()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -11, 0)
	days, err := s.figures.RequestDays(first.Format(catalog.DayLayout), now.Format(catalog.DayLayout))
	if err != nil {
		return err
	}
	months := make([]monthXO, 12)
	for i := range months {
		months[i] = monthXO{MetricDate: first.AddDate(0, i, 0).Format(time.RFC3339), PercentageChangeComponent: "N/A"}
	}
	for _, d := range days {
		day, err := time.Parse(catalog.DayLayout, d.Day)
		if err != nil {
			continue
		}
		if i := (day.Year()-first.Year())*12 + int(day.Month()) - int(first.Month()); i >= 0 && i < len(months) {
			months[i].RequestCount += d.Requests
		}
	}
	for i := range months {
		months[i].PercentageChangeRequest = change(months, i)
	}
	return s.writeAnswer(w, req, rest.Answer{Status: http.StatusOK, Body: months})
}

// change is the change of the request count against the month before, in
// percent with two decimals, "N/A" when that month had none.
func change(months []monthXO, i int) string {
	if i == 0 || months[i-1].RequestCount == 0 {
		return "N/A"
	}
	before := float64(months[i-1].RequestCount)
	return fmt.Sprintf("%.2f%%", (float64(months[i].RequestCount)-before)/before*100)
}

// dataWritable reports whether the data directory takes a new file; tests
// replace the access check, because root writes in spite of the modes.
func (s *Server) dataWritable() bool {
	access := s.dataAccess
	if access == nil {
		access = syscall.Access
	}
	return access(s.rp.cfg.DataDir, accessWrite) == nil
}
