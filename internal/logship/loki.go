package logship

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strconv"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// The adapter loki: one POST per batch of a push request of the Loki HTTP
// API in JSON, compressed with gzip. The lines go into one stream per pair of
// kind and level, with the labels kind, level and instance, the host name;
// the values of a stream are in time order, each the time of its line in
// nanoseconds as a string and the line. Any 2xx answer is success.

func init() { register(kindLoki, openLoki) }

type lokiAdapter struct {
	web   *webClient
	host  string
	clock clock
}

// lokiStream is one stream of a push: its labels and its values.
type lokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

func openLoki(t Target, secret string, e env) (Adapter, error) {
	web, err := newWebClient(t, secret, e)
	if err != nil {
		return nil, err
	}
	return &lokiAdapter{web: web, host: e.host, clock: e.clock}, nil
}

func (a *lokiAdapter) Send(ctx context.Context, batch []logs.Line) (Result, error) {
	body, err := a.push(batch)
	if err != nil {
		return Result{}, &Failure{Class: "request", Final: true}
	}
	ans, err := a.web.post(ctx, contentTypeJSON, body, http.Header{"Content-Encoding": {"gzip"}}, maxAnswer)
	if err != nil {
		return Result{}, err
	}
	return Result{}, a.web.check(ans)
}

func (a *lokiAdapter) Close() error {
	a.web.close()
	return nil
}

// push is the body of a batch: the push request in JSON, compressed with
// gzip. The streams follow the order in which their first line came.
func (a *lokiAdapter) push(batch []logs.Line) ([]byte, error) {
	type value struct {
		at   int64
		line string
	}
	type stream struct {
		kind, level string
		values      []value
	}
	var order []*stream
	index := map[[2]string]*stream{}
	for _, line := range batch {
		key := [2]string{line.Kind, levelText(line.Level)}
		s := index[key]
		if s == nil {
			s = &stream{kind: key[0], level: key[1]}
			index[key] = s
			order = append(order, s)
		}
		s.values = append(s.values, value{lineTime(line, a.clock).UnixNano(), string(line.JSON)})
	}
	streams := make([]lokiStream, 0, len(order))
	for _, s := range order {
		slices.SortStableFunc(s.values, func(x, y value) int { return cmp.Compare(x.at, y.at) })
		values := make([][2]string, len(s.values))
		for i, v := range s.values {
			values[i] = [2]string{strconv.FormatInt(v.at, 10), v.line}
		}
		streams = append(streams, lokiStream{Stream: map[string]string{"kind": s.kind, "level": s.level, "instance": a.host},
			Values: values})
	}
	data, err := json.Marshal(struct {
		Streams []lokiStream `json:"streams"`
	}{streams})
	if err != nil {
		return nil, err
	}
	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	if _, err := zw.Write(data); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return body.Bytes(), nil
}

// levelText is the name of the level of a line: DEBUG, INFO, WARN or ERROR;
// a level between two of them takes the lower one.
func levelText(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "ERROR"
	case level >= slog.LevelWarn:
		return "WARN"
	case level >= slog.LevelInfo:
		return "INFO"
	}
	return "DEBUG"
}
