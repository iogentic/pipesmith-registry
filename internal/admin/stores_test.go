package admin

import (
	"fmt"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

func TestStoreVectors(t *testing.T) {
	var file struct {
		Family string `json:"family"`
		Note   string `json:"note"`
		Cases  []struct {
			Row       string           `json:"row"`
			Op        string           `json:"op"` // create, save
			Kind      string           `json:"kind"`
			Stored    *model.BlobStore `json:"stored"`
			Path      *string          `json:"path"` // null: the body sends no path
			SoftQuota *model.SoftQuota `json:"softQuota"`
			Refusals  *[]wantRefusal   `json:"refusals"`
		} `json:"cases"`
	}
	readJSON(t, "stores", &file)
	counts := tally{}
	for i, c := range file.Cases {
		storePath := ""
		if c.Path != nil {
			storePath = *c.Path
		}
		label := fmt.Sprintf("case %d (%s %s %s path %q sent %v %+v)", i, c.Row, c.Op, c.Kind, storePath, c.Path != nil, c.SoftQuota)
		var err error
		switch {
		case c.Op == "create":
			err = CheckStoreCreate(c.Kind, storePath, c.SoftQuota)
		case c.Op == "save" && c.Stored != nil:
			err = CheckStoreSave(*c.Stored, c.Path, c.SoftQuota)
		default:
			t.Fatalf("%s: unknown op, or a save without a stored store", label)
		}
		checkRefusals(t, label, err, c.Refusals)
		if c.Refusals != nil {
			counts.add(c.Row, len(*c.Refusals) > 0)
		}
	}
	counts.check(t, "stores", "R10")
}

// The kind check alone answers 501 with the area blob-stores.
func TestStoreKind(t *testing.T) {
	status, body := Answer(CheckStoreKind("azure"))
	if area, _ := body.(AreaMessage); status != 501 || area.Area != "blob-stores" || area.Message == "" {
		t.Errorf("%d %+v", status, body)
	}
	for _, kind := range []string{"file", "File", "FILE"} {
		if err := CheckStoreKind(kind); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
	}
}
