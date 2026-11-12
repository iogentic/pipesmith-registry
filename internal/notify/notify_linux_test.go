package notify

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"
)

// A NOTIFY_SOCKET that starts with @ names a socket of the abstract namespace.
func TestSendToAnAbstractSocket(t *testing.T) {
	name := fmt.Sprintf("@pipesmith-registry-notify-test-%d", os.Getpid())
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	m := &manager{conn: conn, path: name}
	if err := FromEnv(env{"NOTIFY_SOCKET": name}.get).Send(Ready); err != nil {
		t.Fatal(err)
	}
	if got := m.next(t, 2*time.Second); got != Ready {
		t.Fatalf("datagram %q", got)
	}
}
