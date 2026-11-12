// Package cli is the command line of pipesmith-registry, a port of cli.py of
// the Python server: the same commands, texts and exit codes, with the
// program name pipesmith-registry and the product name Pipesmith Registry.
//
// Every command reads the data directory from PIPESMITH_REGISTRY_DATA, else
// ARTIFACT_REPOSITORY_DATA, else <root>/data where <root>/bin holds the
// binary. Exit codes: 0 done, 1 refused or failed, 2 wrong usage.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/mirosso25/pipesmith-registry/internal/config"
	"github.com/mirosso25/pipesmith-registry/internal/docker"
	"github.com/mirosso25/pipesmith-registry/internal/index"
	"github.com/mirosso25/pipesmith-registry/internal/listeners"
	"github.com/mirosso25/pipesmith-registry/internal/notify"
	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
	"github.com/mirosso25/pipesmith-registry/internal/pystr"
	"github.com/mirosso25/pipesmith-registry/internal/repos"
	"github.com/mirosso25/pipesmith-registry/internal/scan"
	"github.com/mirosso25/pipesmith-registry/internal/server"
	"github.com/mirosso25/pipesmith-registry/internal/state"
	"github.com/mirosso25/pipesmith-registry/internal/stores"
	"github.com/mirosso25/pipesmith-registry/internal/version"
)

const program = "pipesmith-registry"

var (
	kinds      = []string{stores.KindBlobStore, stores.KindDirectory}
	storeName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	configKeys = []string{"data_dir", "bind", "port", "config_path", "index_path", "repositories_path"}
	partEnds   = []string{".tsv", ".tsv.gz", ".tsv.zst"}
)

const helpBody = `Serves file blob stores and directory trees read only under /repository/<name>/<path>.

usage: pipesmith-registry <command> [options]

Server
  serve [--port N] [--bind ADDR]       run the server in the foreground, with one more
                                       listener per Docker repository that has a port

Stores
  store add <name> --kind <kind> --root <path> [--repos a,b] [--manifest <path>]
                                       attach a store; kind is nexus-file or directory,
                                       --repos limits a directory store to these names,
                                       --manifest gives a directory store the blob manifest
                                       (a file or a directory of part-*.tsv[.gz|.zst])
                                       for SHA1, content type and creation time
  store list                           list the stores with their indexed files
  store remove <name>                  detach a store and drop its rows from the index
  scan [<name>]                        scan one store or all of them into the index

Repositories
  repos [--json]                       repositories with format, type, access, Docker port,
                                       files and size
  repos port <name> <port|none>        set or clear the Docker port of a repository in
                                       config.json; it wins over the port of the settings
                                       import and takes effect at the next start
  import-settings <file>               write repositories.json from a repository
                                       settings export; credentials are not taken over
  docker-check <name>                  read every stored manifest of a Docker repository and
                                       list the blobs and manifests they name that are not
                                       in the index; exit code 1 when one is missing

` + stateHelpBody + `Settings
  config [--get KEY | --check | --json | --docker-ports]
                                       show the configuration, one value, or its problems;
                                       --docker-ports prints "<port> <repository>" for each
                                       Docker listener of the next start
  version                              print the version
  help                                 print this help

The data directory is PIPESMITH_REGISTRY_DATA, else ARTIFACT_REPOSITORY_DATA, else <project root>/data.
PIPESMITH_REGISTRY_PORT, else ARTIFACT_REPOSITORY_PORT, overrides the port of the configuration.
`

func helpText() string {
	return "Pipesmith Registry " + version.Version + "\n" + helpBody
}

// stopSignals end `serve`.
var stopSignals = []os.Signal{syscall.SIGTERM, os.Interrupt}

// commandError is CommandError: a command refused or failed, exit code 1.
type commandError struct{ message string }

func (e *commandError) Error() string { return e.message }

func refuse(format string, args ...any) error {
	return &commandError{fmt.Sprintf(format, args...)}
}

// Main runs one command and returns its exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		io.WriteString(stdout, helpText())
		return 0
	}
	previous := slog.Default()
	slog.SetDefault(slog.New(newLogHandler(stderr, false)))
	defer slog.SetDefault(previous)
	ns, err := parseArgs(args)
	if err != nil {
		var help *helpExit
		if errors.As(err, &help) {
			io.WriteString(stdout, help.text)
			return 0
		}
		fmt.Fprintf(stderr, "error: %s\n", err)
		fmt.Fprintf(stderr, "see: %s help\n", program)
		return 2
	}
	c := &command{ns: ns, out: stdout, err: stderr}
	code, err := c.run()
	if err != nil {
		var usage *usageError
		if errors.As(err, &usage) {
			fmt.Fprintf(stderr, "error: %s\n", usage.message)
			return 2
		}
		fmt.Fprintf(stderr, "error: %s\n", errorText(err))
		return 1
	}
	return code
}

type namespace struct {
	run    string
	values map[string]any
}

func parseIntArg(text string) (any, error) {
	return pystr.ParseInt(text)
}

