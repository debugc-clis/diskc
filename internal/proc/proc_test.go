package proc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/debugc-clis/diskc/internal/disk"
)

func TestProcFixtureAttributesWritersAndDeletedFiles(t *testing.T) {
	root := t.TempDir()
	oldRoot := procRoot
	procRoot = filepath.Join(root, "proc")
	t.Cleanup(func() { procRoot = oldRoot })

	processRoot := filepath.Join(procRoot, "123")
	for _, directory := range []string{"fd", "fdinfo"} {
		if err := os.MkdirAll(filepath.Join(processRoot, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(root, "gone.log (deleted)")
	if err := os.WriteFile(target, make([]byte, 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, descriptor := range []string{"3", "4"} {
		if err := os.Symlink(target, filepath.Join(processRoot, "fd", descriptor)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(processRoot, "fdinfo", descriptor), []byte("flags:\t0100001\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFixtureFile(t, filepath.Join(processRoot, "comm"), "db-writer\n")
	writeFixtureFile(t, filepath.Join(processRoot, "status"), "Name:\tdb-writer\nUid:\t1000\t1000\t1000\t1000\n")
	writeFixtureFile(t, filepath.Join(processRoot, "cgroup"), "0::/system.slice/db.service/docker-0123456789abcdef.scope\n")
	if err := os.Symlink("/usr/bin/db-writer", filepath.Join(processRoot, "exe")); err != nil {
		t.Fatal(err)
	}

	files := []disk.File{{Path: target}}
	AttributeWriters(files)
	if len(files[0].Writers) != 1 || files[0].Writers[0].PID != 123 || files[0].Writers[0].Service != "db.service" {
		t.Fatalf("unexpected writer attribution: %+v", files[0].Writers)
	}

	deleted := DeletedFiles()
	if len(deleted) != 1 {
		t.Fatalf("expected one retained inode, got %+v", deleted)
	}
	if deleted[0].AllocatedBytes <= 0 || deleted[0].ApparentBytes != 8192 || len(deleted[0].Writers) != 1 {
		t.Fatalf("unexpected deleted file: %+v", deleted[0])
	}
}

func TestWritableDescriptorRejectsReadOnly(t *testing.T) {
	root := t.TempDir()
	oldRoot := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = oldRoot })
	path := filepath.Join(root, "7", "fdinfo", "1")
	writeFixtureFile(t, path, "flags:\t0100000\n")
	if writableDescriptor(7, "1") {
		t.Fatal("read-only descriptor must not be treated as a writer")
	}
}

func writeFixtureFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
