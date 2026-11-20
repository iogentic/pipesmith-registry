package release

import (
	"context"
	"fmt"
	"os"
	"testing"
)

// renewing writes a renewed certificate into the pinned file at the restart.
type renewing struct {
	*fakeService
	path string
	pem  []byte
}

func (r *renewing) Restart(ctx context.Context, unit string) error {
	if err := os.WriteFile(r.path, r.pem, 0o600); err != nil {
		return err
	}
	return r.fakeService.Restart(ctx, unit)
}

// A certificate renewed during the update: the wait reads the file again
// when the server presents another certificate, and a server that presents
// the new one is healthy (finding A-10).
func TestHealthTLSRenewal(t *testing.T) {
	r, certPEM := tlsRig(t, "0.1.0")
	_, oldPEM := testCert(t)
	path := writeFile(t, "server.pem", oldPEM)
	r.config(fmt.Sprintf(`, "health_tls_cert": %q`, path))
	r.u.Service = &renewing{fakeService: r.svc, path: path, pem: certPEM}
	if err := r.u.Update(r.release(versionScript("0.2.0")), false); err != nil {
		t.Fatalf("update: %v\n%s", err, r.out.String())
	}
}
