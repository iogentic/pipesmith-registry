package maven

import (
	"bytes"
	"strings"
	"testing"
)

// entityDocument names an entity of unit bytes refs times; the comment makes
// the document large enough that the rule of at most a hundred times the
// input lets the expansion through.
func entityDocument(paddingMiB, refs, unit int) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<!DOCTYPE metadata [<!ENTITY a "`)
	b.WriteString(strings.Repeat("A", unit))
	b.WriteString(`">]>` + "\n<metadata><!--")
	b.WriteString(strings.Repeat("x", paddingMiB<<20))
	b.WriteString("--><groupId>com.example</groupId><artifactId>demo-lib</artifactId><versioning><versions><version>")
	b.WriteString(strings.Repeat("&a;", refs))
	b.WriteString("</version></versions></versioning></metadata>")
	return b.Bytes()
}

func nestedDocument(depth int) []byte {
	return []byte("<metadata>" + strings.Repeat("<a>", depth-1) + strings.Repeat("</a>", depth-1) + "</metadata>")
}

const plainDocument = "<metadata><groupId>com.example</groupId><artifactId>demo-lib</artifactId>" +
	"<versioning><versions><version>1.0.0</version></versions></versioning></metadata>"

func TestEntityExpansionIsBounded(t *testing.T) {
	// 300 references of 30,000 bytes: 9,000,000 bytes, past 8 MiB.
	if root, err := parseMetadata(entityDocument(1, 300, 30_000), pythonCodec); err != nil || root != nil {
		t.Fatalf("expansion past the bound: root %v, err %v", root != nil, err)
	}
	// 250 references: 7,500,000 bytes, below 8 MiB.
	if root, err := parseMetadata(entityDocument(1, 250, 30_000), pythonCodec); err != nil || root == nil {
		t.Fatalf("expansion below the bound: root %v, err %v", root != nil, err)
	}
}

func TestElementDepthIsBounded(t *testing.T) {
	if root, err := parseMetadata(nestedDocument(maxElementDepth), pythonCodec); err != nil || root == nil {
		t.Fatalf("depth at the bound: root %v, err %v", root != nil, err)
	}
	if root, err := parseMetadata(nestedDocument(maxElementDepth+1), pythonCodec); err != nil || root != nil {
		t.Fatalf("depth past the bound: root %v, err %v", root != nil, err)
	}
	open := []byte("<metadata>" + strings.Repeat("<a>", 3_000_000))
	if root, err := parseMetadata(open, pythonCodec); err != nil || root != nil {
		t.Fatalf("open nesting: root %v, err %v", root != nil, err)
	}
}

func TestGroupMergeSkipsADocumentOverABound(t *testing.T) {
	for name, hostile := range map[string][]byte{
		"entity": entityDocument(1, 300, 30_000),
		"depth":  nestedDocument(maxElementDepth + 1),
	} {
		got, err := MergeMetadata([][]byte{[]byte(plainDocument), hostile})
		if err != nil || !bytes.Contains(got, []byte("<version>1.0.0</version>")) || bytes.Contains(got, []byte("AAAA")) {
			t.Errorf("%s: %d bytes, AAAA %v, err %v", name, len(got), bytes.Contains(got, []byte("AAAA")), err)
		}
	}
}

func wideDocument(elements int) []byte {
	return []byte("<metadata>" + strings.Repeat("<a/>", elements-1) + "</metadata>")
}

// defaultsDocument declares count attribute defaults for <a> and names
// <a/> elements times.
func defaultsDocument(count, elements int) []byte {
	var b strings.Builder
	b.WriteString("<!DOCTYPE metadata [")
	for n := range count {
		b.WriteString(`<!ATTLIST a a` + strings.Repeat("x", n+1) + ` CDATA "">`)
	}
	b.WriteString("]><metadata>" + strings.Repeat("<a/>", elements) + "</metadata>")
	return []byte(b.String())
}

func entityElements(unit, refs int) []byte {
	return []byte(`<!DOCTYPE metadata [<!ENTITY a "` + strings.Repeat("<a/>", unit) + `">]><metadata>` +
		strings.Repeat("&a;", refs) + "</metadata>")
}

func TestTreeIsBounded(t *testing.T) {
	for _, c := range []struct {
		name     string
		document []byte
		parses   bool
	}{
		{"elements at the bound", wideDocument(maxElements), true},
		{"elements past the bound", wideDocument(maxElements + 1), false},
		{"defaults at the bound", defaultsDocument(10, maxAttributes/10), true},
		{"defaults past the bound", defaultsDocument(10, maxAttributes/10+1), false},
		{"entity elements below the bound", entityElements(1_000, 199), true},
		{"entity elements past the bound", entityElements(1_000, 201), false},
	} {
		root, err := parseMetadata(c.document, pythonCodec)
		if err != nil || (root != nil) != c.parses {
			t.Errorf("%s: root %v, err %v", c.name, root != nil, err)
		}
	}
	got, err := MergeMetadata([][]byte{[]byte(plainDocument), defaultsDocument(1_850, 12_500)})
	if err != nil || !bytes.Contains(got, []byte("<version>1.0.0</version>")) {
		t.Errorf("merge beside a document past the bound: %d bytes, err %v", len(got), err)
	}
}
