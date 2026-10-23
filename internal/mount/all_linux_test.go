//go:build linux

package mount

import (
	"strings"
	"testing"
)

func TestParseLinuxMounts(t *testing.T) {
	input := strings.NewReader("36 25 8:1 / / rw,relatime - ext4 /dev/sda1 rw\n" +
		"37 25 8:2 / /data\\040disk ro,nosuid - xfs /dev/sdb1 ro\n" +
		"39 25 0:42 / /backup rw - nfs4 server:/backup rw\n" +
		"38 25 0:5 / /proc rw,nosuid - proc proc rw\n")
	mounts, err := parseLinuxMounts(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 3 {
		t.Fatalf("unexpected mounts: %+v", mounts)
	}
	if mounts[0].Path != "/" || mounts[0].Source != "/dev/sda1" || mounts[0].Type != "ext4" || mounts[0].Kind != KindLocal || mounts[0].ReadOnly {
		t.Fatalf("unexpected root mount: %+v", mounts[0])
	}
	if mounts[1].Path != "/data disk" || mounts[1].Source != "/dev/sdb1" || mounts[1].Type != "xfs" || !mounts[1].ReadOnly {
		t.Fatalf("unexpected data mount: %+v", mounts[1])
	}
	if mounts[2].Path != "/backup" || mounts[2].Kind != KindRemote {
		t.Fatalf("unexpected remote mount: %+v", mounts[2])
	}
}
