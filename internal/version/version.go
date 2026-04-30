package version

import "fmt"

// These values are set by GoReleaser via ldflags for release builds.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

func String() string {
	if Version == "" {
		return "dev"
	}
	return Version
}

func Details() string {
	return fmt.Sprintf("%s (commit %s, built %s)", String(), Commit, Date)
}

func IsDev() bool {
	return Version == "" || Version == "dev"
}
