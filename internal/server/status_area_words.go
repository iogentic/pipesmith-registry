package server

// The words of the OpenAPI document for the routes of status_area.go. A
// package variable is set before any init function, so the document of
// openapi.go finds them.
var _ = addWords(map[string]words{
	"GET /service/rest/beta/status/check/cluster": {path: "/service/rest/beta/status/check/cluster", tag: "Status",
		summary: "Health checks of every node",
		answers: []reply{ok200("Array with one entry for this server, {nodeId, hostname, results}; results holds the checks of " +
			"/v1/status/check as {healthy, message}.")}},
	"GET /service/rest/beta/status/check/{}": {path: "/service/rest/beta/status/check/{nodeId}", tag: "Status",
		summary:   "Health checks of one node",
		pathWords: map[string]string{"nodeId": "Id of the node, as GET /v1/system/node answers it."},
		answers: []reply{ok200("{nodeId, hostname, results}, as one entry of the cluster checks."),
			errorBody("404", `{"message"}: no node has this id.`)}},
	"GET /service/rest/v1/monthly-metrics": {path: restV1 + "/monthly-metrics", tag: "Status", summary: "Requests per month",
		answers: []reply{ok200("Array of the 12 months up to the current one, oldest first, each {requestCount, componentCount, " +
			"metricDate, percentageChangeRequest, percentageChangeComponent}; componentCount is 0 and its change N/A.")}},
})

// addWords adds the words of routes defined outside openapi_words.go.
func addWords(more map[string]words) bool {
	for key, w := range more {
		operationWords[key] = w
	}
	return true
}
