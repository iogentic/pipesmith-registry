package server

// The words of the OpenAPI document for the routes of figures_routes.go.
var _ = addWords(map[string]words{
	"GET /api/metrics": {path: "/api/metrics", tag: "Status", summary: "Counters since the start",
		answers: []reply{ok200("{requests, bytesReceived, bytesSent, status, downloads, downloadBytes, flushFailures, logTargets}; " +
			"status counts the answers per class 1xx to 5xx; each log target is {name, kind, queueDepth, queued, sent, dropped, " +
			"failed, rejected, cut, attempts, errors}.")}},
	"GET /api/metrics/requests": {path: "/api/metrics/requests", tag: "Status", summary: "Requests per day",
		params: []param{query("from", "string", "First day, YYYY-MM-DD in UTC; without it 29 days before to."),
			query("to", "string", "Last day, included, YYYY-MM-DD in UTC; without it today.")},
		answers: []reply{ok200("{from, to, days}; one entry {day, requests, bytesOut} per day of the span, 0 for a day without requests."),
			errorBody("400", `{"message"}: a day that does not read, from after to, or more than 366 days.`)}},
	"GET /api/metrics/downloads": {path: "/api/metrics/downloads", tag: "Status", summary: "Downloads per repository and day",
		params: []param{query("from", "string", "First day, YYYY-MM-DD in UTC; without it 29 days before to."),
			query("to", "string", "Last day, included, YYYY-MM-DD in UTC; without it today.")},
		answers: []reply{ok200("{from, to, rows}; one row {repository, day, count, bytes} per repository and day with downloads."),
			errorBody("400", `{"message"}: a day that does not read, from after to, or more than 366 days.`)}},
	"GET /api/figures": {path: "/api/figures", tag: "Status", summary: "Requests, traffic, CPU and memory over time",
		params: []param{{name: "range", in: "query", typ: "string", meaning: "Span of the series.", def: "24h", enum: []string{"24h", "7d", "30d"}}},
		answers: []reply{ok200("{enabled, range, stepSeconds, from, to, points}; each point {time, requests, bytesSent, bytesReceived, " +
			"cpuSeconds, memoryBytes} sums the samples of its step; enabled is false and points empty while the samples are off."),
			errorBody("400", `{"message"}: a range other than 24h, 7d and 30d.`)}},
})
