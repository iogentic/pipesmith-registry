package logship

import (
	"bytes"
	"context"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// The adapter http-json, a format of this server: one POST per batch to the
// address as it is, the body the JSON lines each followed by a line break,
// Content-Type application/x-ndjson. Any 2xx answer is success.

func init() { register(kindHTTPJSON, openHTTPJSON) }

const contentTypeNDJSON = "application/x-ndjson"

type httpJSON struct{ web *webClient }

func openHTTPJSON(t Target, secret string, e env) (Adapter, error) {
	web, err := newWebClient(t, secret, e)
	if err != nil {
		return nil, err
	}
	return &httpJSON{web: web}, nil
}

func (a *httpJSON) Send(ctx context.Context, batch []logs.Line) (Result, error) {
	var body bytes.Buffer
	for _, line := range batch {
		body.Write(line.JSON)
		body.WriteByte('\n')
	}
	ans, err := a.web.post(ctx, contentTypeNDJSON, body.Bytes(), nil, maxAnswer)
	if err != nil {
		return Result{}, err
	}
	return Result{}, a.web.check(ans)
}

func (a *httpJSON) Close() error {
	a.web.close()
	return nil
}
