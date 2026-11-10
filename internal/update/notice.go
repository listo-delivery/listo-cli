package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/listo-delivery/listo-cli/internal/version"
)

// Repo is the public repository releases are published from.
const Repo = "listo-delivery/listo-cli"

// The two addresses the update code talks to. Variables so tests point them at
// a local server; nothing in a shipped build changes them.
var (
	// LatestURL answers the latest release as JSON ("tag_name").
	LatestURL = "https://api.github.com/repos/" + Repo + "/releases/latest"
	// DownloadBase holds <base>/download/<tag>/<file> for every release asset.
	DownloadBase = "https://github.com/" + Repo + "/releases"
)

// CheckInterval is the least time between two checks.
const CheckInterval = 24 * time.Hour

// NoticeTimeout bounds the once-a-day check so it can never hold a command.
const NoticeTimeout = 2 * time.Second

const stateFile = "update-check.json"

type state struct {
	CheckedAt time.Time `json:"checked_at"`
}

// Due reports whether a check is allowed now: the stamp in dir is missing,
// unreadable, in the future, or at least CheckInterval old.
func Due(dir string, now time.Time) bool {
	raw, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return true
	}
	var s state
	if json.Unmarshal(raw, &s) != nil {
		return true
	}
	if s.CheckedAt.After(now) {
		return true
	}
	return now.Sub(s.CheckedAt) >= CheckInterval
}

// Stamp records that a check was made at now. It is written BEFORE the request,
// so a computer that is offline does not retry on every command.
func Stamp(dir string, now time.Time) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(state{CheckedAt: now})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, stateFile), raw, 0o600)
}

// LatestTag asks the release API for the newest release's tag ("v0.2.0").
func LatestTag(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", version.UserAgent())
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("release lookup answered %d", res.StatusCode)
	}
	var body struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body); err != nil {
		return "", err
	}
	if strings.TrimSpace(body.Tag) == "" {
		return "", errors.New("release lookup carried no tag")
	}
	return body.Tag, nil
}

// Newer reports whether latest is a higher release than current. Both may carry
// a leading "v". A version that is not MAJOR.MINOR.PATCH answers false, so
// nothing odd ever prompts an upgrade.
func Newer(current, latest string) bool {
	c, ok1 := parseSemver(current)
	l, ok2 := parseSemver(latest)
	if !ok1 || !ok2 {
		return false
	}
	for i := range c {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func parseSemver(s string) ([3]int, bool) {
	var out [3]int
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// Bare drops a leading "v" from a tag.
func Bare(tag string) string { return strings.TrimPrefix(strings.TrimSpace(tag), "v") }

// NoticeOptions is everything CheckForNotice needs, so the command layer builds
// it from its Runtime and a test builds it by hand.
type NoticeOptions struct {
	Dir     string // the config directory holding the stamp
	Now     time.Time
	Current string       // the running version
	HTTP    *http.Client // nil means a client with NoticeTimeout
	URL     string       // empty means LatestURL
}

// CheckForNotice is the once-a-day check. It returns the newer tag when one
// exists and "" for everything else, a failure included: a notice never errors.
func CheckForNotice(ctx context.Context, o NoticeOptions) string {
	if !Due(o.Dir, o.Now) {
		return ""
	}
	if err := Stamp(o.Dir, o.Now); err != nil {
		return ""
	}
	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: NoticeTimeout}
	}
	url := o.URL
	if url == "" {
		url = LatestURL
	}
	ctx, cancel := context.WithTimeout(ctx, NoticeTimeout)
	defer cancel()
	tag, err := LatestTag(ctx, client, url)
	if err != nil || !Newer(o.Current, tag) {
		return ""
	}
	return tag
}
