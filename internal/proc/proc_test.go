package proc

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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

	files := scanFixtureFile(t, target)
	AttributeWriters(files)
	if len(files[0].Writers) != 1 || files[0].Writers[0].PID != 123 || files[0].Writers[0].Service != "db.service" {
		t.Fatalf("unexpected writer attribution: %+v", files[0].Writers)
	}

	deleted, warnings := DeletedFiles()
	if len(warnings) != 0 {
		t.Fatalf("unexpected visibility warnings: %v", warnings)
	}
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
	writable, err := writableDescriptor(7, "1")
	if err != nil {
		t.Fatal(err)
	}
	if writable {
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

// The fake fd links point to retained hard links/renamed files so os.Stat
// observes the inode an open Linux descriptor would retain after rotation.
func TestAttributeWritersUsesObservedIdentityAfterRotation(t *testing.T) {
	for _, sampled := range []bool{false, true} {
		for _, replacement := range []string{"regular", "symlink", "missing"} {
			t.Run(strconv.FormatBool(sampled)+"/"+replacement, func(t *testing.T) {
				root := fixtureProcRoot(t)
				path := filepath.Join(root, "app.log")
				writeFixtureFile(t, path, "old log")
				files := scanFixtureFile(t, path)
				if sampled {
					if err := disk.MeasureGrowth(files, time.Millisecond); err != nil {
						t.Fatal(err)
					}
					if files[0].GrowthStatus != "measured" {
						t.Fatalf("sample not measured: %+v", files[0])
					}
				}
				retained := path + ".1"
				if err := os.Rename(path, retained); err != nil {
					t.Fatal(err)
				}
				fixtureDescriptor(t, 123, "3", retained, "0100001")
				switch replacement {
				case "regular":
					writeFixtureFile(t, path, "new log")
					fixtureDescriptor(t, 456, "3", path, "0100001")
				case "symlink":
					other := filepath.Join(root, "other.log")
					writeFixtureFile(t, other, "different log")
					if err := os.Symlink(other, path); err != nil {
						t.Fatal(err)
					}
					fixtureDescriptor(t, 456, "3", other, "0100001")
				}
				AttributeWriters(files)
				assertWriterPIDs(t, files[0], 123)
			})
		}
	}
}

func TestAttributeWritersUpdatesAllRowsAndClearsStaleResults(t *testing.T) {
	root := fixtureProcRoot(t)
	path := filepath.Join(root, "app.log")
	writeFixtureFile(t, path, "log")
	alias := path + ".hardlink"
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	files := scanFixtureFile(t, path)
	files = append(files, files[0], scanFixtureFile(t, alias)[0])
	for _, pid := range []int{123, 456} {
		fixtureDescriptor(t, pid, "3", path, "0100001")
		fixtureDescriptor(t, pid, "4", alias, "0100002")
	}
	fixtureDescriptor(t, 789, "3", path, "0100000")
	for attempt := 0; attempt < 2; attempt++ {
		AttributeWriters(files)
		for _, file := range files {
			assertWriterPIDs(t, file, 123, 456)
		}
	}
	if err := os.RemoveAll(filepath.Join(procRoot, "123")); err != nil {
		t.Fatal(err)
	}
	AttributeWriters(files)
	for _, file := range files {
		assertWriterPIDs(t, file, 456)
	}
	if err := os.RemoveAll(procRoot); err != nil {
		t.Fatal(err)
	}
	AttributeWriters(files)
	for _, file := range files {
		assertWriterPIDs(t, file)
	}
}

func TestAttributeWritersDoesNotGuessUnknownIdentity(t *testing.T) {
	root := fixtureProcRoot(t)
	path := filepath.Join(root, "app.log")
	writeFixtureFile(t, path, "log")
	fixtureDescriptor(t, 123, "3", path, "0100001")
	files := []disk.File{{Path: path, Writers: []disk.Writer{{PID: 999}}}}
	AttributeWriters(files)
	assertWriterPIDs(t, files[0])
}

func TestAttributeWritersUsesRefreshedSampleIdentity(t *testing.T) {
	root := fixtureProcRoot(t)
	path := filepath.Join(root, "app.log")
	writeFixtureFile(t, path, "old")
	files := scanFixtureFile(t, path)
	retained := path + ".1"
	if err := os.Rename(path, retained); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, path, "replacement")
	if err := disk.MeasureGrowth(files, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if files[0].GrowthStatus != "replaced" {
		t.Fatalf("replacement not identified: %+v", files[0])
	}
	fixtureDescriptor(t, 123, "3", retained, "0100001")
	fixtureDescriptor(t, 456, "3", path, "0100001")
	AttributeWriters(files)
	assertWriterPIDs(t, files[0], 456)
}

func fixtureProcRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	oldRoot := procRoot
	procRoot = filepath.Join(root, "proc")
	t.Cleanup(func() { procRoot = oldRoot })
	return root
}

