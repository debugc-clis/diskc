//go:build linux

package proc

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// Use real proc fd magic links for the current process, behind an isolated
// process-directory fixture so unrelated processes cannot affect assertions.
func TestDeletedFilesRealLinuxDescriptors(t *testing.T) {
	for _, kind := range []string{"linked-suffix", "remaining-hardlink", "unlinked", "unlinked-literal-suffix", "unlinked-directory"} {
		t.Run(kind, func(t *testing.T) {
			root := fixtureProcRoot(t)
			processRoot := filepath.Join(procRoot, strconv.Itoa(os.Getpid()))
			if err := os.MkdirAll(processRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/proc/self/fd", filepath.Join(processRoot, "fd")); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "candidate")
			if kind == "linked-suffix" || kind == "unlinked-literal-suffix" {
				path += " (deleted)"
			}
			if kind == "unlinked-directory" {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				writeFixtureFile(t, path, "retained regular-file contents")
			}
			file, err := os.Open(path) // A read-only holder still retains blocks.
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { file.Close() })
			if kind == "remaining-hardlink" {
				if err := os.Link(path, filepath.Join(root, "alias")); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "linked-suffix" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			info, err := file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			stat := info.Sys().(*syscall.Stat_t)
			deleted, warnings := DeletedFiles()
			if len(warnings) != 0 {
				t.Fatalf("unexpected current-process inspection warnings: %v", warnings)
			}
			wantRetained := kind == "unlinked" || kind == "unlinked-literal-suffix"
			if !wantRetained {
				if len(deleted) != 0 {
					t.Fatalf("%s produced false retained-space evidence: %+v", kind, deleted)
				}
				return
			}
			if len(deleted) != 1 {
				t.Fatalf("expected one real unlinked file, got %+v", deleted)
			}
			got := deleted[0]
			if got.Path != path || got.Device != uint64(stat.Dev) || got.ApparentBytes != info.Size() || got.AllocatedBytes != stat.Blocks*512 || len(got.Writers) != 1 || got.Writers[0].PID != os.Getpid() {
				t.Fatalf("retained evidence differs from open inode: %+v stat=%+v", got, stat)
			}
		})
	}
}
