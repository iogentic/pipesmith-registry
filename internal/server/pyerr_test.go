package server

import (
	"fmt"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/blobprops"
)

func TestPyClassOfBlobpropsValueError(t *testing.T) {
	for _, err := range []error{
		&blobprops.ValueError{Message: "Exceeds the limit (4300 digits) for integer string conversion"},
		fmt.Errorf("scan: %w", &blobprops.ValueError{Message: "x"}),
	} {
		if PyClass(err) != "ValueError" || !Caught(err) {
			t.Errorf("%v: class %q, caught %v", err, PyClass(err), Caught(err))
		}
	}
	if Caught(fmt.Errorf("plain")) || PyClass(fmt.Errorf("plain")) != "Exception" {
		t.Error("a plain error is caught")
	}
}
