package npm

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

// publishBody is a publish document as libnpmpublish writes it, with the
// attachment last.
func publishBody(data []byte, extra string) string {
	return `{"_id":"@demo/pkg","name":"@demo/pkg","description":"a package","dist-tags":{"latest":"1.0.0"},` +
		`"versions":{"1.0.0":{"name":"@demo/pkg","version":"1.0.0","dist":{"shasum":"x"}}},"access":null` + extra +
		`,"_attachments":{"pkg-1.0.0.tgz":{"content_type":"application/octet-stream","data":"` +
		base64.StdEncoding.EncodeToString(data) + `","length":` + strconv.Itoa(len(data)) + `}}}`
}

func collect(t *testing.T, body io.Reader) (*pyjson.Object, []Attachment, map[string]*bytes.Buffer, error) {
	t.Helper()
	got := map[string]*bytes.Buffer{}
	document, attachments, err := ReadPublish(body, func(name string) (io.Writer, error) {
		got[name] = &bytes.Buffer{}
		return got[name], nil
	})
	return document, attachments, got, err
}

// The data of an attachment streams to its writer; every other value is in
// the document, in its order, and _attachments is not.
func TestReadPublishStreamsTheAttachment(t *testing.T) {
	data := bytes.Repeat([]byte("tarball bytes \x00\xff"), 70_000)
	document, attachments, got, err := collect(t, iotest.OneByteReader(strings.NewReader(publishBody(data, ""))))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(document.Keys(), ",") != "_id,name,description,dist-tags,versions,access" {
		t.Fatalf("keys %v", document.Keys())
	}
	if len(attachments) != 1 || attachments[0] != (Attachment{Name: "pkg-1.0.0.tgz", ContentType: "application/octet-stream",
		Length: int64(len(data)), Size: int64(len(data))}) {
		t.Fatalf("%+v", attachments)
	}
	if !bytes.Equal(got["pkg-1.0.0.tgz"].Bytes(), data) {
		t.Fatal("the data differs")
	}
	// the attachment first, escaped slashes in its data, white space around
	encoded := strings.ReplaceAll(base64.StdEncoding.EncodeToString(data), "/", `\/`)
	first := " { \"_attachments\" : { \"a.tgz\" : { \"data\" : \"" + encoded + "\" } } , \"name\" : \"@demo/pkg\" } \n"
	document, attachments, got, err = collect(t, strings.NewReader(first))
	if err != nil || !bytes.Equal(got["a.tgz"].Bytes(), data) || attachments[0].Length != -1 || strings.Join(document.Keys(), ",") != "name" {
		t.Fatalf("%v %+v %v", err, attachments, document.Keys())
	}
}

// What the reader refuses, each as a *Refusal; the read stops there.
func TestReadPublishRefusals(t *testing.T) {
	deep := strings.Repeat("[", maxPublishDepth+1) + strings.Repeat("]", maxPublishDepth+1)
	for _, c := range []struct{ name, body, text string }{
		{"no object", `[1]`, "is a JSON object"},
		{"cut", `{"name": "x", "versions": {`, "ends before"},
		{"empty", ``, "ends before"},
		{"after the end", `{"name": "x"} {}`, "goes on after its end"},
		{"two _attachments", `{"_attachments": {}, "_attachments": {}}`, "twice"},
		{"no JSON value", `{"name": nope}`, "is not JSON"},
		{"too deep", `{"versions": ` + deep + `}`, "nested deeper"},
		{"bracket", `{"versions": {"a": [1}}`, "did not open"},
		{"no data", `{"_attachments": {"a.tgz": {"length": 3}}}`, "holds no data"},
		{"not base64", `{"_attachments": {"a.tgz": {"data": "!!!!"}}}`, "not base64"},
		{"data after padding", `{"_attachments": {"a.tgz": {"data": "YQ==YQ=="}}}`, "not base64"},
		{"control character", "{\"_attachments\": {\"a.tgz\": {\"data\": \"YQ\x01=\"}}}", "control character"},
		{"length", `{"_attachments": {"a.tgz": {"data": "YQ==", "length": -1}}}`, "no count of bytes"},
		{"content type", `{"_attachments": {"a.tgz": {"data": "YQ==", "content_type": 7}}}`, "is not a text"},
		{"key", `{7: 1}`, "is not a string"},
	} {
		_, _, _, err := collect(t, strings.NewReader(c.body))
		var refusal *Refusal
		if !errors.As(err, &refusal) || !strings.Contains(refusal.Message, c.text) {
			t.Errorf("%s: %v, want a refusal with %q", c.name, err, c.text)
		}
	}
}

// The JSON outside the data of attachments is bounded; the data is not
// read into memory and so it is not.
func TestReadPublishBoundsTheDocument(t *testing.T) {
	big := `{"readme": "` + strings.Repeat("r", MaxPublishDocument) + `"}`
	_, _, _, err := collect(t, strings.NewReader(big))
	var refusal *Refusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Message, "bytes besides the data") {
		t.Fatalf("%v", err)
	}
	data := bytes.Repeat([]byte{7}, MaxPublishDocument+1024)
	if _, attachments, _, err := collect(t, strings.NewReader(publishBody(data, ""))); err != nil || attachments[0].Size != int64(len(data)) {
		t.Fatalf("%v %+v", err, attachments)
	}
}

// An error of the body, of open and of the writer comes back as it is.
func TestReadPublishPassesErrorsOn(t *testing.T) {
	cut := errors.New("the connection is gone")
	body := io.MultiReader(strings.NewReader(publishBody([]byte("data"), "")[:150]), iotest.ErrReader(cut))
	if _, _, _, err := collect(t, body); !errors.Is(err, cut) {
		t.Fatalf("body: %v", err)
	}
	full := errors.New("full")
	_, _, err := ReadPublish(strings.NewReader(publishBody([]byte("data"), "")), func(string) (io.Writer, error) { return nil, full })
	if !errors.Is(err, full) {
		t.Fatalf("open: %v", err)
	}
	_, _, err = ReadPublish(strings.NewReader(publishBody([]byte("data"), "")), func(string) (io.Writer, error) {
		return failingWriter{full}, nil
	})
	if !errors.Is(err, full) {
		t.Fatalf("writer: %v", err)
	}
}

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }
