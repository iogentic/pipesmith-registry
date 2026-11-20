package logship

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// The adapter elasticsearch, for OpenSearch as well: one POST per batch to
// the bulk API, Content-Type application/x-ndjson, for each line the action
// {"create":{}} and the JSON object of the line with @timestamp, the time of
// the line, as its first key; the body ends with a line break. With errors
// true the items are read: an item of 429 is sent again within the attempts
// of the batch, every other item of 400 and above counts as rejected.

func init() { register(kindElasticsearch, openElasticsearch) }

const bulkAction = `{"create":{}}` + "\n"

type elasticsearchAdapter struct {
	web   *webClient
	clock clock
}

func openElasticsearch(t Target, secret string, e env) (Adapter, error) {
	web, err := newWebClient(t, secret, e)
	if err != nil {
		return nil, err
	}
	return &elasticsearchAdapter{web: web, clock: e.clock}, nil
}

func (a *elasticsearchAdapter) Send(ctx context.Context, batch []logs.Line) (Result, error) {
	var body bytes.Buffer
	for _, line := range batch {
		body.WriteString(bulkAction)
		a.source(&body, line)
		body.WriteByte('\n')
	}
	// The answer holds one item per line: 64 KiB plus 1 KiB per line.
	ans, err := a.web.post(ctx, contentTypeNDJSON, body.Bytes(), nil, maxAnswer+int64(len(batch))<<10)
	if err != nil {
		return Result{}, err
	}
	if err := a.web.check(ans); err != nil {
		return Result{}, err
	}
	statuses, ok := bulkItems(ans.body, len(batch))
	if !ok {
		return Result{}, &Failure{Class: "answer", Final: true}
	}
	var res Result
	for i, status := range statuses {
		switch {
		case status == 429:
			res.Retry = append(res.Retry, batch[i])
		case status >= 400:
			res.Rejected++
		}
	}
	return res, nil
}

func (a *elasticsearchAdapter) Close() error {
	a.web.close()
	return nil
}

// source writes the document of line: its JSON object with @timestamp first.
func (a *elasticsearchAdapter) source(b *bytes.Buffer, line logs.Line) {
	b.WriteString(`{"@timestamp":"`)
	b.WriteString(lineTime(line, a.clock).Format("2006-01-02T15:04:05.000Z"))
	b.WriteByte('"')
	data := bytes.TrimSpace(line.JSON)
	if len(data) < 2 || data[0] != '{' || !json.Valid(data) {
		// A line that is not a JSON object goes as the string message.
		text, _ := json.Marshal(string(line.JSON))
		b.WriteString(`,"message":`)
		b.Write(text)
		b.WriteByte('}')
		return
	}
	rest := bytes.TrimSpace(data[1:])
	if rest[0] != '}' {
		b.WriteByte(',')
	}
	b.Write(rest)
}

// bulkItems is the status of each item of a bulk answer, all 0 when errors
// is not true. false when the answer cannot be read, or its items do not
// match the n actions of the request.
func bulkItems(body []byte, n int) ([]int, bool) {
	var answer map[string]json.RawMessage
	if json.Unmarshal(body, &answer) != nil {
		return nil, false
	}
	var failed bool
	if json.Unmarshal(answer["errors"], &failed) != nil {
		return nil, false
	}
	statuses := make([]int, n)
	if !failed {
		return statuses, true
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(answer["items"], &items) != nil || len(items) != n {
		return nil, false
	}
	for i, item := range items {
		if len(item) != 1 {
			return nil, false
		}
		for _, raw := range item {
			var result map[string]json.RawMessage
			if json.Unmarshal(raw, &result) != nil || json.Unmarshal(result["status"], &statuses[i]) != nil {
				return nil, false
			}
		}
	}
	return statuses, true
}
