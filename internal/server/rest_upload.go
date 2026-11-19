package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/maven"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/npm"
	"github.com/iogentic/pipesmith-registry/internal/policy"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/rest"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The component upload of POST /v1/components?repository=<name> (task 15
// of the phase 4 plan). The multipart body streams part by part: a file
// goes into a work file in content/tmp of the write store as it comes, a
// field is read up to maxUploadFieldBytes. The field list of the format
// bounds the parts: a part whose name is no field of it, or names one a
// second time, is refused at once, so an upload has at most 14 parts
// (maven2 with three files). Each file is bounded by the limit of the
// repository, and every read waits at most chunkWait (write_body.go). Once
// every part is read and every field checked, the files are written through
// ingest: raw and maven2 one blob per file, maven2 with the .md5 and .sha1
// files of each, a POM made from the coordinates when the upload asks for
// one, and the metadata of the artifact; npm as a publish of the tarball
// with its package document.

const (
	// maxUploadFieldBytes bounds the value of one field that is no file.
	maxUploadFieldBytes = 4096
	// maxUnpackedTarball bounds the bytes an npm upload unpacks to find the
	// package.json of its tarball.
	maxUnpackedTarball = 1 << 30
)

// uploadForm is an upload body as read: the values of its fields by
// multipart name and the work file of each asset by its number.
type uploadForm struct {
	values map[string]string
	files  map[int]*spoolFile
}

func (f *uploadForm) remove() {
	for _, sp := range f.files {
		sp.remove()
	}
}

// formRefusal is an upload body the server refuses while it reads it.
type formRefusal struct {
	status         int
	field, message string
}

func (e *formRefusal) Error() string { return e.message }

// partReader keeps the first error of reading a part, so that a failure of
// the body is told from one of the work file it goes into.
type partReader struct {
	r   io.Reader
	err error
}

func (p *partReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if err != nil && !errors.Is(err, io.EOF) && p.err == nil {
		p.err = err
	}
	return n, err
}

// readUploadForm reads the parts of an upload of a format. A part that is
// no field of the format, a field given twice, a field value above
// maxUploadFieldBytes, a file above limit and files past the room of the
// store are refused at once; nothing more of the body is read then.
func (s *Server) readUploadForm(body io.Reader, boundary, format string, limit int64, room *storeRoom) (*uploadForm, error) {
	form := &uploadForm{values: map[string]string{}, files: map[int]*spoolFile{}}
	seen := map[string]bool{}
	mr := multipart.NewReader(body, boundary)
	for {
		part, err := mr.NextRawPart()
		if errors.Is(err, io.EOF) {
			return form, nil
		}
		if err != nil {
			return form, &formRefusal{http.StatusBadRequest, "*", "the body is no multipart form: " + err.Error()}
		}
		name := part.FormName()
		field, known := rest.ParseUploadField(format, name)
		switch {
		case !known:
			return form, &formRefusal{http.StatusBadRequest, name, fmt.Sprintf("%q is no field of a %s upload", name, format)}
		case seen[name]:
			return form, &formRefusal{http.StatusBadRequest, name, fmt.Sprintf("the field %s is given twice", name)}
		}
		seen[name] = true
		in := &partReader{r: part}
		if field.File {
			sp, err := s.newSpool(limit, room)
			if err != nil {
				return form, err
			}
			form.files[field.Asset] = sp
			_, err = io.CopyBuffer(sp, in, make([]byte, copyBufferBytes))
			switch {
			case errors.Is(err, stores.ErrTooLarge):
				return form, &formRefusal{http.StatusRequestEntityTooLarge, name, fmt.Sprintf("the file %s is larger than the limit of %d bytes", name, limit)}
			case err != nil && in.err != nil:
				return form, &formRefusal{http.StatusBadRequest, name, "the body is no multipart form: " + in.err.Error()}
			case err != nil:
				return form, err
			}
			continue
		}
		data, err := io.ReadAll(io.LimitReader(in, maxUploadFieldBytes+1))
		if err != nil {
			return form, &formRefusal{http.StatusBadRequest, name, "the body is no multipart form: " + err.Error()}
		}
		if len(data) > maxUploadFieldBytes {
			return form, &formRefusal{http.StatusBadRequest, name, fmt.Sprintf("the field %s is longer than %d bytes", name, maxUploadFieldBytes)}
		}
		form.values[name] = string(data)
	}
}

