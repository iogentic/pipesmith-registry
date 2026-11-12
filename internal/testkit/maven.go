package testkit

import (
	"crypto/md5" // #nosec G501 -- MD5 is the .md5 checksum file of Maven, not a security use.
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"math"
	"strings"
)

// MavenArtifact returns pom and jar with their .sha1 and .md5 files. Keys
// are repository paths.
func MavenArtifact(group, artifact, version string) map[string][]byte {
	base := strings.ReplaceAll(group, ".", "/") + "/" + artifact + "/" + version + "/" + artifact + "-" + version
	pom := []byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
		"<project>\n" +
		"  <modelVersion>4.0.0</modelVersion>\n" +
		"  <groupId>" + group + "</groupId>\n" +
		"  <artifactId>" + artifact + "</artifactId>\n" +
		"  <version>" + version + "</version>\n" +
		"</project>\n")
	manifest := []byte("Implementation-Title: " + artifact + "\nImplementation-Version: " + version + "\n")
	jar := storedZip("META-INF/MANIFEST.MF", manifest)
	out := map[string][]byte{}
	for _, f := range []struct {
		suffix  string
		content []byte
	}{{".pom", pom}, {".jar", jar}} {
		md5sum := md5.Sum(f.content) // #nosec G401 -- the .md5 checksum file of Maven.
		out[base+f.suffix] = f.content
		out[base+f.suffix+".sha1"] = []byte(SHA1Hex(f.content))
		out[base+f.suffix+".md5"] = []byte(hex.EncodeToString(md5sum[:]))
	}
	return out
}

// storedZip is the archive Python's zipfile writes for one entry written
// with writestr(ZipInfo(name, date_time=(2023, 11, 14, 22, 13, 20)), data)
// into a seekable buffer: stored, no data descriptor, made on Unix, version
// 2.0, permissions 0600.
func storedZip(name string, data []byte) []byte {
	const (
		version      = 20
		unix         = 3
		externalAttr = 0o600 << 16
		dosTime      = 22<<11 | 13<<5 | 20/2
		dosDate      = (2023-1980)<<9 | 11<<5 | 14
	)
	crc := crc32.ChecksumIEEE(data)
	le := binary.LittleEndian
	var b []byte
	b = append(b, "PK\x03\x04"...)
	b = append(b, version, 0)
	b = le.AppendUint16(b, 0) // flags
	b = le.AppendUint16(b, 0) // stored
	b = le.AppendUint16(b, dosTime)
	b = le.AppendUint16(b, dosDate)
	b = le.AppendUint32(b, crc)
	b = le.AppendUint32(b, u32(len(data)))
	b = le.AppendUint32(b, u32(len(data)))
	b = le.AppendUint16(b, u16(len(name)))
	b = le.AppendUint16(b, 0) // extra
	b = append(b, name...)
	b = append(b, data...)
	directory := len(b)
	b = append(b, "PK\x01\x02"...)
	b = append(b, version, unix, version, 0)
	b = le.AppendUint16(b, 0)
	b = le.AppendUint16(b, 0)
	b = le.AppendUint16(b, dosTime)
	b = le.AppendUint16(b, dosDate)
	b = le.AppendUint32(b, crc)
	b = le.AppendUint32(b, u32(len(data)))
	b = le.AppendUint32(b, u32(len(data)))
	b = le.AppendUint16(b, u16(len(name)))
	b = le.AppendUint16(b, 0) // extra
	b = le.AppendUint16(b, 0) // comment
	b = le.AppendUint16(b, 0) // disk
	b = le.AppendUint16(b, 0) // internal attributes
	b = le.AppendUint32(b, externalAttr)
	b = le.AppendUint32(b, 0) // offset of the local header
	b = append(b, name...)
	size := len(b) - directory
	b = append(b, "PK\x05\x06"...)
	b = le.AppendUint16(b, 0)
	b = le.AppendUint16(b, 0)
	b = le.AppendUint16(b, 1)
	b = le.AppendUint16(b, 1)
	b = le.AppendUint32(b, u32(size))
	b = le.AppendUint32(b, u32(directory))
	b = le.AppendUint16(b, 0) // comment
	return b
}

// u32 and u16 are the zip header fields; an archive without zip64 has no
// room for more, so a larger value stops the test helper.
func u32(n int) uint32 {
	if n < 0 || n > math.MaxUint32 {
		panic("testkit: value does not fit a zip header field without zip64")
	}
	return uint32(n)
}

func u16(n int) uint16 {
	if n < 0 || n > math.MaxUint16 {
		panic("testkit: value does not fit a zip header field without zip64")
	}
	return uint16(n)
}
