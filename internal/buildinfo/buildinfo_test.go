package buildinfo

import (
	"strings"
	"testing"
)

func TestStringIncludesReleaseMetadata(t *testing.T) {
	oldVersion, oldCommit, oldDate := Version, Commit, Date
	Version, Commit, Date = "v1.2.3", "abc123", "2026-09-06"
	t.Cleanup(func() { Version, Commit, Date = oldVersion, oldCommit, oldDate })
	result := String()
	for _, expected := range []string{"diskc v1.2.3", "commit: abc123", "built: 2026-09-06", "platform:"} {
		if !strings.Contains(result, expected) {
			t.Fatalf("missing %q in %q", expected, result)
		}
	}
}
