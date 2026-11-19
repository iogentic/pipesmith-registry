package cli

import (
	"log/slog"

	"github.com/iogentic/pipesmith-registry/internal/maven"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/server"
)

// The command maven rebuild-metadata (task 10 and W15 of the phase 4 plan).
// It writes through the write path of the server, so it takes the lock of
// the write store (W12) and refuses while a serve with writes on holds it. A
// serve without writes beside it attaches the write store the command makes
// and serves the files it writes (finding S4 of review 4).

const mavenHelpBody = `Maven
  maven rebuild-metadata <repository> [--group G] [--artifact A] [--version V]
                                       write maven-metadata.xml of a hosted maven2 repository
                                       and its checksum files again from the files it holds;
                                       needs state.sqlite and refuses beside a serve with
                                       writes on; a serve without writes serves what it writes

`

// rebuildUser and rebuildAddress are created-by and created-by-ip of the
// blobs the command writes: no request makes them, the machine does.
const (
	rebuildUser    = "system"
	rebuildAddress = "127.0.0.1"
)

var mavenCommandNames = []string{"maven"}

func addMavenCommands(commands map[string]*parser, sub func(map[string]*parser, string, string) *parser) {
	mavenCommand := sub(commands, "maven", "maven")
	mavenCommands := map[string]*parser{}
	rebuild := sub(mavenCommands, "maven rebuild-metadata", "rebuild-metadata")
	rebuild.add(&action{dest: "group", options: []string{"--group"}})
	rebuild.add(&action{dest: "artifact", options: []string{"--artifact"}})
	rebuild.add(&action{dest: "version", options: []string{"--version"}})
	rebuild.add(&action{dest: "repository"})
	rebuild.defaults["run"] = "cmd_maven_rebuild"
	mavenCommand.add(&action{dest: "maven_command", nargs: "parser", required: true, choices: []string{"rebuild-metadata"},
		commands: mavenCommands})
}

func (c *command) mavenRebuild() (int, error) {
	cfg, err := load()
	if err != nil {
		return 1, err
	}
	repository, _ := c.text("repository")
	scope := maven.Scope{}
	scope.GroupID, _ = c.text("group")
	scope.ArtifactID, _ = c.text("artifact")
	scope.BaseVersion, _ = c.text("version")
	if _, err := scope.Dir(); err != nil {
		return 1, refuse("%v", err)
	}
	sec, err := buildSecurity(cfg.DataDir, nil, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		return 1, err
	}
	defer closeSecurity(sec)
	if sec == nil {
		return 1, refuse("the rebuild writes blobs, which needs state.sqlite with the users of this server; import them first (import)")
	}
	srv, err := server.New(cfg, nil, server.Options{Security: sec, Writes: true})
	if err != nil {
		return 1, err
	}
	report, err := srv.RebuildMavenMetadata(repository, scope, &model.Subject{User: model.User{ID: rebuildUser}}, rebuildAddress)
	if closeErr := srv.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return 1, err
	}
	c.print("rebuilt the metadata of repository %s: %d written, %d unchanged, %d removed", repository, len(report.Written),
		len(report.Unchanged), len(report.Removed))
	for _, list := range []struct {
		word  string
		paths []string
	}{{"written", report.Written}, {"removed", report.Removed}, {"kept", report.Kept}} {
		for _, p := range list.paths {
			if list.word == "kept" {
				c.print("kept %s: stale, but a read only store holds it", p)
				continue
			}
			c.print("%s %s", list.word, p)
		}
	}
	return 0, nil
}
