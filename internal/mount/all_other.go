//go:build !linux && !darwin

package mount

import (
	"fmt"
	"runtime"
)

func All() ([]string, error) {
	return nil, fmt.Errorf("mount discovery is not supported on %s", runtime.GOOS)
}