// portOrNone is _port_or_none: "none" in any case, or ASCII digits.
func portOrNone(value string) (any, error) {
	if strings.ToLower(value) == "none" && len(value) == 4 {
		return nil, nil
	}
	if !asciiDigits(value) {
		return nil, &typeError{pystr.Repr(value) + " is neither a port number nor none"}
	}
	n, _ := new(big.Int).SetString(value, 10)
	return n, nil
}

func asciiDigits(text string) bool {
	if text == "" {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return true
}

func buildParser() *parser {
	top := newParser(program, false, "")
	commands := map[string]*parser{}
	sub := func(parent map[string]*parser, key, name string) *parser {
		p := newParser(program+" "+key, true, commandHelp[key])
		parent[name] = p
		return p
	}
	serve := sub(commands, "serve", "serve")
	serve.add(&action{dest: "port", options: []string{"--port"}, convert: parseIntArg, typeName: "int"})
	serve.add(&action{dest: "bind", options: []string{"--bind"}})
	serve.add(&action{dest: "server_names", options: []string{"--server-names"}})
	serve.add(&action{dest: "ask_ldap_password", options: []string{"--ask-ldap-password"}, nargs: "0", flag: true})
	serve.defaults["run"] = "cmd_serve"

	store := sub(commands, "store", "store")
	storeCommands := map[string]*parser{}
	add := sub(storeCommands, "store add", "add")
	add.add(&action{dest: "name"})
	add.add(&action{dest: "kind", options: []string{"--kind"}, required: true, choices: kinds})
	add.add(&action{dest: "root", options: []string{"--root"}, required: true})
	add.add(&action{dest: "repos", options: []string{"--repos"}})
	add.add(&action{dest: "manifest", options: []string{"--manifest"}})
	add.defaults["run"] = "cmd_store_add"
	sub(storeCommands, "store list", "list").defaults["run"] = "cmd_store_list"
	remove := sub(storeCommands, "store remove", "remove")
	remove.add(&action{dest: "name"})
	remove.defaults["run"] = "cmd_store_remove"
	store.add(&action{dest: "store_command", nargs: "parser", required: true, choices: []string{"add", "list", "remove"},
		commands: storeCommands})

	scanCommand := sub(commands, "scan", "scan")
	scanCommand.add(&action{dest: "name", nargs: "?"})
	scanCommand.defaults["run"] = "cmd_scan"

	reposCommand := sub(commands, "repos", "repos")
	reposCommand.add(&action{dest: "json", options: []string{"--json"}, nargs: "0", flag: true})
	reposCommand.defaults["run"] = "cmd_repos"
	reposCommands := map[string]*parser{}
	port := sub(reposCommands, "repos port", "port")
	port.add(&action{dest: "name"})
	port.add(&action{dest: "port", metavar: "<port|none>", convert: portOrNone})
	port.defaults["run"] = "cmd_repos_port"
	reposCommand.add(&action{dest: "repos_command", nargs: "parser", choices: []string{"port"}, commands: reposCommands})

	check := sub(commands, "docker-check", "docker-check")
	check.add(&action{dest: "name"})
	check.defaults["run"] = "cmd_docker_check"

	settings := sub(commands, "import-settings", "import-settings")
	settings.add(&action{dest: "file"})
	settings.defaults["run"] = "cmd_import_settings"
	addStateCommands(commands, sub)

	configCommand := sub(commands, "config", "config")
	get := configCommand.add(&action{dest: "get", options: []string{"--get"}, metavar: "KEY"})
	checkFlag := configCommand.add(&action{dest: "check", options: []string{"--check"}, nargs: "0", flag: true})
	jsonFlag := configCommand.add(&action{dest: "json", options: []string{"--json"}, nargs: "0", flag: true})
	portsFlag := configCommand.add(&action{dest: "docker_ports", options: []string{"--docker-ports"}, nargs: "0", flag: true})
	configCommand.exclusive(get, checkFlag, jsonFlag, portsFlag)
	configCommand.defaults["run"] = "cmd_config"

	sub(commands, "version", "version").defaults["run"] = "cmd_version"

	top.add(&action{dest: "command", nargs: "parser", required: true,
		choices: slices.Concat([]string{"serve", "store", "scan", "repos", "docker-check", "import-settings"},
			stateCommandNames, []string{"config", "version"}),
		commands: commands})
	return top
}

func parseArgs(args []string) (*namespace, error) {
	values, err := buildParser().parseArgs(args)
	if err != nil {
		return nil, err
	}
	run, _ := values["run"].(string)
	delete(values, "run")
	return &namespace{run: run, values: values}, nil
}

type command struct {
	ns  *namespace
	out io.Writer
	err io.Writer
}

func (c *command) print(format string, args ...any) {
	fmt.Fprintf(c.out, format+"\n", args...)
}

func (c *command) text(key string) (string, bool) {
	value, ok := c.ns.values[key].(string)
	return value, ok
}

func (c *command) run() (int, error) {
	switch c.ns.run {
	case "cmd_serve":
		return c.serve()
	case "cmd_store_add":
		return c.storeAdd()
	case "cmd_store_list":
		return c.storeList()
	case "cmd_store_remove":
		return c.storeRemove()
	case "cmd_scan":
		return c.scan()
	case "cmd_repos":
		return c.repos()
	case "cmd_repos_port":
		return c.reposPort()
	case "cmd_docker_check":
		return c.dockerCheck()
	case "cmd_import_settings":
		return c.importSettings()
	case "cmd_import":
		return c.importExport()
	case "cmd_import_state":
		return c.importState()
	case "cmd_state_show":
		return c.stateShow()
	case "cmd_config":
		return c.config()
	case "cmd_version":
		c.print("%s %s", program, version.Version)
		return 0, nil
	}
	return 1, fmt.Errorf("no command %q", c.ns.run)
}

func load() (*config.Config, error) {
	return config.Load("", nil)
}

func openIndexReader(path string) (listeners.IndexReader, error) {
	return index.Open(path)
}

// Server

var dnsName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// parseHostNames reads --server-names: names and addresses separated by
// commas, in lower case, an IPv6 address in brackets.
func parseHostNames(text string) ([]string, error) {
	var names []string
	for _, part := range strings.Split(text, ",") {
		given := strings.TrimSpace(part)
		name := strings.ToLower(given)
		inner := strings.TrimSuffix(strings.TrimPrefix(name, "["), "]")
		switch ip := net.ParseIP(inner); {
		case ip != nil && strings.Contains(inner, ":"):
			names = append(names, "["+inner+"]")
		case ip != nil && inner == name:
			names = append(names, name)
		case dnsName.MatchString(name):
			names = append(names, name)
		default:
			return nil, refuse("host name %s is not a DNS name or an IP address", pystr.Repr(given))
		}
	}
	return names, nil
}

func (c *command) serve() (int, error) {
	cfg, err := load()
	if err != nil {
		return 1, err
	}
	port := big.NewInt(int64(cfg.Port))
	if value, ok := c.ns.values["port"].(*big.Int); ok {
		port = value
	}
	if bind, ok := c.text("bind"); ok {
		cfg.Bind = bind
	}
	var hostNames []string
	if text, ok := c.text("server_names"); ok {
		if hostNames, err = parseHostNames(text); err != nil {
			return 1, err
		}
	}
	if !slices.Contains(config.LocalBinds(), cfg.Bind) {
		// a network bind needs the users of state.sqlite and the names of this server
		if _, err := os.Stat(state.Path(cfg.DataDir)); err != nil {
			return 1, refuse("bind address %s is not local; only %s are allowed: no authentication in phase 1", pystr.Repr(cfg.Bind),
				strings.Join(config.LocalBinds(), ", "))
		}
		if len(hostNames) == 0 {
			return 1, refuse("bind address %s is not local; name this server with --server-names NAME[,NAME]", pystr.Repr(cfg.Bind))
		}
	}
	if port.Cmp(big.NewInt(1)) < 0 || port.Cmp(big.NewInt(65535)) > 0 {
		return 1, refuse("port %s is outside 1 to 65535", port)
	}
	cfg.Port = int(port.Int64())
	if listeners.IsReserved(cfg.Port) {
		return 1, refuse("port %d is reserved on this machine; use --port N", cfg.Port)
	}
	slog.SetDefault(slog.New(newLogHandler(c.err, true)))
	var ask askFunc
	if c.ns.values["ask_ldap_password"] == true {
		ask = askSecret(os.Stdin, c.err)
	}
	sec, err := buildSecurity(cfg.DataDir, ask, slog.New(fieldHandler{newLogHandler(c.err, true)}).With("logger", securityLogger))
	if err != nil {
		return 1, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), stopSignals...)
	defer stop()
	defer catchContinue()()
	opts := server.ServeOptions{AccessLog: c.err, Security: sec, HostNames: hostNames, Notify: notify.FromEnv(os.Getenv)}
	if err := server.Serve(ctx, cfg, opts); err != nil {
		return 1, err
	}
	return 0, nil
}

