package rest

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The component upload of POST /v1/components (task 15 of the phase 4
// plan) as far as it needs no store: the field lists of the formats that
// take an upload, the multipart field names of an upload, the paths of its
// assets and the POM of Maven coordinates. The field names are those of the
// public documentation of the old product, page Components API, section
// Upload Component (read 2026-11-18); a format the public page Uploading
// Components lists as taking no upload, docker among them, has no field
// list. The server reads the body, spools the files and writes them through
// its ingest.

// MaxUploadAssets is the most files one upload of a format with
// multipleUpload carries: assetN with N from 1 to 3, as the REST
// description of the old product names them.
const MaxUploadAssets = 3

// The types of an upload field.
const (
	fieldString  = "STRING"
	fieldBoolean = "BOOLEAN"
)

// uploadFieldXO is UploadFieldDefinitionXO: one field of a component or of
// an asset.
type uploadFieldXO struct {
	Name        string  `json:"name"`
	Type        string  `json:"type"`
	Description string  `json:"description"`
	Optional    bool    `json:"optional"`
	Group       *string `json:"group"`
}

// uploadDefinitionXO is UploadDefinitionXO: the fields of the upload of a
// format.
type uploadDefinitionXO struct {
	Format          string          `json:"format"`
	MultipleUpload  bool            `json:"multipleUpload"`
	ComponentFields []uploadFieldXO `json:"componentFields"`
	AssetFields     []uploadFieldXO `json:"assetFields"`
}

func field(name, kind, description string, optional bool, group string) uploadFieldXO {
	f := uploadFieldXO{Name: name, Type: kind, Description: description, Optional: optional}
	if group != "" {
		f.Group = &group
	}
	return f
}

// uploadDefinitions are the formats that take an upload, by name.
var uploadDefinitions = []uploadDefinitionXO{
	{Format: "maven2", MultipleUpload: true,
		ComponentFields: []uploadFieldXO{
			field("groupId", fieldString, "The group of the component; a POM among the files can give it", true, "Coordinates"),
			field("artifactId", fieldString, "The artifact of the component; a POM among the files can give it", true, "Coordinates"),
			field("version", fieldString, "The version of the component; a POM among the files can give it", true, "Coordinates"),
			field("generate-pom", fieldBoolean, "Write a POM file with these coordinates", true, "Coordinates"),
			field("packaging", fieldString, "The packaging of the POM that is written, such as jar or war", true, "Coordinates"),
		},
		AssetFields: []uploadFieldXO{
			field("classifier", fieldString, "The classifier of the file, such as sources", true, ""),
			field("extension", fieldString, "The extension of the file, such as jar or pom", false, ""),
		}},
	{Format: "npm", ComponentFields: []uploadFieldXO{}, AssetFields: []uploadFieldXO{}},
	{Format: "raw", MultipleUpload: true,
		ComponentFields: []uploadFieldXO{
			field("directory", fieldString, "The folder of the files in the repository", false, "Folder"),
		},
		AssetFields: []uploadFieldXO{
			field("filename", fieldString, "The name of the file in the folder", false, ""),
		}},
}

func definitionOf(format string) *uploadDefinitionXO {
	for i := range uploadDefinitions {
		if uploadDefinitions[i].Format == format {
			return &uploadDefinitions[i]
		}
	}
	return nil
}

// Uploadable reports a format that takes an upload.
func Uploadable(format string) bool { return definitionOf(format) != nil }

// UploadSpecs is GET /v1/formats/upload-specs: the field list of every
// format that takes an upload.
func UploadSpecs() Answer {
	return Answer{Status: http.StatusOK, Body: uploadDefinitions}
}

// UploadSpec is GET /v1/formats/{format}/upload-specs, 404 for a format
// that takes no upload.
func UploadSpec(format string) Answer {
	if d := definitionOf(format); d != nil {
		return Answer{Status: http.StatusOK, Body: d}
	}
	return Answer{Status: http.StatusNotFound, Body: messageBody{"format " + format + " takes no upload"}}
}

// UploadField is what one multipart field of an upload names: a field of
// the component (Asset 0), the file of an asset or a field of it (Asset 1
// to MaxUploadAssets; 1 for the one asset of a format without
// multipleUpload).
type UploadField struct {
	Asset int
	File  bool
	Name  string // the field of the component or of the asset; "" for a file
}

func fieldNamed(fields []uploadFieldXO, name string) bool {
	for _, f := range fields {
		if f.Name == name {
			return true
		}
	}
	return false
}

// ParseUploadField reads a multipart field name of an upload of a format:
// <format>.<field> for the component; <format>.assetN and
// <format>.assetN.<field> for a format with multipleUpload, else
// <format>.asset and <format>.asset.<field>. False for any other name.
func ParseUploadField(format, name string) (UploadField, bool) {
	d := definitionOf(format)
	rest, ok := strings.CutPrefix(name, format+".")
	if d == nil || !ok {
		return UploadField{}, false
	}
	head, tail, sub := strings.Cut(rest, ".")
	if index, isAsset := assetIndex(head, d.MultipleUpload); isAsset {
		if !sub {
			return UploadField{Asset: index, File: true}, true
		}
		if fieldNamed(d.AssetFields, tail) {
			return UploadField{Asset: index, Name: tail}, true
		}
		return UploadField{}, false
	}
	if fieldNamed(d.ComponentFields, rest) {
		return UploadField{Name: rest}, true
	}
	return UploadField{}, false
}

// assetIndex is N of assetN, 1 to MaxUploadAssets without a leading zero,
// or 1 for "asset" of a format without multipleUpload.
func assetIndex(head string, multiple bool) (int, bool) {
	if !multiple {
		return 1, head == "asset"
	}
	digits, ok := strings.CutPrefix(head, "asset")
	if !ok || len(digits) != 1 {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 1 || n > MaxUploadAssets {
		return 0, false
	}
	return n, true
}

// UploadFieldName is the multipart name of a field of an upload.
func UploadFieldName(format string, f UploadField) string {
	asset := "asset"
	if d := definitionOf(format); d != nil && d.MultipleUpload {
		asset += strconv.Itoa(f.Asset)
	}
	switch {
	case f.Asset == 0:
		return format + "." + f.Name
	case f.File:
		return format + "." + asset
	}
	return format + "." + asset + "." + f.Name
}

// UploadFieldNames are every multipart name an upload of a format takes,
// in the order of its field list: the component fields, then per asset its
// file and its fields.
func UploadFieldNames(format string) []string {
	d := definitionOf(format)
	if d == nil {
		return nil
	}
	var out []string
	for _, f := range d.ComponentFields {
		out = append(out, UploadFieldName(format, UploadField{Name: f.Name}))
	}
	assets := 1
	if d.MultipleUpload {
		assets = MaxUploadAssets
	}
	for n := 1; n <= assets; n++ {
		out = append(out, UploadFieldName(format, UploadField{Asset: n, File: true}))
		for _, f := range d.AssetFields {
			out = append(out, UploadFieldName(format, UploadField{Asset: n, Name: f.Name}))
		}
	}
	return out
}

// FieldError is ValidationErrorXO: the field a refusal names, "*" for the
// whole request, and the text.
type FieldError struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

// Invalid is the 400 of an upload the fields refuse: the list of field
// errors, the form the interface contract reads (docs/ui/API.md, section
// Conventions).
func Invalid(errs ...FieldError) Answer {
	return Answer{Status: http.StatusBadRequest, Body: errs}
}

// badText refuses a value that cannot be part of a path: empty, "." or
// "..", a slash or a backslash, a control character, or no UTF-8.
func badText(value string) string {
	switch {
	case value == "":
		return "is empty"
	case value == "." || value == "..":
		return "is " + strconv.Quote(value)
	case !utf8.ValidString(value):
		return "is no UTF-8"
	case strings.ContainsAny(value, "/\\"):
		return "holds a slash or a backslash"
	case strings.IndexFunc(value, unicode.IsControl) >= 0:
		return "holds a control character"
	}
	return ""
}

// RawDirectory is the folder of a raw upload as a path below the root of
// the repository, its empty segments left out ("" for the root), and a
// text that says why a segment cannot be a folder.
func RawDirectory(directory string) (string, string) {
	var parts []string
	for _, segment := range strings.Split(directory, "/") {
		if segment == "" {
			continue
		}
		if problem := badText(segment); problem != "" {
			return "", problem
		}
		parts = append(parts, segment)
	}
	return strings.Join(parts, "/"), ""
}

// RawFileName says why a file name of a raw upload cannot name a file of
// its folder: a path among them; "" for a good name.
func RawFileName(name string) string { return badText(name) }

// MavenCoordinates are the coordinates of a Maven upload.
type MavenCoordinates struct {
	GroupID, ArtifactID, Version, Packaging string
}

// Check refuses coordinates that do not make a path of the Maven layout;
// prefix is the start of the field names (maven2.).
func (c MavenCoordinates) Check(prefix string) []FieldError {
	var errs []FieldError
	for _, f := range []struct{ name, value string }{{"groupId", c.GroupID}, {"artifactId", c.ArtifactID}, {"version", c.Version}} {
		if f.value == "" {
			errs = append(errs, FieldError{ID: prefix + f.name, Message: f.name + " is needed: give the field or a POM among the files"})
			continue
		}
		parts := []string{f.value}
		if f.name == "groupId" {
			parts = strings.Split(f.value, ".")
		}
		for _, part := range parts {
			if problem := badText(part); problem != "" {
				errs = append(errs, FieldError{ID: prefix + f.name, Message: f.name + " " + strconv.Quote(f.value) + " " + problem})
				break
			}
		}
	}
	if c.Packaging != "" {
		if problem := badText(c.Packaging); problem != "" {
			errs = append(errs, FieldError{ID: prefix + "packaging", Message: "the packaging " + problem})
		}
	}
	return errs
}

// MavenPath is the path of a file of the coordinates in the Maven layout:
// <group dirs>/<artifact>/<version>/<artifact>-<version>[-<classifier>].<extension>.
func MavenPath(c MavenCoordinates, classifier, extension string) string {
	name := c.ArtifactID + "-" + c.Version
	if classifier != "" {
		name += "-" + classifier
	}
	return strings.Join([]string{strings.ReplaceAll(c.GroupID, ".", "/"), c.ArtifactID, c.Version, name + "." + extension}, "/")
}

// CheckAssetPart refuses a classifier or an extension that cannot be part
// of a file name of the Maven layout.
func CheckAssetPart(name, value string) string {
	if value == "" {
		return ""
	}
	if problem := badText(value); problem != "" {
		return "the " + name + " " + problem
	}
	if strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") || strings.HasPrefix(value, "-") {
		return "the " + name + " starts or ends with a dot or starts with a hyphen"
	}
	return ""
}

// GeneratedPOM is the POM the upload writes for coordinates when it is
// asked to: the model version and the coordinates, with the packaging when
// one is known.
func GeneratedPOM(c MavenCoordinates) []byte {
	var b bytes.Buffer
	text := func(element, value string) {
		b.WriteString("  <" + element + ">")
		_ = xml.EscapeText(&b, []byte(value)) // a bytes.Buffer does not fail
		b.WriteString("</" + element + ">\n")
	}
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString(`<project xmlns="http://maven.apache.org/POM/4.0.0" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"` +
		` xsi:schemaLocation="http://maven.apache.org/POM/4.0.0 https://maven.apache.org/xsd/maven-4.0.0.xsd">` + "\n")
	text("modelVersion", "4.0.0")
	text("groupId", c.GroupID)
	text("artifactId", c.ArtifactID)
	text("version", c.Version)
	if c.Packaging != "" {
		text("packaging", c.Packaging)
	}
	b.WriteString("</project>\n")
	return b.Bytes()
}

// MaxPOMBytes bounds what the upload reads of a POM among its files.
const MaxPOMBytes = 1 << 20

type pomXML struct {
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Version    string `xml:"version"`
	Packaging  string `xml:"packaging"`
	Parent     struct {
		GroupID string `xml:"groupId"`
		Version string `xml:"version"`
	} `xml:"parent"`
}

// ReadPOM reads the coordinates of a POM: group and version of the parent
// where the project has none of its own. A POM above MaxPOMBytes or one
// that is no XML document is refused.
func ReadPOM(r io.Reader) (MavenCoordinates, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxPOMBytes+1))
	if err != nil {
		return MavenCoordinates{}, err
	}
	if len(data) > MaxPOMBytes {
		return MavenCoordinates{}, fmt.Errorf("the POM is larger than %d bytes", MaxPOMBytes)
	}
	var p pomXML
	if err := xml.Unmarshal(data, &p); err != nil {
		return MavenCoordinates{}, errors.New("the POM is no XML document: " + err.Error())
	}
	c := MavenCoordinates{GroupID: strings.TrimSpace(p.GroupID), ArtifactID: strings.TrimSpace(p.ArtifactID),
		Version: strings.TrimSpace(p.Version), Packaging: strings.TrimSpace(p.Packaging)}
	if c.GroupID == "" {
		c.GroupID = strings.TrimSpace(p.Parent.GroupID)
	}
	if c.Version == "" {
		c.Version = strings.TrimSpace(p.Parent.Version)
	}
	return c, nil
}
