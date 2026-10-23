package mount

import "testing"

func TestUnescape(t *testing.T) {
	got := unescape(`/srv/my\040disk\011data\134file`)
	if got != "/srv/my disk\tdata\\file" {
		t.Fatalf("unexpected mount path: %q", got)
	}
}

func TestHasOption(t *testing.T) {
	if !hasOption("rw,nosuid,nodev", "nodev") {
		t.Fatal("expected nodev option")
	}
	if hasOption("rw,nosuid,nodev", "ro") {
		t.Fatal("did not expect ro option")
	}
}

func TestClassify(t *testing.T) {
	tests := map[string]string{
		"ext4": KindLocal, "nfs4": KindRemote, "overlay": KindContainer,
		"bpf": KindPseudo, "fuse.portal": KindPseudo, "ramfs": KindMemory,
		"futurefs": KindUnknown,
	}
	for filesystemType, expected := range tests {
		if got := Classify(filesystemType); got != expected {
			t.Errorf("Classify(%q) = %q, want %q", filesystemType, got, expected)
		}
	}
}