func scanFixtureFile(t *testing.T, path string) []disk.File {
	t.Helper()
	usage, err := disk.FilesystemUsage(path)
	if err != nil {
		t.Fatal(err)
	}
	files, _, _, err := disk.Scan(path, usage.Device, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected one scanned file, got %+v", files)
	}
	return files
}

func fixtureDescriptor(t *testing.T, pid int, fd, target, flags string) {
	t.Helper()
	processRoot := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(filepath.Join(processRoot, "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(processRoot, "fd", fd)); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(processRoot, "fdinfo", fd), "flags:\t"+flags+"\n")
	writeFixtureFile(t, filepath.Join(processRoot, "comm"), "writer-"+strconv.Itoa(pid)+"\n")
}

func assertWriterPIDs(t *testing.T, file disk.File, want ...int) {
	t.Helper()
	if len(file.Writers) != len(want) {
		t.Fatalf("writers=%+v; want PIDs %v", file.Writers, want)
	}
	for index, pid := range want {
		if file.Writers[index].PID != pid {
			t.Fatalf("writers=%+v; want PIDs %v", file.Writers, want)
		}
	}
}

func TestUnavailableProcReturnsVisibilityWarnings(t *testing.T) {
	for _, kind := range []string{"missing", "not-directory"} {
		t.Run(kind, func(t *testing.T) {
			fixtureProcRoot(t)
			if kind == "not-directory" {
				writeFixtureFile(t, procRoot, "not proc")
			}
			files := []disk.File{{Writers: []disk.Writer{{PID: 999}}}}
			warnings := AttributeWriters(files)
			assertWriterPIDs(t, files[0])
			assertVisibilityWarning(t, warnings, "writer attribution unavailable", "cannot read /proc", "does not establish", "affected service user")
			deleted, warnings := DeletedFiles()
			if len(deleted) != 0 {
				t.Fatalf("unexpected deleted files: %+v", deleted)
			}
			assertVisibilityWarning(t, warnings, "deleted-open inspection unavailable", "cannot read /proc", "not exhaustive", "affected service user")
		})
	}
}

func TestPartialProcInspectionKeepsVisibleResultsAndAggregatesFailures(t *testing.T) {
	root := fixtureProcRoot(t)
	path := filepath.Join(root, "retained.log (deleted)")
	writeFixtureFile(t, path, "retained data")
	files := scanFixtureFile(t, path)
	fixtureDescriptor(t, 123, "3", path, "0100001")
	// Deterministic ENOTDIR exercises inaccessible fd directories even when
	// tests run as root, where chmod-based permission fixtures are ineffective.
	for _, pid := range []string{"456", "789"} {
		writeFixtureFile(t, filepath.Join(procRoot, pid, "fd"), "not a directory")
	}
	warnings := AttributeWriters(files)
	assertWriterPIDs(t, files[0], 123)
	assertVisibilityWarning(t, warnings, "writer attribution partial", "2 process fd directories", "0 fdinfo records", "affected service user")
	deleted, warnings := DeletedFiles()
	if len(deleted) != 1 || len(deleted[0].Writers) != 1 || deleted[0].Writers[0].PID != 123 {
		t.Fatalf("partial inspection lost visible retained file: %+v", deleted)
	}
	assertVisibilityWarning(t, warnings, "deleted-open inspection partial", "2 process fd directories", "not exhaustive")
}

func TestUnknownDescriptorFlagsAreNotReadOnly(t *testing.T) {
	for _, kind := range []string{"invalid", "missing-flags", "empty", "read-error"} {
		t.Run(kind, func(t *testing.T) {
			root := fixtureProcRoot(t)
			path := filepath.Join(root, "app.log")
			writeFixtureFile(t, path, "log")
			files := scanFixtureFile(t, path)
			fixtureDescriptor(t, 123, "3", path, "0100001")
			fixtureDescriptor(t, 456, "3", path, "0100001")
			infoPath := filepath.Join(procRoot, "456", "fdinfo", "3")
			switch kind {
			case "invalid":
				writeFixtureFile(t, infoPath, "flags:\tinvalid\n")
			case "missing-flags":
				writeFixtureFile(t, infoPath, "pos:\t0\n")
			case "empty":
				writeFixtureFile(t, infoPath, "")
			case "read-error":
				if err := os.Remove(infoPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(infoPath, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			warnings := AttributeWriters(files)
			assertWriterPIDs(t, files[0], 123)
			assertVisibilityWarning(t, warnings, "writer attribution partial", "1 fdinfo records")
		})
	}
}

func TestDescriptorInspectionFailuresAreVisible(t *testing.T) {
	root := fixtureProcRoot(t)
	path := filepath.Join(root, "app.log")
	writeFixtureFile(t, path, "log")
	files := scanFixtureFile(t, path)
	fixtureDescriptor(t, 123, "3", path, "0100001")
	// A symlink loop produces a non-ENOENT stat failure.
	loop := filepath.Join(root, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	fixtureDescriptor(t, 456, "3", loop, "0100001")
	warnings := AttributeWriters(files)
	assertWriterPIDs(t, files[0], 123)
	assertVisibilityWarning(t, warnings, "writer attribution partial", "1 descriptors")
	// A non-symlink fd entry produces a non-ENOENT readlink failure.
	writeFixtureFile(t, filepath.Join(procRoot, "789", "fd", "3"), "not a link")
	_, warnings = DeletedFiles()
	assertVisibilityWarning(t, warnings, "deleted-open inspection partial", "1 descriptors")
}

func TestDeletedDescriptorStatFailureIsVisible(t *testing.T) {
	root := fixtureProcRoot(t)
	loop := filepath.Join(root, "loop (deleted)")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	fixtureDescriptor(t, 123, "3", loop, "0100001")
	deleted, warnings := DeletedFiles()
	if len(deleted) != 0 {
		t.Fatalf("unexpected retained files: %+v", deleted)
	}
	assertVisibilityWarning(t, warnings, "deleted-open inspection partial", "1 descriptors", "not exhaustive")
}

func TestVanishedProcessesAndDescriptorsDoNotWarn(t *testing.T) {
	root := fixtureProcRoot(t)
	path := filepath.Join(root, "app.log")
	writeFixtureFile(t, path, "log")
	files := scanFixtureFile(t, path)
	fixtureDescriptor(t, 123, "3", path, "0100001")
	fixtureDescriptor(t, 456, "3", filepath.Join(root, "gone (deleted)"), "0100001")
	fixtureDescriptor(t, 789, "3", path, "0100001")
	if err := os.Remove(filepath.Join(procRoot, "789", "fdinfo", "3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(procRoot, "999"), 0o755); err != nil {
		t.Fatal(err)
	}
	if warnings := AttributeWriters(files); len(warnings) != 0 {
		t.Fatalf("normal races must not warn: %v", warnings)
	}
	assertWriterPIDs(t, files[0], 123)
	if _, warnings := DeletedFiles(); len(warnings) != 0 {
		t.Fatalf("normal races must not warn: %v", warnings)
	}
}

func TestReadableReadOnlyDescriptorsDoNotWarn(t *testing.T) {
	root := fixtureProcRoot(t)
	path := filepath.Join(root, "app.log")
	writeFixtureFile(t, path, "log")
	files := scanFixtureFile(t, path)
	fixtureDescriptor(t, 123, "3", path, "0100000")
	if warnings := AttributeWriters(files); len(warnings) != 0 {
		t.Fatalf("readable read-only descriptors must not warn: %v", warnings)
	}
	assertWriterPIDs(t, files[0])
}

func assertVisibilityWarning(t *testing.T, warnings []string, fragments ...string) {
	t.Helper()
	if len(warnings) != 1 {
		t.Fatalf("expected one aggregate warning, got %v", warnings)
	}
	for _, fragment := range fragments {
		if !strings.Contains(warnings[0], fragment) {
			t.Fatalf("warning %q missing %q", warnings[0], fragment)
		}
	}
}

func TestInspectionFailuresIgnoreExitedTasksButKeepAccessFailures(t *testing.T) {
	var failures inspectionFailures
	for _, err := range []error{os.ErrNotExist, syscall.ESRCH, fmt.Errorf("read fdinfo: %w", syscall.ESRCH)} {
		failures.record(&failures.fdinfo, err)
	}
	if warnings := failures.warnings("writer attribution", "partial results"); len(warnings) != 0 {
		t.Fatalf("exited tasks must not warn: %v", warnings)
	}
	failures.record(&failures.fdinfo, &os.PathError{Op: "open", Path: "/proc/123/fdinfo/3", Err: syscall.EACCES})
	assertVisibilityWarning(t, failures.warnings("writer attribution", "partial results"), "1 fdinfo records")
}
