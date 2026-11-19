package blobprops

import "testing"

// The uploader and its address come from @BlobStore.created-by and
// @BlobStore.created-by-ip. They are nil when the key is absent, empty or
// not a valid text, and they never change the rest of the parse: the
// reference does not read these keys, so no blob becomes unparsable by them.
func TestUploaderAndAddress(t *testing.T) {
	const base = "@Bucket.repo-name=demo-releases\n@BlobStore.blob-name=/docs/a.txt\nsize=1\ncreationTime=1700000000000\n" +
		"sha1=86f7e437faa5a7fce15d1ddcb9eaeaea377667b8\n"
	plain, err := Parse([]byte(base))
	if err != nil || plain == nil {
		t.Fatalf("Parse of the base: %v %v", plain, err)
	}
	for _, c := range []struct {
		name, extra       string
		uploader, address *string
	}{
		{"both", "@BlobStore.created-by=demo-user\n@BlobStore.created-by-ip=192.0.2.10\n", text("demo-user"), text("192.0.2.10")},
		{"escaped", "@BlobStore.created-by=demo\\u00e9user\n", text("demoéuser"), nil},
		{"absent", "", nil, nil},
		{"empty", "@BlobStore.created-by=\n@BlobStore.created-by-ip=\n", nil, nil},
		{"lone surrogate", "@BlobStore.created-by=demo\\uD800\n@BlobStore.created-by-ip=192.0.2.10\n", nil, text("192.0.2.10")},
		{"Latin-1 byte", "@BlobStore.created-by=d\xe9mo\n", text("démo"), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := Parse([]byte(base + c.extra))
			if err != nil || got == nil {
				t.Fatalf("Parse: %v %v", got, err)
			}
			if show(got.CreatedBy) != show(c.uploader) || show(got.CreatedByIP) != show(c.address) {
				t.Fatalf("uploader %s, address %s; want %s, %s", show(got.CreatedBy), show(got.CreatedByIP), show(c.uploader),
					show(c.address))
			}
			rest := *got
			rest.CreatedBy, rest.CreatedByIP = nil, nil
			if show(rest.SHA1) != show(plain.SHA1) || rest.Repo != plain.Repo || rest.Path != plain.Path ||
				rest.Size != plain.Size || rest.CreatedMS != plain.CreatedMS || rest.ContentType != plain.ContentType ||
				rest.Deleted != plain.Deleted || rest.IsCopy != plain.IsCopy || rest.Overflow != plain.Overflow {
				t.Fatalf("the keys changed the parse: %+v, want %+v", rest, *plain)
			}
		})
	}
}

func text(s string) *string { return &s }