// Stores

func (c *command) storeAdd() (int, error) {
	cfg, err := load()
	if err != nil {
		return 1, err
	}
	name, _ := c.text("name")
	if !storeName.MatchString(name) {
		return 1, refuse("store name %s is not allowed; use letters, digits, '.', '_' and '-'", pystr.Repr(name))
	}
	for _, s := range cfg.Stores {
		if s.Name == name {
			return 1, refuse("a store named %s is attached already", name)
		}
	}
	rootArg, _ := c.text("root")
	root, err := abspath(osExpandUser(rootArg))
	if err != nil {
		return 1, err
	}
	if ok, err := isDir(root); err != nil || !ok {
		if err != nil {
			return 1, err
		}
		return 1, refuse("store root %s does not exist or is not a directory", root)
	}
	kind, _ := c.text("kind")
	if kind == stores.KindBlobStore {
		if ok, err := isDir(pystr.Join(root, stores.ContentDir)); err != nil || !ok {
			if err != nil {
				return 1, err
			}
			return 1, refuse("%s has no %s/ directory, so it is not a file blob store", root, stores.ContentDir)
		}
	}
	var storeRepos []string
	if value, ok := c.text("repos"); ok {
		if kind != stores.KindDirectory {
			return 1, refuse("only directory stores take --repos")
		}
		storeRepos = []string{}
		for _, part := range strings.Split(value, ",") {
			if part = pystr.Strip(part); part != "" {
				storeRepos = append(storeRepos, part)
			}
		}
	}
	var manifest *string
	if value, ok := c.text("manifest"); ok {
		if kind != stores.KindDirectory {
			return 1, refuse("only directory stores take --manifest; a file blob store holds its own metadata")
		}
		path, err := abspath(osExpandUser(value))
		if err != nil {
			return 1, err
		}
		dir, err := isDir(path)
		if err != nil {
			return 1, err
		}
		file, err := isFile(path)
		if err != nil {
			return 1, err
		}
		if !dir && !file {
			return 1, refuse("manifest %s does not exist or is neither a file nor a directory", path)
		}
		manifest = &path
	}
	cfg.Stores = append(cfg.Stores, config.StoreConfig{Name: name, Kind: kind, Root: root, Repos: storeRepos, Manifest: manifest})
	if err := config.Save(cfg); err != nil {
		return 1, err
	}
	c.print("added store %s (%s, %s)", name, kind, root)
	if manifest != nil {
		c.print("manifest: %s", *manifest)
		found, err := manifestParts(*manifest)
		if err != nil {
			return 1, err
		}
		if !found {
			c.print("note: the manifest holds no part-*.tsv, part-*.tsv.gz or part-*.tsv.zst files")
		}
		c.print("the first scan builds its lookup table below %s", cfg.ManifestsPath())
	}
	c.print("next: scan %s", name)
	return 0, nil
}

