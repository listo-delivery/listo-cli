// Package update is how the CLI learns that a newer release exists and, for a
// copy installed by install.sh, replaces itself with it.
//
// Two jobs:
//
//   - method.go and notice.go decide WHICH upgrade command to name (npm, Homebrew
//     or the install script) and whether to say anything at all: at most once a
//     day, only on a terminal, never in JSON output, silent on every failure.
//   - apply.go downloads a release the way install.sh does (a signature over
//     checksums.txt checked against the release signing key, then the archive's
//     sha256) and swaps the binary in place with an atomic rename.
//
// The release signing key is signing_key.pem (embedded). install.sh carries the
// same key typed in the script, and a test holds the two equal.
package update

import (
	"os"
	"path/filepath"
	"strings"
)

// Environment variables the update code reads.
const (
	EnvInstallMethod = "LISTO_INSTALL_METHOD"
	EnvNoNotifier    = "LISTO_NO_UPDATE_NOTIFIER"
)

// MethodFile is written beside the binary by install.sh.
const MethodFile = ".listo-install-method"

// Method is how this copy of listo got onto the computer.
type Method string

const (
	MethodUnknown Method = ""
	MethodNPM     Method = "npm"
	MethodBrew    Method = "brew"
	MethodScript  Method = "script"
)

// DocsInstallURL is where an unknown install is sent.
const DocsInstallURL = "https://docs.listo.delivery/en/guides/cli"

// ReinstallCommand is the one line that installs the CLI from scratch.
const ReinstallCommand = "curl -fsSL https://cli.listo.delivery/install.sh | sh"

// DetectMethod reads, in this order: LISTO_INSTALL_METHOD (the npm shim sets it
// to npm), the .listo-install-method file beside the binary (install.sh writes
// script), and a path under a Homebrew Cellar. Anything else is MethodUnknown.
func DetectMethod(getenv func(string) string, exe string) Method {
	if m := parseMethod(getenv(EnvInstallMethod)); m != MethodUnknown {
		return m
	}
	if exe == "" {
		return MethodUnknown
	}
	if raw, err := os.ReadFile(filepath.Join(filepath.Dir(exe), MethodFile)); err == nil {
		if m := parseMethod(string(raw)); m != MethodUnknown {
			return m
		}
	}
	resolved := exe
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		resolved = r
	}
	if strings.Contains(filepath.ToSlash(resolved), "/Cellar/") {
		return MethodBrew
	}
	return MethodUnknown
}

func parseMethod(s string) Method {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "npm":
		return MethodNPM
	case "brew", "homebrew":
		return MethodBrew
	case "script":
		return MethodScript
	}
	return MethodUnknown
}

// UpgradeCommand is the command that upgrades a copy installed this way, or ""
// when the way is not known.
func (m Method) UpgradeCommand() string {
	switch m {
	case MethodNPM:
		return "npm i -g listo-cli"
	case MethodBrew:
		return "brew upgrade listo"
	case MethodScript:
		return "listo update"
	}
	return ""
}

// Manager is the name a person knows the package manager by.
func (m Method) Manager() string {
	switch m {
	case MethodNPM:
		return "npm"
	case MethodBrew:
		return "Homebrew"
	}
	return ""
}
