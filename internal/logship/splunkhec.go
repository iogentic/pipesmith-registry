package logship

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// The adapter splunk-hec: one POST per batch to the event endpoint of the
// HTTP Event Collector, one event object per line with a line break between
// two, each with time in epoch seconds with three decimals, host, source the
// kind, sourcetype pipesmith:registry and event the JSON object of the line.
// Content-Type application/json, no channel, no acknowledgment. A 400 with
// invalid-event-number k took the events before k, as the collector sends
// those and drops the rest.

func init() { register(kindSplunkHEC, openSplunkHEC) }

const splunkHECSourcetype = "pipesmith:registry"

type splunkHECAdapter struct {
	web   *webClient
	host  []byte // the host name as a JSON string
	clock clock
}

func openSplunkHEC(t Target, secret string, e env) (Adapter, error) {
	web, err := newWebClient(t, secret, e)
	if err != nil {
		return nil, err
	}
	host, err := json.Marshal(e.host)
	if err != nil {
		return nil, err
	}
	return &splunkHECAdapter{web: web, host: host, clock: e.clock}, nil
}

func (a *splunkHECAdapter) Send(ctx context.Context, batch []logs.Line) (Result, error) {
	var body bytes.Buffer
	for i, line := range batch {
		if i > 0 {
			body.WriteByte('\n')
		}
		a.event(&body, line)
	}
	ans, err := a.web.post(ctx, contentTypeJSON, body.Bytes(), nil, maxAnswer)
	if err != nil {
		return Result{}, err
	}
	err = a.web.check(ans)
	if err == nil || ans.status != http.StatusBadRequest {
		return Result{}, err
	}
	if k, ok := invalidEventNumber(ans.body); ok && k < len(batch) {
		return Result{Retry: batch[k:]}, err
	}
	return Result{}, err
}

func (a *splunkHECAdapter) Close() error {
	a.web.close()
	return nil
}

// event writes the event object of line.
func (a *splunkHECAdapter) event(b *bytes.Buffer, line logs.Line) {
	ms := lineTime(line, a.clock).UnixMilli()
	b.WriteString(`{"time":`)
	b.WriteString(strconv.FormatInt(ms/1000, 10))
	b.WriteByte('.')
	frac := strconv.FormatInt(ms%1000, 10)
	for range 3 - len(frac) {
		b.WriteByte('0')
	}
	b.WriteString(frac)
	b.WriteString(`,"host":`)
	b.Write(a.host)
	b.WriteString(`,"source":`)
	source, _ := json.Marshal(line.Kind)
	b.Write(source)
	b.WriteString(`,"sourcetype":"` + splunkHECSourcetype + `","event":`)
	if json.Valid(line.JSON) {
		b.Write(line.JSON)
	} else {
		// A line that is not JSON goes as a string, so the batch stays valid.
		text, _ := json.Marshal(string(line.JSON))
		b.Write(text)
	}
	b.WriteByte('}')
}

// invalidEventNumber reads invalid-event-number of the answer of the
// collector, the zero based index of the first invalid event.
func invalidEventNumber(body []byte) (int, bool) {
	var answer map[string]json.RawMessage
	if json.Unmarshal(body, &answer) != nil {
		return 0, false
	}
	var k int
	if err := json.Unmarshal(answer["invalid-event-number"], &k); err != nil || k < 0 {
		return 0, false
	}
	return k, true
}
