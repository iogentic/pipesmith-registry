package datafs

import "testing"

func TestKindOfMagic(t *testing.T) {
	for magic, want := range map[int64]string{
		0x6a656a63: "fakeowner",
		0x65735546: "fuse",
		0x6969:     "nfs",
		0x517b:     "smb",
		0xfe534d42: "smb2",
		0xff534d42: "cifs",
		0xef53:     "", // ext2, ext3, ext4
		0x58465342: "", // xfs
		0x01021994: "", // tmpfs
		0x794c7630: "", // overlayfs
	} {
		if got := kindOf(magic); got != want {
			t.Errorf("kindOf(%#x) = %q, want %q", magic, got, want)
		}
	}
}

func TestALocalTemporaryDirectoryIsNotShared(t *testing.T) {
	if got := Shared(t.TempDir()); got != "" {
		t.Fatalf("Shared(TempDir) = %q", got)
	}
}

func TestAMissingDirectoryIsJudgedByItsParent(t *testing.T) {
	if got := Shared(t.TempDir() + "/not/yet/there"); got != "" {
		t.Fatalf("Shared = %q", got)
	}
}