// uploadFile is one file an upload writes: from a work file of the body,
// or made by the server.
type uploadFile struct {
	path  string
	field string     // the multipart name it comes from, for a refusal
	spool *spoolFile // nil for a file the server makes
	data  []byte
}

func (f uploadFile) open() (io.Reader, int64, error) {
	if f.spool == nil {
		return bytes.NewReader(f.data), int64(len(f.data)), nil
	}
	r, err := f.spool.reader()
	return r, f.spool.size, err
}

// uploadComponent is POST /v1/components?repository=<name>: 204 once every
// file of the upload is written. Before a byte of the body is read: 422
// without the repository, 404 for one that does not exist, 400 for a group,
// a proxy, a format that takes no upload (docker among them, as the public
// page Uploading Components of the old product lists it, read 2026-11-18)
// and the write policy read only, 415 for a body that is no multipart form,
// 507 below the free space floor.
func (s *Server) uploadComponent(w *recorder, req *request, _ []string) error {
	name, ok := uploadRepository(req)
	if !ok {
		s.message(w, req, http.StatusUnprocessableEntity, "the query parameter repository names the repository of the upload, once")
		return nil
	}
	repo, st, ok, err := s.hostedTarget(w, req, name, "repository "+name+" is not known")
	if err != nil || !ok {
		return err
	}
	if !rest.Uploadable(repo.Format) {
		text := "repository " + repo.Name + " has format " + repo.Format + ", which takes no upload through this route"
		if repo.Format == "docker" {
			text += "; push images with a client of the registry protocol"
		}
		s.message(w, req, http.StatusBadRequest, text)
		return nil
	}
	settings, err := s.writeSettingsOf(repo)
	if err != nil {
		return s.restWriteFailed(w, req, err)
	}
	if err := policy.Write(settings.WritePolicy, false, false); err != nil {
		return s.restWriteFailed(w, req, refusalOf(err))
	}
	mediaType, params, err := mime.ParseMediaType(req.r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		s.message(w, req, http.StatusUnsupportedMediaType, "an upload is a multipart/form-data body with its boundary")
		return nil
	}
	room, err := s.checkSpace(max(req.r.ContentLength, 0), settings.FreeSpaceFloor)
	if err != nil {
		return s.restWriteFailed(w, req, err)
	}
	subject, err := s.subjectOf(req)
	if err != nil {
		return err
	}
	body := newTimedBody(w, req)
	form, err := s.readUploadForm(body, params["boundary"], repo.Format, settings.MaxBodyBytes, room)
	defer form.remove()
	if err != nil {
		return s.uploadBodyFailed(w, req, repo, body, err)
	}
	up := &upload{s: s, w: w, req: req, st: st, repo: repo, settings: settings, form: form, subject: subject, address: peerAddress(w)}
	switch repo.Format {
	case "npm":
		err = up.npm()
	case "maven2":
		err = up.maven()
	default:
		err = up.raw()
	}
	if err != nil || up.answered {
		return err
	}
	noContent(w)
	return nil
}

