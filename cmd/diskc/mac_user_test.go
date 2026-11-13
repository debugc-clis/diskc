package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMacUserRoutingAndHelp(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"mac", "user", "--help"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "diskc mac user") {
			t.Fatalf("help route %v: stdout=%q stderr=%q", args, stdout.String(), stderr.String())
		}
	}
	for _, args := range [][]string{{"mac"}, {"mac", "unknown"}, {"mac", "user", "--depth", "-1"}, {"mac", "user", "unexpected"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Fatalf("usage route %v: stdout=%q stderr=%q", args, stdout.String(), stderr.String())
		}
	}
}

func TestMacUserInspectsSelectedHomeDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, subdir := range []string{"Library/Caches", "Library/Application Support"} {
		root := filepath.Join(home, subdir)
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "data.bin"), make([]byte, 8192), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"mac", "user", "--depth", "0", "--top", "1", "--json"}, &stdout, &stderr)
	if runtime.GOOS != "darwin" {
		if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "requires macOS") {
			t.Fatalf("platform guard: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		return
	}
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("user scan: code=%d stderr=%q", code, stderr.String())
	}
	var report struct {
		Scope string `json:"scope"`
		Home  string `json:"home"`
		Roots []struct {
			Path  string `json:"path"`
			Files []struct {
				Path string `json:"path"`
			} `json:"files"`
		} `json:"roots"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Scope != "user" || report.Home != home || len(report.Roots) != 2 {
		t.Fatalf("unexpected selected-home report: %s", stdout.String())
	}
	for _, root := range report.Roots {
		if !strings.HasPrefix(root.Path, home+string(filepath.Separator)) || len(root.Files) != 1 || filepath.Base(root.Files[0].Path) != "data.bin" {
			t.Fatalf("unexpected root evidence: %+v", root)
		}
	}
}
