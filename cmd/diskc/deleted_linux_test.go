package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/debugc-clis/diskc/internal/report"
)

func TestRunDeletedOpenRequiresUnlinkedRegularFile(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skipf("Linux process descriptors unavailable: %v", err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	linkedPath := filepath.Join(root, "still-linked.log (deleted)")
	deletedPath := filepath.Join(root, "gone.log (deleted)")
	for _, path := range []string{linkedPath, deletedPath} {
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 8192), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Read-only descriptors still retain an unlinked inode. Keeping two open
	// also verifies that one process's descriptors do not duplicate the row.
	var retained *os.File
	for _, path := range []string{linkedPath, deletedPath, deletedPath} {
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { file.Close() })
		if path == deletedPath {
			retained = file
		}
	}
	if err := os.Remove(deletedPath); err != nil {
		t.Fatal(err)
	}
	info, err := retained.Stat()
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Nlink != 0 {
		t.Fatalf("fixture remains linked: %d links", stat.Nlink)
	}

	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			args := []string{root, "--sample", "0"}
			if format == "json" {
				args = append(args, "--json")
			}
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 0 {
				t.Fatalf("run returned %d, stderr=%s", code, stderr.String())
			}
			if format == "text" {
				before, deleted, found := strings.Cut(stdout.String(), "Deleted but still open (")
				if !found || !strings.Contains(before, linkedPath) {
					t.Fatalf("missing ordinary or retained-file evidence: %s", stdout.String())
				}
				deleted, _, _ = strings.Cut(deleted, "\nWarnings")
				if strings.Contains(deleted, strings.TrimSuffix(linkedPath, " (deleted)")) {
					t.Fatalf("linked suffix name misreported as deleted: %s", deleted)
				}
				if strings.Count(deleted, deletedPath) != 1 || !strings.Contains(deleted, fmt.Sprintf("(PID %d)", os.Getpid())) {
					t.Fatalf("retained file or owning PID missing or duplicated: %s", deleted)
				}
				return
			}
			var result report.Data
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
			}
			if len(result.Files) != 1 || result.Files[0].Path != linkedPath {
				t.Fatalf("ordinary suffix file lost from scanned files: %+v", result.Files)
			}
			matches := 0
			for _, file := range result.Deleted {
				if file.Path == linkedPath || file.Path == strings.TrimSuffix(linkedPath, " (deleted)") {
					t.Fatalf("linked suffix name misreported as deleted: %+v", file)
				}
				if file.Path != deletedPath {
					continue
				}
				matches++
				if file.ApparentBytes != info.Size() || file.AllocatedBytes != stat.Blocks*512 {
					t.Fatalf("retained sizes differ from descriptor stat: %+v; stat=%+v", file, stat)
				}
				if len(file.Writers) != 1 || file.Writers[0].PID != os.Getpid() {
					t.Fatalf("retaining process missing or duplicated: %+v", file.Writers)
				}
			}
			if matches != 1 {
				t.Fatalf("expected one retained row preserving the literal suffix; got %d: %+v", matches, result.Deleted)
			}
		})
	}
}
