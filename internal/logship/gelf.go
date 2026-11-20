package logship

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// The adapter gelf: one GELF 1.1 message per line, with the fields version,
// host, short_message, timestamp, level, _kind, _logger and _line. Over udp
// a message above 8,192 bytes goes in chunks of at most 8,192 bytes, 128 at
// most; over tcp each message ends with a null byte; over http and https
// each message is one POST with Content-Type application/json. Nothing is
// compressed.

func init() { register(kindGELF, openGELF) }

const (
	gelfDatagram    = 8192 // the most bytes of one datagram
	gelfChunkHead   = 12   // the magic bytes, the message id, the sequence number and the count
	gelfMaxChunks   = 128
	contentTypeJSON = "application/json"
)

type gelfAdapter struct {
	link  *link      // udp and tcp
	web   *webClient // http and https
	host  string
	clock clock
}

// gelfPayload is the message of a line, its keys in this order.
type gelfPayload struct {
	Version      string      `json:"version"`
	Host         string      `json:"host"`
	ShortMessage string      `json:"short_message"`
	Timestamp    json.Number `json:"timestamp"`
	Level        int         `json:"level"`
	Kind         string      `json:"_kind"`
	Logger       string      `json:"_logger"`
	Line         string      `json:"_line"`
}

func openGELF(t Target, secret string, e env) (Adapter, error) {
	a := &gelfAdapter{host: e.host, clock: e.clock}
	if scheme := t.endpoint().Scheme; scheme == "http" || scheme == "https" {
		web, err := newWebClient(t, secret, e)
		if err != nil {
			return nil, err
		}
		a.web = web
		return a, nil
	}
	l, err := newLink(t)
	if err != nil {
		return nil, err
	}
	a.link = l
	return a, nil
}

func (a *gelfAdapter) Send(ctx context.Context, batch []logs.Line) (Result, error) {
	switch {
	case a.web != nil:
		for i, line := range batch {
			ans, err := a.web.post(ctx, contentTypeJSON, a.message(line), nil, maxAnswer)
			if err == nil {
				err = a.web.check(ans)
			}
			if err != nil {
				return Result{Retry: batch[i:]}, err // the lines before i were taken
			}
		}
		return Result{}, nil
	case a.link.network == "udp":
		var res Result
		for i, line := range batch {
			datagrams := gelfChunks(a.message(line))
			if datagrams == nil {
				res.Failed++
				continue
			}
			for _, d := range datagrams {
				if err := a.link.write(ctx, d); err != nil {
					res.Retry = batch[i:]
					return res, err
				}
			}
		}
		return res, nil
	}
	var stream []byte
	for _, line := range batch {
		stream = append(append(stream, a.message(line)...), 0)
	}
	return Result{}, a.link.write(ctx, stream)
}

func (a *gelfAdapter) Close() error {
	if a.web != nil {
		a.web.close()
		return nil
	}
	a.link.close()
	return nil
}

// message is the GELF message of a line: timestamp the time of the line in
// seconds with three decimals, level its syslog severity. A msg that holds
// only white space is sent as "-", as short_message may not be blank.
func (a *gelfAdapter) message(line logs.Line) []byte {
	at := lineTime(line, a.clock)
	short := line.Message
	if strings.TrimFunc(short, func(r rune) bool { return unicode.IsSpace(r) || r == 0xfeff }) == "" {
		short = "-"
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(gelfPayload{Version: "1.1", Host: a.host, ShortMessage: short, // a struct of strings and numbers always encodes
		Timestamp: json.Number(fmt.Sprintf("%d.%03d", at.Unix(), at.Nanosecond()/1_000_000)), Level: severity(line.Level),
		Kind: line.Kind, Logger: line.Logger, Line: string(line.JSON)})
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'})
}

// gelfChunks is the datagrams of a message over udp: the message itself up
// to 8,192 bytes, else chunks of at most 8,192 bytes, each the magic bytes
// 0x1e 0x0f, a random message id of 8 bytes, its sequence number and the
// count, then its part. A message that needs more than 128 chunks has none.
func gelfChunks(msg []byte) [][]byte {
	if len(msg) <= gelfDatagram {
		return [][]byte{msg}
	}
	size := gelfDatagram - gelfChunkHead
	var count byte
	for rest := len(msg); rest > 0; rest -= size {
		if count == gelfMaxChunks {
			return nil
		}
		count++
	}
	head := []byte{0x1e, 0x0f, 0, 0, 0, 0, 0, 0, 0, 0, 0, count}
	_, _ = rand.Read(head[2:10]) // crypto/rand does not fail
	out := make([][]byte, 0, count)
	for seq := byte(0); seq < count; seq++ {
		head[10] = seq
		from := int(seq) * size
		part := msg[from:min(len(msg), from+size)]
		out = append(out, append(append(make([]byte, 0, gelfChunkHead+len(part)), head...), part...))
	}
	return out
}
