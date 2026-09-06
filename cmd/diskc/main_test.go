package main

import (
	"testing"

	"github.com/debugc-clis/diskc/internal/proc"
)

func TestRemoveLegacyDeletedFlag(t *testing.T) {
	args := removeLegacyArgs([]string{"/var", "--deleted", "--top", "5"})
	if len(args) != 3 || args[0] != "/var" || args[1] != "--top" || args[2] != "5" {
		t.Fatalf("unexpected arguments: %#v", args)
	}
}

func TestDeletedFilesAreScopedByDevice(t *testing.T) {
	files := []proc.DeletedFile{{Path: "/a", Device: 1}, {Path: "/b", Device: 2}}
	result := deletedForDevice(files, 2)
	if len(result) != 1 || result[0].Path != "/b" {
		t.Fatalf("unexpected deleted files: %#v", result)
	}
}
