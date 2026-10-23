//go:build darwin

package mount

import "testing"

func TestDarwinInternalMount(t *testing.T) {
	for _, path := range []string{"/System/Volumes/Preboot", "/System/Volumes/Update", "/System/Volumes/VM"} {
		if !darwinInternalMount(path) {
			t.Fatalf("expected %s to be treated as an internal mount", path)
		}
	}
	for _, path := range []string{"/", "/System/Volumes/Data", "/Volumes/External"} {
		if darwinInternalMount(path) {
			t.Fatalf("did not expect %s to be treated as an internal mount", path)
		}
	}
}
