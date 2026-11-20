package cli

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logship"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The command log-target and the log targets of serve. Every secret below
// is invented for these tests.

const (
	hecCanary   = "demo-hec-token-canary-5"
	esCanary    = "demo-es-password-canary-6"
	badCanary   = "demo-bad-secret-canary-7"
	pipeCanary  = "demo-pipe-token-canary-8"
	esTestUser  = "demo-shipper"
	targetsWait = 15 * time.Second
)

// receiver is an HTTP stub of a log target: it keeps every request and
// answers with the body of the test.
type receiver struct {
	srv    *httptest.Server
	answer string
	mu     sync.Mutex
	got    []received
}

type received struct {
	path   string
	header http.Header
	body   string
}

func newReceiver(t *testing.T, answer string) *receiver {
	t.Helper()
	r := &receiver{answer: answer}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.got = append(r.got, received{path: req.URL.Path, header: req.Header.Clone(), body: string(body)})
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, r.answer)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) requests() []received {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.got)
}

// bodies is every body the receiver got, joined.
func (r *receiver) bodies() string {
	var b strings.Builder
	for _, got := range r.requests() {
		b.WriteString(got.body)
		b.WriteByte('\n')
	}
	return b.String()
}

// targetsOf reads the stored setting log_targets.
func targetsOf(t *testing.T, dir string) []map[string]any {
	t.Helper()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	value, found, err := st.Setting(logship.SettingKey)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		return nil
	}
	var rows []map[string]any
	if err := json.Unmarshal(value, &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func namesOfTargets(rows []map[string]any) []string {
	var names []string
	for _, r := range rows {
		names = append(names, fmt.Sprint(r["name"]))
	}
	return names
}

// noCanary fails when text holds one of the secrets of these tests, raw or
// in the forms of an Authorization header.
func noCanary(t *testing.T, where, text string) {
	t.Helper()
	basic := base64.StdEncoding.EncodeToString([]byte(esTestUser + ":" + esCanary))
	for _, canary := range []string{hecCanary, esCanary, badCanary, pipeCanary, basic} {
		if strings.Contains(text, canary) {
			t.Errorf("%s holds the secret %q", where, canary)
		}
	}
}

var logTargetHelp = map[string]string{
	"log-target": "usage: pipesmith-registry log-target [-h] {list,add,remove,test} ...\n\npositional arguments:\n" +
		"  {list,add,remove,test}\n\noptions:\n  -h, --help            show this help message and exit\n",
	"log-target list": "usage: pipesmith-registry log-target list [-h] [--json] [--data DATA]\n\noptions:\n" +
		"  -h, --help   show this help message and exit\n  --json\n  --data DATA\n",
	"log-target remove": "usage: pipesmith-registry log-target remove [-h] [--data DATA] name\n\npositional arguments:\n" +
		"  name\n\noptions:\n  -h, --help   show this help message and exit\n  --data DATA\n",
	"log-target test": "usage: pipesmith-registry log-target test [-h] [--data DATA] name\n\npositional arguments:\n" +
		"  name\n\noptions:\n  -h, --help   show this help message and exit\n  --data DATA\n",
}

func TestLogTargetHelp(t *testing.T) {
	dir := dataDir(t)
	for command, want := range logTargetHelp {
		code, out, errOut := run(t, append(strings.Fields(command), "--help")...)
		if code != 0 || out != want || errOut != "" {
			t.Errorf("%s --help: %d\n%s\nwant\n%s", command, code, out, want)
		}
	}
	code, out, _ := run(t, "log-target", "add", "--help")
	if code != 0 || !strings.HasPrefix(out, "usage: pipesmith-registry log-target add [-h] --kind KIND --address ADDRESS\n") ||
		!strings.Contains(out, "  --secret-stdin\n") {
		t.Errorf("log-target add --help: %d\n%s", code, out)
	}
	_, out, _ = run(t, "help")
	for _, line := range []string{"  log-target list [--json] [--data <dir>]", "  log-target remove <name> [--data <dir>]",
		"  log-target test <name> [--data <dir>]"} {
		if !strings.Contains(out, line) {
			t.Errorf("help lacks %q", line)
		}
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("help created the data directory")
	}
}

// Every sub command refuses without state.sqlite and does not create it:
// a new state.sqlite would switch security on.
func TestLogTargetNeedsState(t *testing.T) {
	dir := dataDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("error: no state.sqlite in %s; write one with import or import-state\n", dir)
	for _, args := range [][]string{{"list"}, {"list", "--json"}, {"add", "demo", "--kind", "http-json", "--address", "http://127.0.0.1:9/"},
		{"remove", "demo"}, {"test", "demo"}} {
		code, out, errOut := run(t, append([]string{"log-target"}, args...)...)
		if code != 1 || out != "" || errOut != want {
			t.Errorf("%v: %d %q %q", args, code, out, errOut)
		}
	}
	if _, err := os.Stat(state.Path(dir)); err == nil {
		t.Fatal("log-target created state.sqlite")
	}
	if names := namesIn(t, dir); len(names) != 0 {
		t.Fatalf("data directory holds %v", names)
	}
}

// add, list, test and remove against a data directory of the fixture
// import, with a stub receiver in the test.
func TestLogTargetCommands(t *testing.T) {
	dir := importedFixture(t)
	stub := newReceiver(t, "")
	code, out, errOut := withInput(t, pipeCanary+"\nsecond line is not read\n", "log-target", "add", "demo-json",
		"--kind", "http-json", "--address", stub.srv.URL+"/ingest", "--files", "server,audit", "--batch-lines", "50", "--secret-stdin")
	if code != 0 || errOut != "" {
		t.Fatalf("add: %d %q %q", code, out, errOut)
	}
	noCanary(t, "the output of add", out+errOut)
	path := filepath.Join(dir, "secrets", "log-target-demo-json")
	data, err := os.ReadFile(path)
	if err != nil || string(data) != pipeCanary {
		t.Fatalf("secret file %q %v", data, err)
	}
	for file, mode := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
		if info, err := os.Stat(file); err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s: %v %v, want mode %04o", file, info.Mode(), err, mode)
		}
	}
	rows := targetsOf(t, dir)
	if len(rows) != 1 || rows[0]["secret"] != "log-target-demo-json" || rows[0]["kind"] != "http-json" ||
		fmt.Sprint(rows[0]["files"]) != "[server audit]" || rows[0]["batch_lines"] != float64(50) {
		t.Fatalf("setting %v", rows)
	}

	if code, _, errOut := run(t, "log-target", "add", "demo-raw", "--kind", "http-json", "--address", stub.srv.URL); code != 0 {
		t.Fatalf("add without secret: %d %s", code, errOut)
	}
	code, out, errOut = run(t, "log-target", "add", "demo-raw", "--kind", "http-json", "--address", stub.srv.URL)
	if code != 1 || errOut != "error: a log target demo-raw exists; remove it first; nothing changed\n" {
		t.Fatalf("second add: %d %q %q", code, out, errOut)
	}

	code, out, errOut = run(t, "log-target", "list")
	wantList := fmt.Sprintf("demo-json http-json %s/ingest files=server,audit secret=yes\n"+
		"demo-raw http-json %s files=server,requests,audit,tasks secret=no\n", stub.srv.URL, stub.srv.URL)
	if code != 0 || out != wantList || errOut != "" {
		t.Fatalf("list: %d\n%s\nwant\n%s%s", code, out, wantList, errOut)
	}
	noCanary(t, "list", out)
	code, out, _ = run(t, "log-target", "list", "--json")
	var listed []map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &listed) != nil || len(listed) != 2 || listed[0]["secret"] != true ||
		listed[1]["secret"] != false || listed[0]["batch_lines"] != float64(50) {
		t.Fatalf("list --json: %d %s", code, out)
	}
	noCanary(t, "list --json", out)
	if strings.Contains(out, "log-target-demo-json") {
		t.Fatalf("list --json names the secret file: %s", out)
	}

	code, out, errOut = run(t, "log-target", "test", "demo-json")
	if code != 0 || out != "sent\n" || errOut != "" {
		t.Fatalf("test: %d %q %q", code, out, errOut)
	}
	got := stub.requests()
	if len(got) != 1 || got[0].path != "/ingest" || got[0].header.Get("Authorization") != "Bearer "+pipeCanary {
		t.Fatalf("stub got %+v", got)
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSuffix(got[0].body, "\n")), &line); err != nil || line["msg"] != "log-target test" {
		t.Fatalf("line %q %v", got[0].body, err)
	}

	code, out, errOut = run(t, "log-target", "remove", "demo-json")
	if code != 0 || out != "removed log target demo-json\n" || errOut != "" {
		t.Fatalf("remove: %d %q %q", code, out, errOut)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("the secret file is still there: %v", err)
	}
	if names := namesOfTargets(targetsOf(t, dir)); !slices.Equal(names, []string{"demo-raw"}) {
		t.Fatalf("targets %v", names)
	}
	if code, _, errOut := run(t, "log-target", "remove", "demo-raw"); code != 0 {
		t.Fatalf("remove without a secret file: %s", errOut)
	}
	for _, args := range [][]string{{"remove", "demo-raw"}, {"test", "demo-raw"}} {
		code, out, errOut = run(t, append([]string{"log-target"}, args...)...)
		if code != 1 || errOut != "error: no log target demo-raw\n" {
			t.Fatalf("%v: %d %q %q", args, code, out, errOut)
		}
	}
	code, out, _ = run(t, "log-target", "list")
	if code != 0 || out != "no log targets\n" {
		t.Fatalf("list of none: %d %q", code, out)
	}
}