// uploadBodyFailed answers a body the upload refused or that did not
// arrive in full; the rest of the body is not read.
func (s *Server) uploadBodyFailed(w *recorder, req *request, repo repos.Repo, body *timedBody, err error) error {
	w.Header().Set("Connection", "close")
	if status, cut := body.cut(); cut {
		logAt(slogWarn, serverLogger, "POST of an upload to %s: the body did not arrive in full, nothing is written: %v", repo.Name, body.err)
		s.message(w, req, status, "the body did not arrive in full; nothing is written")
		return nil
	}
	var space *writeRefusal
	if errors.As(err, &space) {
		s.message(w, req, space.status, space.message) // the files passed the room of the store
		return nil
	}
	var refused *formRefusal
	if !errors.As(err, &refused) {
		return err
	}
	if refused.status == http.StatusBadRequest {
		return s.writeAnswer(w, req, rest.Invalid(rest.FieldError{ID: refused.field, Message: refused.message}))
	}
	s.message(w, req, refused.status, refused.message)
	return nil
}

// upload is one upload whose body is read.
type upload struct {
	s        *Server
	w        *recorder
	req      *request
	st       *state
	repo     repos.Repo
	settings repos.WriteSettings
	form     *uploadForm
	subject  *model.Subject
	address  string
	answered bool // a refusal is written
}

// invalid answers the field errors of an upload.
func (u *upload) invalid(errs []rest.FieldError) error {
	u.answered = true
	return u.s.writeAnswer(u.w, u.req, rest.Invalid(errs...))
}

// failed answers a refusal of ingest or of a rule for the file of field.
func (u *upload) failed(field string, err error) error {
	var refusal *writeRefusal
	if errors.As(err, &refusal) {
		u.answered = true
		retryAfter(u.w, refusal)
		if refusal.status == http.StatusBadRequest {
			return u.s.writeAnswer(u.w, u.req, rest.Invalid(rest.FieldError{ID: field, Message: refusal.message}))
		}
		u.s.message(u.w, u.req, refusal.status, refusal.message)
		return nil
	}
	var rule *npm.Refusal
	if errors.As(err, &rule) {
		return u.invalid([]rest.FieldError{{ID: field, Message: rule.Message}})
	}
	return err
}

// check applies the policies of the repository to every file before one is
// written, and asks the right edit of the view permission for a path the
// repository holds, as a client write does (seam Permission of a write).
func (u *upload) check(files []uploadFile) error {
	var total int64
	for _, f := range files {
		if _, err := u.s.checkWrite(&ingestRequest{repo: u.repo, path: f.path}, u.settings, false); err != nil {
			return u.failed(f.field, err)
		}
		row, err := u.s.rp.index.Lookup(u.repo.Name, f.path)
		if err != nil {
			return err
		}
		if row != nil && !u.s.sec.Permits(u.subject, rest.ViewPermission(u.repo.Format, u.repo.Name, "edit")) {
			u.answered = true
			u.s.message(u.w, u.req, http.StatusForbidden, repositoryRefusal)
			return nil
		}
		_, size, err := f.open()
		if err != nil {
			return err
		}
		total += size
	}
	if _, err := u.s.checkSpace(total, u.settings.FreeSpaceFloor); err != nil {
		return u.failed("*", err)
	}
	return nil
}

// write writes the files through ingest, in their order. checked are files
// of the form that check took: under the lock of its path each asks edit
// again when the path exists then and the caller holds add only (finding S3
// of review 4); the checksum files a maven2 upload adds are not checked.
func (u *upload) write(files []uploadFile, checked bool) ([]*ingestResult, error) {
	var results []*ingestResult
	addOnly := checked && !u.s.mayEdit(u.subject, u.repo)
	for _, f := range files {
		body, size, err := f.open()
		if err != nil {
			return results, err
		}
		result, err := u.s.ingest(ingestRequest{repo: u.repo, path: f.path, body: body, size: size, addOnly: addOnly, subject: u.subject,
			address: u.address})
		if err != nil {
			return results, u.failed(f.field, err)
		}
		results = append(results, result)
	}
	return results, nil
}

