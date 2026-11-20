package server

// The words of the OpenAPI document for the routes of logs_routes.go.

const logsNote = "Only server.log, requests.log, requests-classic.log, audit.log, tasks.log and their rotated files " +
	"<kind>-<day>.<n>.log and .log.gz are served; a link, a folder and every other name are not."

const levelsNote = "The level of a logger applies to its lines in server.log and to the log targets; standard error keeps INFO. " +
	"The levels are the setting log_levels of state.sqlite and hold after a restart. No logger is added."

var (
	logsUnavailable = message("503", "The log files and levels of this server are not open.")
	loggerName      = map[string]string{"name": "Name of a logger of this server, as GET /api/logging/loggers lists it."}
	loggerRefused   = errorBody("400", `[{"id", "message"}]: id name for a name this server does not have, level for a level other than DEBUG, INFO, WARN and ERROR, * for a body that is not JSON.`)
)

var _ = addWords(map[string]words{
	"GET /api/logging/loggers": {path: "/api/logging/loggers", tag: "Status", summary: "Loggers and their levels",
		description: levelsNote,
		answers:     []reply{ok200("Array of {name, level, default}, sorted by name; level and default are DEBUG, INFO, WARN or ERROR."), logsUnavailable}},
	"PUT /api/logging/loggers/{}": {path: "/api/logging/loggers/{name}", tag: "Status", summary: "Set the level of a logger",
		description: levelsNote, pathWords: loggerName,
		body:    &bodyWords{ctype: jsonType, text: "{level}: DEBUG, INFO, WARN or ERROR.", fields: []string{"level"}},
		answers: []reply{{status: "204", text: "The level is stored and applies."}, loggerRefused, logsUnavailable}},
	"DELETE /api/logging/loggers/{}": {path: "/api/logging/loggers/{name}", tag: "Status", summary: "Put a logger back to its default",
		description: levelsNote, pathWords: loggerName,
		answers: []reply{{status: "204", text: "The logger is at its default."}, loggerRefused, logsUnavailable}},
	"POST /api/logging/reset": {path: "/api/logging/reset", tag: "Status", summary: "Put every logger back to its default",
		description: levelsNote,
		answers:     []reply{{status: "204", text: "Every logger is at its default."}, logsUnavailable}},
	"GET /api/logs": {path: "/api/logs", tag: "Status", summary: "Log files",
		description: logsNote,
		answers: []reply{ok200("Array of {name, size, lastModified}, sorted by name; name is relative to the log directory, " +
			"lastModified an ISO 8601 time in UTC. Empty while the files are off."), logsUnavailable}},
	"GET /api/logs/{+}": {path: "/api/logs/{name}", tag: "Status", summary: "End of a log file, or the whole file",
		description: logsNote + " The end of a .log.gz file is the end of its lines after decompression; the download is the file as it is on disk.",
		pathWords:   map[string]string{"name": "Name of a log file as GET /api/logs lists it."},
		params: []param{{name: "bytes", in: "query", typ: "integer", meaning: "Length of the end of the file.", def: 25600, enum: []string{"25600", "51200", "102400"}},
			query("download", "string", "1 answers the whole file as an attachment.")},
		answers: []reply{{status: "200", ctype: "text/plain; charset=utf-8", text: "The last bytes of the file; with download=1 the whole file with " +
			"Content-Disposition: attachment, as application/gzip for a .log.gz file."},
			message("400", "A name with .., a leading slash, a slash or backslash, a control byte, a link, or another value of bytes or download."),
			message("404", "No log file of that name, also for a name that is no log file."), logsUnavailable}},
})
