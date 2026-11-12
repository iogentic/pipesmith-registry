// Package version holds the version of the build, set by the linker.
package version

// Version is replaced at build time through -ldflags -X.
var Version = "0.0.0-dev"
