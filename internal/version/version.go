// Package version holds build-time identifying information, injected via
// -ldflags -X at compile time (see Dockerfile), or for Commit read from
// RENDER_GIT_COMMIT at startup. Both
// vars keep their zero-value defaults for `go run`/`go test` and any build
// that doesn't pass the flags.
package version

import (
	"fmt"
	"os"
	"strings"
	"time"
)

var (
	// Version is the tagged release version this binary was built from
	// (e.g. "1.14.1"), or "dev" outside of a release build.
	Version = "dev"
	// Commit is the short git commit SHA this binary was built from.
	Commit = "unknown"
)

// Render sets RENDER_GIT_COMMIT on a running service, so a build that wasn't given the commit still
// says which one it is.
func init() { Commit = commitOr(Commit, os.Getenv("RENDER_GIT_COMMIT")) }

// commitOr returns commit, or the short form of sha when commit is "unknown" and sha is set.
func commitOr(commit, sha string) string {
	if commit != "unknown" || sha == "" {
		return commit
	}
	return sha[:min(len(sha), 7)]
}

// Started is when this process booted. It's captured at package
// initialization, which happens before main and before any server or worker
// wiring, so it's the earliest moment the process can observe about itself.
// Restarts (deploys, crashes, instance replacement) reset it -- this is
// process uptime, not service-lifetime uptime.
var Started = time.Now()

// String renders Version and Commit for display, e.g. "1.14.1 (abc1234)".
func String() string {
	return Version + " (" + Commit + ")"
}

// Uptime is how long this process has been running.
func Uptime() time.Duration {
	return time.Since(Started)
}

// FormatUptime renders a duration in descending units, e.g. "3d 4h 12m 5s".
// Leading zero units are dropped ("12m 5s", not "0d 0h 12m 5s") and
// sub-second durations round down to "0s" rather than printing empty.
func FormatUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)

	days := int64(d / (24 * time.Hour))
	hours := int64(d/time.Hour) % 24
	minutes := int64(d/time.Minute) % 60
	seconds := int64(d/time.Second) % 60

	parts := make([]string, 0, 4)
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 || len(parts) > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 || len(parts) > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	parts = append(parts, fmt.Sprintf("%ds", seconds))

	return strings.Join(parts, " ")
}