// raw is the upload of a raw repository: raw.directory, and per file
// raw.assetN with raw.assetN.filename.
func (u *upload) raw() error {
	var errs []rest.FieldError
	directory, given := u.form.values["raw.directory"]
	dir, problem := rest.RawDirectory(directory)
	switch {
	case !given:
		errs = append(errs, rest.FieldError{ID: "raw.directory", Message: "the directory is needed"})
	case problem != "":
		errs = append(errs, rest.FieldError{ID: "raw.directory", Message: "a folder of the directory " + problem})
	}
	var files []uploadFile
	seen := map[string]bool{}
	for n := 1; n <= rest.MaxUploadAssets; n++ {
		file := rest.UploadFieldName("raw", rest.UploadField{Asset: n, File: true})
		nameField := rest.UploadFieldName("raw", rest.UploadField{Asset: n, Name: "filename"})
		sp, hasFile := u.form.files[n]
		filename, hasName := u.form.values[nameField]
		switch {
		case !hasFile && !hasName:
			continue
		case !hasFile:
			errs = append(errs, rest.FieldError{ID: nameField, Message: "a file name without the file " + file})
			continue
		case !hasName:
			errs = append(errs, rest.FieldError{ID: nameField, Message: "the file " + file + " needs its file name"})
			continue
		}
		if problem := rest.RawFileName(filename); problem != "" {
			errs = append(errs, rest.FieldError{ID: nameField, Message: "the file name " + problem + "; it names a file of the directory, no path"})
			continue
		}
		path := filename
		if dir != "" {
			path = dir + "/" + filename
		}
		if seen[path] {
			errs = append(errs, rest.FieldError{ID: nameField, Message: "two files of the upload have the path " + path})
			continue
		}
		seen[path] = true
		files = append(files, uploadFile{path: path, field: file, spool: sp})
	}
	if len(files) == 0 && len(errs) == 0 {
		errs = append(errs, rest.FieldError{ID: "raw.asset1", Message: "an upload carries at least one file"})
	}
	if len(errs) > 0 {
		return u.invalid(errs)
	}
	if err := u.check(files); err != nil || u.answered {
		return err
	}
	_, err := u.write(files, true)
	return err
}

// mavenAsset is one file of a maven2 upload with its fields.
type mavenAsset struct {
	field, classifier, extension string
	spool                        *spoolFile
}