// manifestParts reports whether manifest.parts of the Python server finds a
// file: the manifest itself, or a part file of the directory.
func manifestParts(source string) (bool, error) {
	if ok, err := isDir(source); err != nil || ok {
		if err != nil {
			return false, err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return false, err
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), "part-") || !slices.ContainsFunc(partEnds, func(end string) bool {
				return strings.HasSuffix(e.Name(), end)
			}) {
				continue
			}
			if ok, err := isFile(pystr.Join(source, e.Name())); err != nil || ok {
				return ok, err
			}
		}
		return false, nil
	}
	if ok, err := isFile(source); err != nil || ok {
		return ok, err
	}
	return false, &stores.NotFoundError{Message: fmt.Sprintf("manifest %s does not exist", source)}
}

func storeStats(cfg *config.Config) (map[string]index.StoreStat, error) {
	stats := map[string]index.StoreStat{}
	if ok, err := exists(cfg.IndexPath()); err != nil || !ok {
		return stats, err
	}
	ix, err := index.Open(cfg.IndexPath())
	if err != nil {
		return nil, err
	}
	defer ix.Close()
	list, err := ix.StoreStats()
	if err != nil {
		return nil, err
	}
	for _, s := range list {
		stats[s.Store] = s
	}
	return stats, nil
}

func (c *command) storeList() (int, error) {
	cfg, err := load()
	if err != nil {
		return 1, err
	}
	if len(cfg.Stores) == 0 {
		c.print("no stores attached; add one with: store add <name> --kind <kind> --root <path>")
		return 0, nil
	}
	counts, err := storeStats(cfg)
	if err != nil {
		return 1, err
	}
	rows := [][]string{{"NAME", "KIND", "FILES", "SIZE", "ROOT"}}
	for _, s := range cfg.Stores {
		root := s.Root
		if s.Repos != nil {
			root += "  (repos: " + strings.Join(s.Repos, ", ") + ")"
		}
		if s.Manifest != nil {
			root += "  (manifest: " + *s.Manifest + ")"
		}
		if ok, err := isDir(s.Root); err != nil {
			return 1, err
		} else if !ok {
			root += "  (missing)"
		}
		files, bytes := "-", "-"
		if stat, ok := counts[s.Name]; ok {
			files, bytes = fmt.Sprint(stat.Files), sizeOf(stat.Bytes)
		}
		rows = append(rows, []string{s.Name, s.Kind, files, bytes, root})
	}
	if err := table(c.out, rows, 2, 3); err != nil {
		return 1, err
	}
	return 0, nil
}

func (c *command) storeRemove() (int, error) {
	cfg, err := load()
	if err != nil {
		return 1, err
	}
	name, _ := c.text("name")
	kept := slices.DeleteFunc(slices.Clone(cfg.Stores), func(s config.StoreConfig) bool { return s.Name == name })
	if len(kept) == len(cfg.Stores) {
		return 1, refuse("no store named %s is attached", name)
	}
	cfg.Stores = kept
	if err := config.Save(cfg); err != nil {
		return 1, err
	}
	if ok, err := exists(cfg.IndexPath()); err != nil {
		return 1, err
	} else if ok {
		ix, err := index.Open(cfg.IndexPath())
		if err != nil {
			return 1, err
		}
		err = ix.RemoveStore(name)
		if closeErr := ix.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return 1, err
		}
	}
	c.print("removed store %s from the configuration and the index", name)
	return 0, nil
}

