package logship

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// The adapter otlp: one POST per batch of an ExportLogsServiceRequest of
// OTLP/HTTP in JSON, without compression. One resource with service.name
// pipesmith-registry and service.instance.id the host name, one scope per
// kind named by the kind, each record with timeUnixNano as a decimal string,
// severityNumber and severityText of the level, and the line as the
// stringValue of its body. A 200 with partialSuccess counts its
// rejectedLogRecords as rejected.

func init() { register(kindOTLP, openOTLP) }

// otlpMaxRequest is the bound of a request body; a batch above it is not
// sent (the specification recommends 64 MiB).
var otlpMaxRequest = 64 << 20

type otlpAdapter struct {
	web   *webClient
	host  string
	clock clock
}

type otlpValue struct {
	StringValue string `json:"stringValue"`
}

type otlpKeyValue struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpRecord struct {
	TimeUnixNano   string    `json:"timeUnixNano"`
	SeverityNumber int       `json:"severityNumber"`
	SeverityText   string    `json:"severityText"`
	Body           otlpValue `json:"body"`
}

type otlpScopeLogs struct {
	Scope struct {
		Name string `json:"name"`
	} `json:"scope"`
	LogRecords []otlpRecord `json:"logRecords"`
}

type otlpResourceLogs struct {
	Resource struct {
		Attributes []otlpKeyValue `json:"attributes"`
	} `json:"resource"`
	ScopeLogs []otlpScopeLogs `json:"scopeLogs"`
}

func openOTLP(t Target, secret string, e env) (Adapter, error) {
	web, err := newWebClient(t, secret, e)
	if err != nil {
		return nil, err
	}
	return &otlpAdapter{web: web, host: e.host, clock: e.clock}, nil
}

func (a *otlpAdapter) Send(ctx context.Context, batch []logs.Line) (Result, error) {
	body, err := a.export(batch)
	if err != nil || len(body) > otlpMaxRequest {
		return Result{}, &Failure{Class: "request", Final: true}
	}
	// One byte above the bound tells a longer answer, which the
	// specification treats as an error that is not retried.
	ans, err := a.web.post(ctx, contentTypeJSON, body, nil, maxAnswer+1)
	if err != nil {
		return Result{}, err
	}
	if err := a.web.check(ans); err != nil {
		return Result{}, err
	}
	rejected, ok := otlpRejected(ans.body)
	if !ok {
		return Result{}, &Failure{Class: "answer", Final: true}
	}
	return Result{Rejected: int(min(rejected, int64(len(batch))))}, nil
}

func (a *otlpAdapter) Close() error {
	a.web.close()
	return nil
}

// export is the body of a batch; the scopes follow the order in which their
// first line came.
func (a *otlpAdapter) export(batch []logs.Line) ([]byte, error) {
	var res otlpResourceLogs
	res.Resource.Attributes = []otlpKeyValue{
		{Key: "service.name", Value: otlpValue{"pipesmith-registry"}},
		{Key: "service.instance.id", Value: otlpValue{a.host}},
	}
	index := map[string]int{}
	for _, line := range batch {
		i, ok := index[line.Kind]
		if !ok {
			i = len(res.ScopeLogs)
			index[line.Kind] = i
			res.ScopeLogs = append(res.ScopeLogs, otlpScopeLogs{})
			res.ScopeLogs[i].Scope.Name = line.Kind
		}
		level := levelText(line.Level)
		res.ScopeLogs[i].LogRecords = append(res.ScopeLogs[i].LogRecords, otlpRecord{
			TimeUnixNano:   strconv.FormatInt(lineTime(line, a.clock).UnixNano(), 10),
			SeverityNumber: otlpSeverityOf(line.Level),
			SeverityText:   level,
			Body:           otlpValue{string(line.JSON)},
		})
	}
	return json.Marshal(struct {
		ResourceLogs []otlpResourceLogs `json:"resourceLogs"`
	}{[]otlpResourceLogs{res}})
}

// otlpSeverityOf is the severityNumber of a level: DEBUG 5, INFO 9, WARN 13,
// ERROR 17, the first of each range of the data model.
func otlpSeverityOf(level slog.Level) int {
	switch levelText(level) {
	case "ERROR":
		return 17
	case "WARN":
		return 13
	case "INFO":
		return 9
	}
	return 5
}

// otlpRejected reads rejectedLogRecords of partialSuccess in the body of a
// 2xx answer, a decimal string or a number; an empty body, an empty object
// and a key left out are 0. false for a body above the bound or not JSON.
func otlpRejected(body []byte) (int64, bool) {
	if len(body) > maxAnswer {
		return 0, false
	}
	if len(body) == 0 {
		return 0, true
	}
	// Maps keep the exact names: a struct would take a key in another case.
	var answer, partial map[string]json.RawMessage
	if err := json.Unmarshal(body, &answer); err != nil {
		return 0, false
	}
	if json.Unmarshal(answer["partialSuccess"], &partial) != nil {
		return 0, true
	}
	raw, ok := partial["rejectedLogRecords"]
	if !ok {
		return 0, true
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		text = string(raw)
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n < 0 {
		return 0, true
	}
	return n, true
}
