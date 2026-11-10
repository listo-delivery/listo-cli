package login

import (
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
)

// OpenBrowser opens address in the person's default browser: `open` on macOS,
// `rundll32 url.dll,FileProtocolHandler` on Windows (an address with & survives
// it, where `cmd /c start` would cut it), `xdg-open` elsewhere. It starts the
// opener and does not wait for it. The caller prints the address whatever this
// returns, because a headless box has no browser to open.
func OpenBrowser(address string) error {
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("not a web address: %q", address)
	}
	name, args := browserCommand(runtime.GOOS, address)
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func browserCommand(goos, address string) (string, []string) {
	switch goos {
	case "darwin":
		return "open", []string{address}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", address}
	default:
		return "xdg-open", []string{address}
	}
}