func (c *command) progressPrinter(name string) func(int) {
	f, ok := c.err.(*os.File)
	if !ok || !isTerminal(f) {
		return nil
	}
	return func(live int) {
		fmt.Fprintf(c.err, "\r  scanning %s: %s blobs", name, thousands(live))
	}
}

func (c *command) endProgress(progress func(int)) {
	if progress != nil {
		fmt.Fprint(c.err, "\r"+strings.Repeat(" ", 60)+"\r")
	}
}

func (c *command) scan() (int, error) {
	cfg, err := load()
	if err != nil {
		return 1, err
	}
	list := cfg.Stores
	if name, ok := c.text("name"); ok {
		list = slices.DeleteFunc(slices.Clone(cfg.Stores), func(s config.StoreConfig) bool { return s.Name != name })
		if len(list) == 0 {
			return 1, refuse("no store named %s is attached", name)
		}
	}
	if len(list) == 0 {
		return 1, refuse("no stores attached; add one with: store add <name> --kind <kind> --root <path>")
	}
	ix, err := index.Open(cfg.IndexPath())
	if err != nil {
		return 1, err
	}
	defer ix.Close()
	failed := 0
	for _, sc := range list {
		progress := c.progressPrinter(sc.Name)
		s, err := stores.Make(sc, cfg.DataDir)
		var result scan.Result
		if err == nil {
			result, err = scan.Run(s, ix, progress)
		}
		c.endProgress(progress)
		if err != nil {
			if !server.Caught(err) {
				return 1, err
			}
			fmt.Fprintf(c.err, "error: scan %s failed, the index keeps its previous rows: %s\n", sc.Name, server.PyText(err))
			failed++
			continue
		}
		c.print("scan %s: live=%d deleted=%d copies=%d no_bytes=%d unparsable=%d seconds=%.2f", result.Store, result.Live,
			result.Deleted, result.Copies, result.NoBytes, result.Unparsable, result.Seconds)
	}
	if failed > 0 {
		return 1, nil
	}
	return 0, nil
}

// Repositories

func (c *command) repos() (int, error) {
	cfg, err := load()
	if err != nil {
		return 1, err
	}
	var stats []repos.Stat
	hints := map[string]string{}
	if ok, err := exists(cfg.IndexPath()); err != nil {
		return 1, err
	} else if ok {
		ix, err := index.Open(cfg.IndexPath())
		if err != nil {
			return 1, err
		}
		list, err := ix.RepoStats()
		if err == nil {
			hints, err = ix.Hints()
		}
		ix.Close()
		if err != nil {
			return 1, err
		}
		for _, s := range list {
			stats = append(stats, repos.Stat{Repo: s.Repo, Files: s.Files, Bytes: s.Bytes, NewestMS: s.NewestMS})
		}
	}
	definitions, err := repos.LoadDefinitions(cfg.RepositoriesPath())
	if err != nil {
		return 1, err
	}
	names := make([]string, len(stats))
	for i, s := range stats {
		names[i] = s.Repo
	}
	rows := repos.RepositoryRows(repos.New(definitions, names, hints), stats, cfg.DockerPorts)
	if c.ns.values["json"] == true {
		data, err := pyjson.DumpsIndent(repos.RowsDocument(rows), 2)
		if err != nil {
			return 1, err
		}
		c.print("%s", data)
		return 0, nil
	}
	if len(rows) == 0 {
		c.print("no repositories; scan a store or run import-settings")
		return 0, nil
	}
	lines := [][]string{{"NAME", "FORMAT", "TYPE", "ACCESS", "PORT", "FILES", "SIZE", "NEWEST", "NOTE"}}
	for _, row := range rows {
		var notes []string
		if !row.Defined {
			notes = append(notes, "no definition")
		}
		if len(row.Members) > 0 {
			notes = append(notes, "members: "+strings.Join(row.Members, ", "))
		}
		port := "-"
		if row.DockerPort != nil {
			port = fmt.Sprint(*row.DockerPort)
		}
		lines = append(lines, []string{row.Name, row.Format, row.Type, row.Access, port, fmt.Sprint(row.Files), sizeOf(row.Bytes),
			timeText(row.NewestMS), strings.Join(notes, "; ")})
	}
	if err := table(c.out, lines, 4, 5, 6); err != nil {
		return 1, err
	}
	return 0, nil
}