// test prints the class of a failure and exits with 1.
func TestLogTargetTestNamesTheFailure(t *testing.T) {
	importedFixture(t)
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer refusing.Close()
	if code, _, errOut := run(t, "log-target", "add", "demo-down", "--kind", "http-json", "--address", refusing.URL); code != 0 {
		t.Fatal(errOut)
	}
	code, out, errOut := run(t, "log-target", "test", "demo-down")
	if code != 1 || out != "failed: status 503\n" || errOut != "" {
		t.Fatalf("test: %d %q %q", code, out, errOut)
	}
}

// Every rule of the validation refuses add, one case each, and leaves the
// setting and the secrets folder as they were. The rule "user is for http
// and https only" cannot be reached by add: no kind takes a secret over
// another scheme, and a user needs a secret.
func TestLogTargetAddRefusals(t *testing.T) {
	dir := importedFixture(t)
	web := []string{"--kind", "http-json", "--address", "http://127.0.0.1:9/"}
	cases := []struct {
		args  []string
		input string
		want  string
	}{
		{append([]string{"Demo"}, web...), "", `name "Demo" is not 1 to 32 of a-z, 0-9, _ and -, starting with a letter or a digit`},
		{[]string{"demo", "--kind", "demo-kind", "--address", "http://127.0.0.1:9/"}, "", `unknown kind "demo-kind"`},
		{[]string{"demo", "--kind", "loki", "--address", "no-url"}, "", "address has no scheme"},
		{[]string{"demo", "--kind", "loki", "--address", "http:///path"}, "", "address has no host"},
		{[]string{"demo", "--kind", "loki", "--address", "udp://127.0.0.1:9"}, "", `kind loki takes no scheme "udp"`},
		{[]string{"demo", "--kind", "loki", "--address", "http://demo:pw@127.0.0.1:9/"}, "",
			"address holds user info: give the user as user and the password as the secret"},
		{[]string{"demo", "--kind", "loki", "--address", "http://127.0.0.1:9/#part"}, "", "address holds a fragment"},
		{[]string{"demo", "--kind", "loki", "--address", "http://127.0.0.1:70000/"}, "", "port 70000 is outside 1 to 65535"},
		{[]string{"demo", "--kind", "syslog", "--address", "udp://192.0.2.1"}, "", "an address of scheme udp needs a port"},
		{[]string{"demo", "--kind", "syslog", "--address", "tcp://192.0.2.1:514/x"}, "", "an address of scheme tcp takes no path or query"},
		{append([]string{"demo", "--files", "server,cache"}, web...), "", `unknown file "cache"`},
		{append([]string{"demo", "--files", "server,server"}, web...), "", `files names the file "server" twice`},
		{append([]string{"demo", "--files", ","}, web...), "", "files is empty"},
		{append([]string{"demo", "--queue-lines", "0"}, web...), "", "queue_lines is 0, outside 1 to 1000000"},
		{append([]string{"demo", "--queue-bytes", "1024"}, web...), "", "queue_bytes is 1024, outside 65536 to 268435456"},
		{append([]string{"demo", "--batch-lines", "10001"}, web...), "", "batch_lines is 10001, outside 1 to 10000"},
		{append([]string{"demo", "--flush-interval-ms", "99"}, web...), "", "flush_interval_ms is 99, outside 100 to 60000"},
		{append([]string{"demo", "--timeout-ms", "60001"}, web...), "", "timeout_ms is 60001, outside 100 to 60000"},
		{append([]string{"demo", "--timeout-ms", "soon"}, web...), "", "--timeout-ms soon is not a whole number"},
		{append([]string{"demo", "--ca-file", "/srv/demo-ca.pem"}, web...), "", "ca_file needs https or tls"},
		{append([]string{"demo", "--user", "demo-user"}, web...), "", "user needs a secret, the password"},
		{[]string{"demo", "--kind", "http-json", "--address", "https://192.0.2.1/", "--user", "demo:user", "--secret-stdin"}, "x\n",
			"user holds a colon or a control character, or is longer than 256 bytes"},
		{append([]string{"demo", "--index", "demo-logs"}, web...), "", "index is for kind elasticsearch only"},
		{[]string{"demo", "--kind", "elasticsearch", "--address", "http://127.0.0.1:9/", "--index", "Demo"}, "",
			"index is not 1 to 255 of a-z, 0-9, ., _ and -, starting with a letter or a digit"},
		{[]string{"demo", "--kind", "syslog", "--address", "tcp://127.0.0.1:514", "--secret-stdin"}, "x\n", "kind syslog takes no secret over tcp"},
		{[]string{"demo", "--kind", "loki", "--address", "http://192.0.2.1:3100/", "--secret-stdin"}, "x\n",
			"a secret only over https or tls, or to a loopback address"},
		{append([]string{"demo", "--secret-stdin"}, web...), "", "no secret on standard input"},
		{append([]string{"demo", "--secret-stdin"}, web...), "\n", "no secret on standard input"},
		{append([]string{"demo", "--secret-stdin"}, web...), strings.Repeat("s", 4097) + "\n", "a secret has 1 to 4096 bytes"},
		{append([]string{"demo", "--secret-stdin"}, web...), strings.Repeat("s", 4097), "a secret has 1 to 4096 bytes"},
		{append([]string{"demo", "--secret-stdin"}, web...), "demo\x01secret\n", "the secret holds a control character"},
	}
	for _, c := range cases {
		code, out, errOut := withInput(t, c.input, append([]string{"log-target", "add"}, c.args...)...)
		if code != 1 || out != "" || errOut != "error: "+c.want+"; nothing changed\n" {
			t.Errorf("%v: %d %q %q, want %q", c.args, code, out, errOut, c.want)
		}
	}
	if rows := targetsOf(t, dir); len(rows) != 0 {
		t.Fatalf("setting %v", rows)
	}
	if names := namesIn(t, filepath.Join(dir, "secrets")); len(names) != 0 {
		t.Fatalf("secrets %v", names)
	}
	code, out, errOut := withInput(t, strings.Repeat("s", 4096)+"\n", append([]string{"log-target", "add", "demo", "--secret-stdin"}, web[:2]...)...)
	_ = out
	if code != 2 || !strings.Contains(errOut, "the following arguments are required: --address") {
		t.Fatalf("add without address: %d %q", code, errOut)
	}
	code, _, errOut = withInput(t, strings.Repeat("s", 4096)+"\n", append([]string{"log-target", "add", "demo", "--secret-stdin"}, web...)...)
	if code != 0 {
		t.Fatalf("a secret of 4096 bytes: %d %s", code, errOut)
	}
}

