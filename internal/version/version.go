// Package version holds the CLI's version and the User-Agent built from it.
//
// The release build sets Version from the release tag with an -ldflags -X on
// this package's Version variable, and Default below is what a plain `go build`
// reports. The maintainers type the number once, in a file only their repository
// holds, and a script writes it here and into the npm manifests; the release
// workflow refuses a tag that disagrees with any of them.
package version

import "runtime"

// Default is the version a build without -ldflags reports. Written by the
// maintainers' version script; do not edit it by hand.
const Default = "0.1.0"

// Version is the running binary's version. A var, not a const, because -X can
// only set a string variable.
var Version = Default

// UserAgent is what every request to the API sends, so a request ledger can
// tell CLI traffic from SDK traffic: listo-cli/0.1.0 (linux/amd64).
func UserAgent() string {
	return "listo-cli/" + Version + " (" + runtime.GOOS + "/" + runtime.GOARCH + ")"
}