// maven is the upload of a maven2 repository: the coordinates from the
// fields or from a POM among the files, per file maven2.assetN with its
// extension and classifier, a POM made from the coordinates on
// maven2.generate-pom, and the .md5 and .sha1 file of each file written.
func (u *upload) maven() error {
	const prefix = "maven2."
	values := u.form.values
	var errs []rest.FieldError
	c := rest.MavenCoordinates{GroupID: values[prefix+"groupId"], ArtifactID: values[prefix+"artifactId"], Version: values[prefix+"version"],
		Packaging: values[prefix+"packaging"]}
	generate := false
	if v, ok := values[prefix+"generate-pom"]; ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true":
			generate = true
		case "false", "":
		default:
			errs = append(errs, rest.FieldError{ID: prefix + "generate-pom", Message: "generate-pom is true or false"})
		}
	}
	var assets []mavenAsset
	pom := -1
	for n := 1; n <= rest.MaxUploadAssets; n++ {
		file := rest.UploadFieldName("maven2", rest.UploadField{Asset: n, File: true})
		extField := rest.UploadFieldName("maven2", rest.UploadField{Asset: n, Name: "extension"})
		clsField := rest.UploadFieldName("maven2", rest.UploadField{Asset: n, Name: "classifier"})
		sp, hasFile := u.form.files[n]
		extension, hasExt := values[extField]
		classifier, hasCls := values[clsField]
		if !hasFile {
			if hasExt || hasCls {
				errs = append(errs, rest.FieldError{ID: extField, Message: "fields of " + file + " without the file"})
			}
			continue
		}
		if extension == "" {
			errs = append(errs, rest.FieldError{ID: extField, Message: "the file " + file + " needs its extension"})
			continue
		}
		bad := false
		for _, part := range []struct{ id, name, value string }{{extField, "extension", extension}, {clsField, "classifier", classifier}} {
			if problem := rest.CheckAssetPart(part.name, part.value); problem != "" {
				errs, bad = append(errs, rest.FieldError{ID: part.id, Message: problem}), true
			}
		}
		if bad {
			continue
		}
		if extension == "pom" && classifier == "" {
			pom = len(assets)
		}
		assets = append(assets, mavenAsset{field: file, classifier: classifier, extension: extension, spool: sp})
	}
	if len(assets) == 0 && len(errs) == 0 {
		errs = append(errs, rest.FieldError{ID: prefix + "asset1", Message: "an upload carries at least one file"})
	}
	if pom >= 0 && generate {
		errs = append(errs, rest.FieldError{ID: prefix + "generate-pom", Message: "generate-pom makes a POM, and the upload carries one"})
	}
	if pom >= 0 && len(errs) == 0 {
		errs = append(errs, u.pomCoordinates(&c, assets[pom])...)
	}
	if len(errs) == 0 {
		errs = c.Check(prefix)
	}
	if len(errs) > 0 {
		return u.invalid(errs)
	}
	var files []uploadFile
	for _, a := range assets {
		files = append(files, uploadFile{path: rest.MavenPath(c, a.classifier, a.extension), field: a.field, spool: a.spool})
	}
	if generate {
		made := c
		if made.Packaging == "" {
			for _, a := range assets {
				if a.classifier == "" && a.extension != "pom" {
					made.Packaging = a.extension
					break
				}
			}
		}
		files = append(files, uploadFile{path: rest.MavenPath(c, "", "pom"), field: prefix + "generate-pom", data: rest.GeneratedPOM(made)})
	}
	seen := map[string]bool{}
	for _, f := range files {
		if seen[f.path] {
			return u.invalid([]rest.FieldError{{ID: f.field, Message: "two files of the upload have the path " + f.path}})
		}
		seen[f.path] = true
	}
	if err := u.check(files); err != nil || u.answered {
		return err
	}
	results, err := u.write(files, true)
	if err != nil || u.answered {
		return err
	}
	var sums []uploadFile
	for i, r := range results {
		sums = append(sums, uploadFile{path: r.asset.Path + ".md5", field: files[i].field, data: []byte(r.digests.MD5)},
			uploadFile{path: r.asset.Path + ".sha1", field: files[i].field, data: []byte(r.digests.SHA1)})
	}
	if _, err := u.write(sums, false); err != nil || u.answered {
		return err
	}
	// The metadata of the artifact lists the new version, as after a deploy
	// that sends it (W15); the files are written when it runs.
	if _, err := u.s.RebuildMavenMetadata(u.repo.Name, maven.Scope{GroupID: c.GroupID, ArtifactID: c.ArtifactID}, u.subject, u.address); err != nil {
		logAt(slogWarn, serverLogger, "the files of %s:%s:%s in %s are written, the metadata of the artifact is not: %v; run maven rebuild-metadata %s",
			c.GroupID, c.ArtifactID, c.Version, u.repo.Name, err, u.repo.Name)
	}
	return nil
}

// pomCoordinates takes the coordinates of a POM among the files: a field
// that is given must say what the POM says.
func (u *upload) pomCoordinates(c *rest.MavenCoordinates, pom mavenAsset) []rest.FieldError {
	r, err := pom.spool.reader()
	if err != nil {
		return []rest.FieldError{{ID: pom.field, Message: "the POM cannot be read: " + err.Error()}}
	}
	found, err := rest.ReadPOM(r)
	if err != nil {
		return []rest.FieldError{{ID: pom.field, Message: err.Error()}}
	}
	var errs []rest.FieldError
	for _, f := range []struct {
		name         string
		given, inPOM string
		target       *string
	}{{"groupId", c.GroupID, found.GroupID, &c.GroupID}, {"artifactId", c.ArtifactID, found.ArtifactID, &c.ArtifactID},
		{"version", c.Version, found.Version, &c.Version}} {
		switch {
		case f.given == "":
			*f.target = f.inPOM
		case f.inPOM != "" && f.inPOM != f.given:
			errs = append(errs, rest.FieldError{ID: "maven2." + f.name, Message: fmt.Sprintf("%s %q differs from %q of the POM", f.name, f.given, f.inPOM)})
		}
	}
	return errs
}

