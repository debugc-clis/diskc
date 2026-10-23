package cliutil

import (
	"errors"
	"testing"
)

func TestUsageErrorClassification(t *testing.T) {
	base := errors.New("bad flag")
	err := Usage(base)
	if !IsUsage(err) || !errors.Is(err, base) || err.Error() != base.Error() {
		t.Fatalf("unexpected usage error: %v", err)
	}
	if IsUsage(base) || Usage(nil) != nil {
		t.Fatal("ordinary and nil errors must not be usage errors")
	}
}
