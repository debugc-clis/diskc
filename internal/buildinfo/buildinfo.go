package buildinfo

import (
	"fmt"
	"runtime"
)

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

func String() string {
	return fmt.Sprintf("diskc %s\ncommit: %s\nbuilt: %s\ngo: %s\nplatform: %s/%s", Version, Commit, Date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