// reposPort sets or clears the Docker port of a repository in config.json.
// It refuses a port the next start would refuse.
func (c *command) reposPort() (int, error) {
	cfg, err := load()
	if err != nil {
		return 1, err
	}
	repositories, err := listeners.LoadRepositories(cfg, openIndexReader)
	if err != nil {
		return 1, err
	}
	name, _ := c.text("name")
	repo, ok := repositories.Get(name)
	if !ok {
		return 1, refuse("no repository named %s; see: repos", name)
	}
	if repos.AccessOf(repo) != "docker" {
		return 1, refuse("repository %s has format %s; only Docker repositories take a port", repo.Name, repo.Format)
	}
	stored, err := config.Load(cfg.DataDir, config.Env(nil)) // what config.json holds, without the port of the environment
	if err != nil {
		return 1, err
	}
	port, _ := c.ns.values["port"].(*big.Int)
	if port == nil {
		delete(stored.DockerPorts, repo.Name)
		delete(cfg.DockerPorts, repo.Name)
	} else {
		// A number too large for the map stands in as -1, which the port map
		// refuses at the same place; its message then names the number.
		value := -1
		if port.IsInt64() && port.Int64() <= 1<<31 {
			value = int(port.Int64())
		}
		stored.DockerPorts[repo.Name] = value
		cfg.DockerPorts[repo.Name] = value
		if _, err := listeners.DockerPortMap(cfg, repositories); err != nil {
			message := err.Error()
			if value == -1 {
				message = strings.Replace(message, "Docker port -1 of "+repo.Name+" ", "Docker port "+port.String()+" of "+repo.Name+" ", 1)
			}
			return 1, refuse("%s; nothing changed", message)
		}
	}
	if err := config.Save(stored); err != nil {
		return 1, err
	}
	effective, hasPort := listeners.DockerPortOf(cfg, repo)
	if port == nil {
		rest := "it has no Docker port now"
		if hasPort {
			rest = fmt.Sprintf("the port of the definition applies: %d", effective)
		}
		c.print("removed the Docker port of %s from %s; %s", repo.Name, cfg.ConfigPath(), rest)
	} else {
		c.print("Docker port of %s set to %s in %s", repo.Name, port, cfg.ConfigPath())
	}
	c.print("the change takes effect at the next start of the server")
	return 0, nil
}

type manifestRef struct{ name, reference string }

// storedManifests is (name, reference) of every file below
// v2/<name>/manifests/ of the members, each once, sorted.
func storedManifests(ix *index.Index, members []string) ([]manifestRef, error) {
	found := map[manifestRef]bool{}
	for _, member := range members {
		stack := []string{"v2"}
		for len(stack) > 0 {
			directory := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			var files []string
			for offset := 0; ; {
				page, err := ix.Children(member, directory, 10_000, offset)
				if err != nil {
					return nil, err
				}
				for _, child := range page {
					if child.IsDir {
						if directory+"/"+child.Name != "v2/-" {
							stack = append(stack, directory+"/"+child.Name)
						}
					} else {
						files = append(files, child.Name)
					}
				}
				if len(page) < 10_000 {
					break
				}
				offset += len(page)
			}
			if strings.HasSuffix(directory, "/manifests") && len(directory) > len("v2//manifests") {
				name := directory[len("v2/") : len(directory)-len("/manifests")]
				for _, reference := range docker.TagNames(files) {
					found[manifestRef{name, reference}] = true
				}
				for _, f := range files {
					if docker.IsDigest(f) {
						found[manifestRef{name, f}] = true
					}
				}
			}
		}
	}
	out := make([]manifestRef, 0, len(found))
	for ref := range found {
		out = append(out, ref)
	}
	slices.SortFunc(out, func(a, b manifestRef) int {
		if c := pystr.Compare(a.name, b.name); c != 0 {
			return c
		}
		return pystr.Compare(a.reference, b.reference)
	})
	return out, nil
}

func readManifest(ix *index.Index, attached map[string]stores.Store, members []string, path string) ([]byte, error) {
	for _, member := range members {
		asset, err := ix.Lookup(member, path)
		if err != nil {
			return nil, err
		}
		if asset == nil {
			continue
		}
		store := attached[asset.Store]
		if store == nil {
			return nil, &pyjson.Error{Class: "OSError", Message: fmt.Sprintf("store %s is not attached", asset.Store)}
		}
		f, err := store.Open(asset.Location)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(f, docker.MaxManifestBytes+1))
		f.Close()
		if err != nil {
			return nil, err
		}
		if len(data) > docker.MaxManifestBytes {
			return nil, &pyjson.Error{Class: "ValueError", Message: fmt.Sprintf("more than %d bytes", docker.MaxManifestBytes)}
		}
		return data, nil
	}
	return nil, &pyjson.Error{Class: "OSError", Message: path + " is not in the index"}
}

