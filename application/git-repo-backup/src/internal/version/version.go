// Package version exposes the build-time version of the backup binary.
package version

// Version is set via -ldflags at build time; dev builds keep the default.
var Version = "dev"

// Get returns the current build version string.
func Get() string {
	return Version
}