// servingTargets runs serve like servingWith and keeps standard output too.
func servingTargets(t *testing.T, args ...string) (done chan int, stderr, stdout func() string, port int) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outText := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(outR)
		outText <- string(data)
	}()
	port = freePort(t)
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done = make(chan int, 1)
	go func() {
		code := Main(append([]string{"serve", "--port", fmt.Sprint(port)}, args...), outW, errW)
		errW.Close()
		outW.Close()
		done <- code
	}()
	errText := make(chan string, 1)
	go func() {
		var b strings.Builder
		scanner := bufio.NewScanner(errR)
		for scanner.Scan() {
			b.WriteString(scanner.Text() + "\n")
		}
		errText <- b.String()
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/health", port))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no answer")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return done, func() string { return <-errText }, func() string { return <-outText }, port
}

// waitUntil polls cond until targetsWait passed.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(targetsWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %v for %s", targetsWait, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// serve ships the lines to a splunk-hec target with a token and an
// elasticsearch target with a user and a password: both secrets reach their
// receiver, and no log file, no standard stream, no output of the command
// and no answer of the server holds either. A target whose secret file
// cannot be read is left out with a warning that names the target and the
// file. The lines of the sender itself reach server.log and standard error,
// never a target.
func TestServeShipsToTargetsWithSecrets(t *testing.T) {
	dir := importedFixture(t)
	noLogDirVariable(t)
	hec := newReceiver(t, `{"text":"Success","code":0}`)
	es := newReceiver(t, `{"took":1,"errors":false,"items":[]}`)
	adds := [][]string{
		{hecCanary + "\n", "demo-hec", "--kind", "splunk-hec", "--address", hec.srv.URL, "--secret-stdin", "--flush-interval-ms", "100"},
		{esCanary + "\n", "demo-es", "--kind", "elasticsearch", "--address", es.srv.URL, "--user", esTestUser, "--secret-stdin",
			"--flush-interval-ms", "100"},
		{badCanary + "\n", "demo-bad", "--kind", "http-json", "--address", es.srv.URL + "/bad", "--secret-stdin"},
	}
	var outputs strings.Builder
	for _, a := range adds {
		code, out, errOut := withInput(t, a[0], append([]string{"log-target", "add"}, a[1:]...)...)
		if code != 0 {
			t.Fatalf("add %s: %d %s", a[1], code, errOut)
		}
		outputs.WriteString(out + errOut)
	}
	badPath := filepath.Join(dir, "secrets", "log-target-demo-bad")
	if err := os.Chmod(badPath, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"list"}, {"list", "--json"}, {"test", "demo-hec"}, {"test", "demo-es"}} {
		code, out, errOut := run(t, append([]string{"log-target"}, args...)...)
		if code != 0 {
			t.Fatalf("%v: %d %s %s", args, code, out, errOut)
		}
		outputs.WriteString(out + errOut)
	}
	noCanary(t, "the output of log-target", outputs.String())

	done, stderr, stdout, port := servingTargets(t)
	var answers strings.Builder
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	for _, path := range []string{"/api/health", "/repository/demo-none/a.txt", "/service/rest/v1/repositories"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Fprintf(&answers, "%d %v %s\n", resp.StatusCode, resp.Header, body)
	}
	waitUntil(t, "lines at both receivers", func() bool {
		return strings.Contains(hec.bodies(), "/repository/demo-none/a.txt") && strings.Contains(es.bodies(), "/repository/demo-none/a.txt")
	})
	stopServing(t, done)
	errText, outText := stderr(), stdout()

	for _, got := range hec.requests() {
		if got.path != "/services/collector/event" || got.header.Get("Authorization") != "Splunk "+hecCanary {
			t.Fatalf("splunk-hec got %s %q", got.path, got.header.Get("Authorization"))
		}
	}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(esTestUser+":"+esCanary))
	for _, got := range es.requests() {
		if got.path == "/bad" {
			t.Fatal("the target with an unreadable secret got a request")
		}
		if got.path != "/pipesmith-registry/_bulk" || got.header.Get("Authorization") != basic {
			t.Fatalf("elasticsearch got %s %q", got.path, got.header.Get("Authorization"))
		}
	}
	for name, r := range map[string]*receiver{"splunk-hec": hec, "elasticsearch": es} {
		if strings.Contains(r.bodies(), logship.Logger) {
			t.Errorf("%s got a line of %s", name, logship.Logger)
		}
	}

	logDir := filepath.Join(dir, "logs")
	var files strings.Builder
	for _, name := range namesIn(t, logDir) {
		data, err := os.ReadFile(filepath.Join(logDir, name))
		if err != nil {
			t.Fatal(err)
		}
		files.Write(data)
	}
	noCanary(t, "the log files", files.String())
	noCanary(t, "standard error", errText)
	noCanary(t, "standard output", outText)
	noCanary(t, "the answers of the server", answers.String())

	server := string(mustRead(t, filepath.Join(logDir, "server.log")))
	for _, text := range []string{server, errText} {
		if !strings.Contains(text, "log target left out") || !strings.Contains(text, "demo-bad") || !strings.Contains(text, badPath) {
			t.Errorf("no warning for demo-bad with its path in:\n%s", text)
		}
		for _, target := range []string{"demo-hec", "demo-es"} {
			if !strings.Contains(text, "log target stopped") || !strings.Contains(text, target) {
				t.Errorf("no stop line of %s in:\n%s", target, text)
			}
		}
	}
	var stops int
	for _, line := range jsonLines(t, filepath.Join(logDir, "server.log")) {
		if line["msg"] == "log target stopped" && line["logger"] == logship.Logger {
			stops++
		}
	}
	if stops != 2 {
		t.Errorf("%d stop lines in server.log, want 2: the manager closes before the files", stops)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// With the files off, a target added while serve runs receives lines within
// the reload time, and no log directory is made.
func TestServeTakesATargetAddedWhileRunningWithFilesOff(t *testing.T) {
	dir := importedFixture(t)
	noLogDirVariable(t)
	stub := newReceiver(t, "")
	done, stderr, _, port := servingTargets(t, "--log-dir", "off")
	code, _, errOut := run(t, "log-target", "add", "demo-late", "--kind", "http-json", "--address", stub.srv.URL,
		"--flush-interval-ms", "100", "--files", "requests")
	if code != 0 {
		t.Fatalf("add: %s", errOut)
	}
	added := time.Now()
	waitUntil(t, "a request line at the new target", func() bool {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/health?demo=late", port))
		if err == nil {
			resp.Body.Close()
		}
		return strings.Contains(stub.bodies(), "demo=late")
	})
	if took := time.Since(added); took > 5*time.Second+3*time.Second {
		t.Errorf("the first line came after %v", took)
	}
	stopServing(t, done)
	errText := stderr()
	if _, err := os.Stat(filepath.Join(dir, "logs")); !os.IsNotExist(err) {
		t.Fatalf("the files are off, yet the log directory exists: %v", err)
	}
	if strings.Contains(stub.bodies(), logship.Logger) {
		t.Fatal("the target got a line of the sender")
	}
	if !strings.Contains(errText, "log target stopped") {
		t.Fatalf("no stop line on standard error:\n%s", errText)
	}
}