// dockerCheck lists what the stored manifests of a Docker repository name
// and the index lacks.
func (c *command) dockerCheck() (int, error) {
	cfg, err := load()
	if err != nil {
		return 1, err
	}
	if ok, err := exists(cfg.IndexPath()); err != nil || !ok {
		if err != nil {
			return 1, err
		}
		return 1, refuse("no index yet; attach a store and run: scan")
	}
	ix, err := index.Open(cfg.IndexPath())
	if err != nil {
		return 1, err
	}
	defer ix.Close()
	definitions, err := repos.LoadDefinitions(cfg.RepositoriesPath())
	if err != nil {
		return 1, err
	}
	names, err := ix.RepoNames()
	if err != nil {
		return 1, err
	}
	hints, err := ix.Hints()
	if err != nil {
		return 1, err
	}
	repositories := repos.New(definitions, names, hints)
	name, _ := c.text("name")
	repo, ok := repositories.Get(name)
	if !ok {
		return 1, refuse("no repository named %s; see: repos", name)
	}
	if repos.AccessOf(repo) != "docker" {
		return 1, refuse("repository %s has format %s; docker-check reads Docker repositories only", repo.Name, repo.Format)
	}
	var members []string
	for _, m := range repositories.Resolve(repo.Name) {
		members = append(members, m.Name)
	}
	attached := map[string]stores.Store{}
	for _, sc := range cfg.Stores {
		s, err := stores.Make(sc, cfg.DataDir)
		if err != nil {
			return 1, err
		}
		attached[sc.Name] = s
	}
	refs, err := storedManifests(ix, members)
	if err != nil {
		return 1, err
	}
	manifests, incomplete, unreadable := 0, 0, 0
	missing := map[string]bool{}
	for _, ref := range refs {
		manifests++
		label := ref.name + ":" + ref.reference
		if docker.IsDigest(ref.reference) {
			label = ref.name + "@" + ref.reference
		}
		data, err := readManifest(ix, attached, members, "v2/"+ref.name+"/manifests/"+ref.reference)
		var named []string
		if err == nil {
			named, err = docker.ReferencedDigests(data)
		}
		if err != nil {
			if !server.Caught(err) {
				return 1, err
			}
			unreadable++
			c.print("%s unreadable: %s", label, server.PyText(err))
			continue
		}
		var absent []string
		for _, digest := range named {
			present := false
			for _, member := range members {
				blob, err := ix.Lookup(member, "v2/-/blobs/"+digest)
				if err != nil {
					return 1, err
				}
				child, err := ix.Lookup(member, "v2/"+ref.name+"/manifests/"+digest)
				if err != nil {
					return 1, err
				}
				if blob != nil || child != nil {
					present = true
					break
				}
			}
			if !present {
				absent = append(absent, digest)
			}
		}
		if len(absent) > 0 {
			incomplete++
			for _, digest := range absent {
				missing[digest] = true
				c.print("%s missing %s", label, digest)
			}
		}
	}
	c.print("%s: %d manifests read, %d incomplete, %d missing, %d unreadable", repo.Name, manifests, incomplete, len(missing), unreadable)
	if len(missing) > 0 || unreadable > 0 {
		return 1, nil
	}
	return 0, nil
}

func (c *command) importSettings() (int, error) {
	cfg, err := load()
	if err != nil {
		return 1, err
	}
	file, _ := c.text("file")
	source, err := pathExpandUser(file)
	if err != nil {
		return 1, err
	}
	if ok, err := isFile(source); err != nil || !ok {
		if err != nil {
			return 1, err
		}
		return 1, refuse("settings file %s does not exist", source)
	}
	count, err := repos.ImportRepositorySettings(source, cfg.RepositoriesPath())
	if err != nil {
		var syntax *pyjson.SyntaxError
		if errors.As(err, &syntax) {
			return 1, refuse("%s is not valid JSON: %s", source, syntax)
		}
		return 1, err
	}
	c.print("imported %d repositories into %s", count, cfg.RepositoriesPath())
	return 0, nil
}

// Settings

func valueErrorClass(err error) bool {
	switch server.PyClass(err) {
	case "ValueError", "JSONDecodeError", "UnicodeDecodeError", "UnicodeEncodeError":
		return true
	}
	return false
}

