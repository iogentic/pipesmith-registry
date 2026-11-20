package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// servingCaptured runs serve with the defaults on a free port in this
// process until it answers /api/health; out returns standard output and
// standard error once done is read.
func servingCaptured(t *testing.T) (done chan int, out func() (stdout, stderr string), port int) {
	t.Helper()
	port = freePort(t)
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done = make(chan int, 1)
	go func() {
		code := Main([]string{"serve", "--port", fmt.Sprint(port)}, outW, errW)
		outW.Close()
		errW.Close()
		done <- code
	}()
	read := func(r *os.File) chan string {
		ch := make(chan string, 1)
		go func() {
			data, _ := io.ReadAll(r)
			ch <- string(data)
		}()
		return ch
	}
	stdout, stderr := read(outR), read(errR)
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
	return done, func() (string, string) { return <-stdout, <-stderr }, port
}

// rawGet sends a request line with the target exactly as given and returns
// the status and the body of the answer.
func rawGet(t *testing.T, port int, target string, header map[string]string) (int, []byte) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	request := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: 127.0.0.1:%d\r\n", target, port)
	for name, value := range header {
		request += name + ": " + value + "\r\n"
	}
	if _, err := io.WriteString(conn, request+"Connection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

// L9: a canary in each of token, password, secret, key and access_token (in
// lower case, in upper case and with a percent encoded name) and in each of
// the headers Authorization, Cookie and Set-Cookie appears in no log file, on
// neither standard stream and in no answer body. The lines of requests.log,
// requests-classic.log and standard error show the path and *** instead.
func TestServeMasksSecrets(t *testing.T) {
	dir := dataDir(t)
	noLogDirVariable(t)
	canary := func(n int) string { return fmt.Sprintf("zq%02dcanaryv", n) }
	var canaries []string
	for n := 1; n <= 20; n++ {
		canaries = append(canaries, canary(n))
	}
	header := map[string]string{"Authorization": "Bearer " + canary(18), "Cookie": "demo=" + canary(19), "Set-Cookie": "demo=" + canary(20)}
	type sent struct {
		path, query, masked string
		status              string // of the access line
	}
	queries := []sent{
		{"/api/health", fmt.Sprintf("token=%s&password=%s&secret=%s&key=%s&access_token=%s&demo=visible", canary(1), canary(2), canary(3), canary(4), canary(5)),
			"token=***&password=***&secret=***&key=***&access_token=***&demo=visible", "200 OK"},
		{"/api/health", fmt.Sprintf("TOKEN=%s;PASSWORD=%s;SECRET=%s;KEY=%s;ACCESS_TOKEN=%s;demo", canary(6), canary(7), canary(8), canary(9), canary(10)),
			"TOKEN=***;PASSWORD=***;SECRET=***;KEY=***;ACCESS_TOKEN=***;demo", "200 OK"},
		{"/api/health", fmt.Sprintf("%%74oken=%s&pass%%77ord=%s&%%53ECRET=%s&k%%65y=%s&access%%5Ftoken=%s&token", canary(11), canary(12), canary(13), canary(14), canary(15)),
			"%74oken=***&pass%77ord=***&%53ECRET=***&k%65y=***&access%5Ftoken=***&token", "200 OK"},
		{"/api/health/", "access_token=" + canary(17), "access_token=***", "307 Temporary Redirect"},
	}
	done, streams, port := servingCaptured(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var bodies [][]byte
	for _, q := range queries {
		req, err := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d%s?%s", port, q.path, q.query), nil)
		if err != nil {
			t.Fatal(err)
		}
		for name, value := range header {
			req.Header.Set(name, value)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.HasPrefix(q.status, fmt.Sprint(resp.StatusCode)+" ") {
			t.Fatalf("%s?%s: %d, want %s", q.path, q.query, resp.StatusCode, q.status)
		}
		bodies = append(bodies, body)
	}
	// the invalid request: a byte outside ASCII fails the parser check
	status, body := rawGet(t, port, "/api/health\xc3\xa4?token="+canary(16), header)
	if status != 400 || string(body) != "Invalid HTTP request received." {
		t.Fatalf("invalid request: %d %q", status, body)
	}
	bodies = append(bodies, body)
	stopServing(t, done)
	stdout, stderr := streams()

	logDir := filepath.Join(dir, "logs")
	names := namesIn(t, logDir)
	if !slices.Equal(names, logFileNames) {
		t.Fatalf("log directory %v, want %v", names, logFileNames)
	}
	searched := map[string]string{"standard output": stdout, "standard error": stderr}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(logDir, name))
		if err != nil {
			t.Fatal(err)
		}
		searched[name] = string(data)
	}
	for i, b := range bodies {
		searched[fmt.Sprintf("answer body %d", i+1)] = string(b)
	}
	for place, text := range searched {
		for _, c := range canaries {
			if strings.Contains(text, c) {
				t.Errorf("%s holds the canary %s", place, c)
			}
		}
	}

	var requestLines []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(searched["requests.log"], "\n"), "\n") {
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("requests.log: %v: %q", err, line)
		}
		requestLines = append(requestLines, v)
	}
	invalid := sent{"/api/health\u00e4", "token=" + canary(16), "token=***", ""}
	for _, q := range append(queries, invalid) {
		found := slices.ContainsFunc(requestLines, func(v map[string]any) bool { return v["path"] == q.path && v["query"] == q.masked })
		if !found {
			t.Errorf("requests.log has no line with path %q and query %q", q.path, q.masked)
		}
	}
	for _, q := range queries {
		code, _, _ := strings.Cut(q.status, " ")
		if line := fmt.Sprintf(`"GET %s?%s HTTP/1.1" %s `, q.path, q.masked, code); !strings.Contains(searched["requests-classic.log"], line) {
			t.Errorf("requests-classic.log has no line with %s", line)
		}
		if access := fmt.Sprintf(`"GET %s?%s HTTP/1.1" %s`+"\n", q.path, q.masked, q.status); !strings.Contains(stderr, access) {
			t.Errorf("standard error has no access line with %s", access)
		}
	}
	if line := `"GET /api/health\xc3\xa4?token=*** HTTP/1.1" 400 `; !strings.Contains(searched["requests-classic.log"], line) {
		t.Errorf("requests-classic.log has no line with %s", line)
	}
	if !strings.Contains(stderr, "WARNING:  Invalid HTTP request received.\n") {
		t.Errorf("standard error has no warning of the invalid request:\n%s", stderr)
	}
}
