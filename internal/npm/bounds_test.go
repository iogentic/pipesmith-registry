package npm

import (
	"archive/tar"
	"bytes"
	"math/big"
	"strconv"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

func rawHeader(name string, size int, typ byte) []byte {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	_ = tw.WriteHeader(&tar.Header{Name: name, Size: int64(size), Mode: 0o644, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR})
	h := raw.Bytes()[:512]
	h[156] = typ
	var sum int64
	copy(h[148:156], "        ")
	for _, c := range h {
		sum += int64(c)
	}
	copy(h[148:156], []byte(strings.Repeat("0", 6-len(itoa8(sum)))+itoa8(sum)+"\x00 "))
	return h
}

func padded(b []byte) []byte { return append(b, make([]byte, (512-len(b)%512)%512)...) }

// withManifest is the raw tar of the members before it, then package.json
// and the end of the archive.
func withManifest(before ...[]byte) []byte {
	var raw []byte
	for _, b := range before {
		raw = append(raw, b...)
	}
	body := manifestJSON("1.0.0")
	raw = append(raw, rawHeader(PackageJSON, len(body), '0')...)
	raw = append(raw, padded(append([]byte(nil), body...))...)
	return append(raw, make([]byte, 1024)...)
}

// paxRecordOf is one pax record "%d comment=%s\n" of exactly length bytes.
func paxRecordOf(length int) []byte {
	digits := len(strconv.Itoa(length))
	value := strings.Repeat("v", length-digits-len(" comment=\n"))
	return []byte(strconv.Itoa(length) + " comment=" + value + "\n")
}

func TestExtendedHeaderDataIsBounded(t *testing.T) {
	pax := func(size int) []byte {
		record := paxRecordOf(size)
		return gz(t, withManifest(rawHeader("././@PaxHeader", len(record), 'x'), padded(record)))
	}
	longName := func(size int) []byte {
		name := append([]byte("package/"+strings.Repeat("n", 100)), make([]byte, size-108)...)
		other := rawHeader("package/other", 0, '0')
		return gz(t, withManifest(rawHeader("././@LongLink", size, 'L'), padded(name), other))
	}
	for kind, archive := range map[string]func(int) []byte{"pax": pax, "long name": longName} {
		if m, err := PackageManifest(archive(maxHeaderData)); m == nil || err != nil {
			t.Errorf("%s data of %d bytes: %v, %v", kind, maxHeaderData, m, err)
		}
		if m, err := PackageManifest(archive(maxHeaderData + 1)); m != nil || err != nil {
			t.Errorf("%s data of %d bytes: %v, %v", kind, maxHeaderData+1, m, err)
		}
	}
}

func TestSparseMapIsBounded(t *testing.T) {
	// old GNU sparse: every extension block but the last sets the flag at 504
	gnu := func(blocks int) error {
		var raw []byte
		for i := range blocks {
			b := make([]byte, 512)
			if i < blocks-1 {
				b[504] = 1
			}
			raw = append(raw, b...)
		}
		w := &walker{s: newStream(gz(t, raw)), offset: new(big.Int), global: &paxMap{}}
		_, err := w.procSparse(&member{typ: 'S', size: new(big.Int), origsize: new(big.Int), extended: true})
		return err
	}
	if err := gnu(maxHeaderData / 512); err != nil {
		t.Errorf("GNU sparse map of %d bytes: %v", maxHeaderData, err)
	}
	if err := gnu(maxHeaderData/512 + 1); classOf(err) != "ReadError" {
		t.Errorf("GNU sparse map past %d bytes: %v", maxHeaderData, err)
	}
	// sparse format 1.0: a count, then offset and size per line
	pax10 := func(fields int) error {
		raw := []byte(strconv.Itoa(fields) + "\n" + strings.Repeat("0\n", 2*fields))
		w := &walker{s: newStream(gz(t, padded(raw))), offset: new(big.Int), global: &paxMap{}}
		return w.sparse10(&member{size: new(big.Int)})
	}
	if err := pax10(200_000); err != nil {
		t.Errorf("sparse 1.0 map of 800,007 bytes: %v", err)
	}
	if err := pax10(300_000); classOf(err) != "ReadError" {
		t.Errorf("sparse 1.0 map of 1,200,007 bytes: %v", err)
	}
}

func TestInflatedBytesAreBounded(t *testing.T) {
	large := append(rawHeader("package/large.bin", 128<<10, '0'), make([]byte, 128<<10)...)
	archive := gz(t, withManifest(large))
	if m, err := PackageManifest(archive); m == nil || err != nil {
		t.Fatalf("archive below the bound: %v, %v", m, err)
	}
	defer func(old int64) { maxInflated = old }(maxInflated)
	maxInflated = 64 << 10
	if m, err := PackageManifest(archive); m != nil || err != nil {
		t.Errorf("archive past the bound: %v, %v", m, err)
	}
}

func TestExtendedHeaderSumIsBounded(t *testing.T) {
	archive := func(typ byte, count int) []byte {
		var headers [][]byte
		for range count {
			record := paxRecordOf(maxHeaderData)
			headers = append(headers, rawHeader("././@PaxHeader", len(record), typ), padded(record))
		}
		return gz(t, withManifest(headers...))
	}
	for _, typ := range []byte{'x', 'g'} {
		if m, err := PackageManifest(archive(typ, maxHeaderTotal/maxHeaderData)); m == nil || err != nil {
			t.Errorf("%c: %d MiB of headers: %v, %v", typ, maxHeaderTotal>>20, m, err)
		}
		if m, err := PackageManifest(archive(typ, maxHeaderTotal/maxHeaderData+1)); m != nil || err != nil {
			t.Errorf("%c: %d MiB of headers: %v, %v", typ, maxHeaderTotal>>20+1, m, err)
		}
	}
}

func TestDocumentInflateBudget(t *testing.T) {
	versions := []string{"1.0.0", "1.1.0", "1.2.0"}
	var tarballs []Tarball
	for i, v := range versions {
		data := tgz(t, file{name: PackageJSON, data: manifestJSON(v)})
		tarballs = append(tarballs, Tarball{FileName: "demo-lib-" + v + ".tgz", Data: data, CreatedMS: int64(1_700_000_000_000 + i)})
	}
	keys := func() string {
		doc, err := DocumentFromTarballs("demo-lib", tarballs)
		if err != nil || doc == nil {
			t.Fatalf("DocumentFromTarballs: %v, %v", doc, err)
		}
		versions, _ := doc.Get("versions")
		return strings.Join(versions.(*pyjson.Object).Keys(), ",")
	}
	if got := keys(); got != "1.0.0,1.1.0,1.2.0" {
		t.Fatalf("with the whole budget: %s", got)
	}
	// what one archive of these inflates
	probe := &inflateBudget{left: 1 << 40}
	if m, err := packageManifest(tarballs[0].Data, probe); m == nil || err != nil {
		t.Fatalf("probe: %v, %v", m, err)
	}
	one := 1<<40 - probe.left
	defer func(old int64) { maxDocumentInflated = old }(maxDocumentInflated)
	maxDocumentInflated = 2*one + one/2
	if got := keys(); got != "1.0.0,1.1.0" {
		t.Errorf("with a budget of %d bytes for archives of %d: %s", maxDocumentInflated, one, got)
	}
}