func (c *command) config() (int, error) {
	if c.ns.values["check"] == true {
		problems, err := problems()
		if err != nil {
			return 1, err
		}
		for _, p := range problems {
			c.print("%s", p)
		}
		if len(problems) > 0 {
			return 1, nil
		}
		return 0, nil
	}
	cfg, err := load()
	if err != nil {
		return 1, err
	}
	if c.ns.values["docker_ports"] == true {
		repositories, err := listeners.LoadRepositories(cfg, openIndexReader)
		if err != nil {
			return 1, err
		}
		ports, err := listeners.DockerPortMap(cfg, repositories)
		if err != nil {
			return 1, err
		}
		for _, port := range listeners.SortedPorts(ports) {
			c.print("%d %s", port, ports[port])
		}
		return 0, nil
	}
	values := [][2]any{{"data_dir", cfg.DataDir}, {"bind", cfg.Bind}, {"port", cfg.Port}, {"config_path", cfg.ConfigPath()},
		{"index_path", cfg.IndexPath()}, {"repositories_path", cfg.RepositoriesPath()}}
	if key, ok := c.text("get"); ok {
		for _, kv := range values {
			if kv[0] == key {
				c.print("%v", kv[1])
				return 0, nil
			}
		}
		return 2, &usageError{fmt.Sprintf("unknown key %s; keys: %s", pystr.Repr(key), strings.Join(configKeys, ", "))}
	}
	if c.ns.values["json"] == true {
		doc := pyjson.NewObject()
		for _, kv := range values {
			doc.Set(kv[0].(string), kv[1])
		}
		list := make([]any, 0, len(cfg.Stores))
		for _, s := range cfg.Stores {
			var storeRepos, manifest any
			if s.Repos != nil {
				items := make([]any, len(s.Repos))
				for i, r := range s.Repos {
					items[i] = r
				}
				storeRepos = items
			}
			if s.Manifest != nil {
				manifest = *s.Manifest
			}
			list = append(list, pyjson.NewObject().Set("name", s.Name).Set("kind", s.Kind).Set("root", s.Root).
				Set("repos", storeRepos).Set("manifest", manifest))
		}
		doc.Set("stores", list)
		names := make([]string, 0, len(cfg.DockerPorts))
		for name := range cfg.DockerPorts {
			names = append(names, name)
		}
		slices.SortFunc(names, pystr.Compare)
		ports := pyjson.NewObject()
		for _, name := range names {
			ports.Set(name, cfg.DockerPorts[name])
		}
		doc.Set("docker_ports", ports)
		data, err := pyjson.DumpsIndent(doc, 2)
		if err != nil {
			return 1, err
		}
		c.print("%s", data)
		return 0, nil
	}
	rows := make([][]string, 0, len(values)+1)
	for _, kv := range values {
		rows = append(rows, []string{kv[0].(string), fmt.Sprint(kv[1])})
	}
	rows = append(rows, []string{"stores", fmt.Sprint(len(cfg.Stores))})
	if err := table(c.out, rows); err != nil {
		return 1, err
	}
	for _, s := range cfg.Stores {
		manifest := ""
		if s.Manifest != nil {
			manifest = "  manifest " + *s.Manifest
		}
		c.print("  %s  %s  %s%s", s.Name, s.Kind, s.Root, manifest)
	}
	repositories, err := listeners.LoadRepositories(cfg, openIndexReader)
	var ports map[int]string
	if err == nil {
		ports, err = listeners.DockerPortMap(cfg, repositories)
	}
	if err != nil {
		if !valueErrorClass(err) {
			return 1, err
		}
		c.print("docker ports  %s", server.PyText(err))
		return 0, nil
	}
	c.print("docker ports  %d", len(ports))
	for _, port := range listeners.SortedPorts(ports) {
		c.print("  %d  %s", port, ports[port])
	}
	return 0, nil
}

// problems is _problems: what keeps the configuration from working, one
// line each. An error Python does not catch there is returned.
func problems() ([]string, error) {
	cfg, err := load()
	if err != nil {
		if valueErrorClass(err) {
			return []string{server.PyText(err)}, nil
		}
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for _, s := range cfg.Stores {
		if seen[s.Name] {
			out = append(out, fmt.Sprintf("store %s is attached twice", s.Name))
		}
		seen[s.Name] = true
		if !slices.Contains(kinds, s.Kind) {
			out = append(out, fmt.Sprintf("store %s has the unknown kind %s", s.Name, pystr.Repr(s.Kind)))
			continue
		}
		root, err := pathExpandUser(s.Root)
		if err != nil {
			return nil, err
		}
		dir, err := isDir(root)
		if err != nil {
			return nil, err
		}
		if !dir {
			out = append(out, fmt.Sprintf("store %s: root %s is missing", s.Name, root))
		} else if s.Kind == stores.KindBlobStore {
			content, err := isDir(pystr.Join(root, stores.ContentDir))
			if err != nil {
				return nil, err
			}
			if !content {
				out = append(out, fmt.Sprintf("store %s: %s has no %s/ directory", s.Name, root, stores.ContentDir))
			}
		}
		if s.Manifest != nil {
			manifest, err := pathExpandUser(*s.Manifest)
			if err != nil {
				return nil, err
			}
			if s.Kind != stores.KindDirectory {
				out = append(out, fmt.Sprintf("store %s: only directory stores take a manifest", s.Name))
			} else {
				dir, err := isDir(manifest)
				if err != nil {
					return nil, err
				}
				file, err := isFile(manifest)
				if err != nil {
					return nil, err
				}
				if !dir && !file {
					out = append(out, fmt.Sprintf("store %s: manifest %s is missing", s.Name, manifest))
				}
			}
		}
	}
	if _, err := repos.LoadDefinitions(cfg.RepositoriesPath()); err != nil {
		if !server.Caught(err) {
			return nil, err
		}
		out = append(out, "repository definitions: "+server.PyText(err))
		return out, nil
	}
	repositories, err := listeners.LoadRepositories(cfg, openIndexReader)
	if err == nil {
		_, err = listeners.DockerPortMap(cfg, repositories)
	}
	if err != nil {
		if !server.Caught(err) {
			return nil, err
		}
		out = append(out, "docker ports: "+server.PyText(err))
	}
	return out, nil
}
