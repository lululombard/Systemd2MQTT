// Package version holds build metadata injected by GoReleaser through ldflags.
package version

import (
	"fmt"
	"runtime"
)

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns the long form used in logs and --version.
func String() string {
	return fmt.Sprintf("%s (%s, %s, %s %s/%s)", Version, Commit, Date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// Short returns just the version number.
func Short() string { return Version }
