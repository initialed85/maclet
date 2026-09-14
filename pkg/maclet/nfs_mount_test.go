package maclet

import (
	"strings"
	"testing"
)

func TestNFSCommandArgsAllowlistedOptionsAndReadOnly(t *testing.T) {
	args, err := nfsMountCommandArgs(nfsMountSpec{Server: "192.168.1.25", Share: "/exports/share", MountOptions: []string{"vers=3", "tcp"}, ReadOnly: true}, "/tmp/maclet-nfs")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\x00")
	if joined != "mount_nfs\x00-o\x00vers=3,tcp,ro\x00192.168.1.25:/exports/share\x00/tmp/maclet-nfs" {
		t.Fatalf("mount args = %#v", args)
	}
}

func TestNFSCommandArgsRejectUnsafeInputs(t *testing.T) {
	for _, test := range []nfsMountSpec{
		{Server: "server/name", Share: "/exports/share"},
		{Server: "server", Share: "/exports/../secret"},
		{Server: "server", Share: "/exports/share", MountOptions: []string{"intr"}},
	} {
		if _, err := nfsMountCommandArgs(test, "/tmp/maclet-nfs"); err == nil {
			t.Fatalf("unsafe NFS spec unexpectedly succeeded: %#v", test)
		}
	}
	if _, err := nfsMountCommandArgs(nfsMountSpec{Server: "server", Share: "/exports/share"}, "relative"); err == nil {
		t.Fatal("relative mount target unexpectedly succeeded")
	}
}