// npm is the upload of an npm repository: the tarball npm.asset, published
// as npm publish publishes it, under the name and the version of its
// package/package.json, with the tag latest.
func (u *upload) npm() error {
	const asset = "npm.asset"
	sp, ok := u.form.files[1]
	if !ok {
		return u.invalid([]rest.FieldError{{ID: asset, Message: "an upload carries the tarball " + asset}})
	}
	r, err := sp.reader()
	if err != nil {
		return err
	}
	manifest, problem := tarballManifest(r)
	if problem != "" {
		return u.invalid([]rest.FieldError{{ID: asset, Message: problem}})
	}
	name, _ := field(manifest, "name").(string)
	version, _ := field(manifest, "version").(string)
	if found, ok := npm.PackageName(name); !ok || found != name {
		return u.invalid([]rest.FieldError{{ID: asset, Message: fmt.Sprintf("package.json names the package %q, which is no npm package name", name)}})
	}
	if !npm.IsVersion(version) {
		return u.invalid([]rest.FieldError{{ID: asset, Message: fmt.Sprintf("package.json names the version %q, which is no version this server takes", version)}})
	}
	if err := u.check([]uploadFile{{path: npm.TarballPath(name, version), field: asset, spool: sp}}); err != nil || u.answered {
		return err
	}
	entry := pyjson.NewObject()
	for key, value := range manifest.All() {
		entry.Set(key, value)
	}
	entry.Set("name", name)
	entry.Set("version", version)
	published := &npm.Published{Version: version, Entry: entry, Tags: []string{"latest"}, Digests: map[string]string{}}
	unlock := u.s.npmLock(u.repo, name)
	defer unlock()
	stored, err := u.s.storedDocument(u.st, u.repo, name)
	if err != nil {
		return err
	}
	data, err := sp.reader()
	if err != nil {
		return err
	}
	if _, err := u.s.publishVersion(u.repo, name, stored, pyjson.NewObject().Set("name", name), published, data, sp.size, u.subject, u.address); err != nil {
		return u.failed(asset, err)
	}
	return nil
}

// tarballManifest is the package/package.json object of an npm tarball, the
// last of that name as the read path takes it, read as a stream of at most
// maxUnpackedTarball bytes; the text says why there is none.
func tarballManifest(r io.Reader) (*pyjson.Object, string) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, "the tarball is no gzip file"
	}
	archive := tar.NewReader(io.LimitReader(gz, maxUnpackedTarball))
	var data []byte
	for {
		h, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "the tarball is no tar archive in gzip, or it unpacks to more than " + fmt.Sprint(maxUnpackedTarball) + " bytes"
		}
		if h.Name != npm.PackageJSON || h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Size > npm.MaxPackageJSONBytes {
			return nil, fmt.Sprintf("package/package.json of the tarball is larger than %d bytes", npm.MaxPackageJSONBytes)
		}
		if data, err = io.ReadAll(archive); err != nil {
			return nil, "package/package.json of the tarball cannot be read"
		}
	}
	if data == nil {
		return nil, "the tarball holds no package/package.json"
	}
	value, err := pyjson.Decode(data)
	manifest, ok := value.(*pyjson.Object)
	if err != nil || !ok {
		return nil, "package/package.json of the tarball is no JSON object"
	}
	return manifest, ""
}
