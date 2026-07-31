// Package selfupdate finds newer releases on GitHub and replaces the running
// binary with one.
//
// Two properties matter more than convenience here. A downloaded archive is only
// installed after its SHA-256 matches the checksums file published alongside it,
// because a tool that overwrites its own binary from the network is otherwise a
// remote-code-execution path onto the user's machine. And the replacement is
// atomic: the new binary is staged beside the old one and moved into place with a
// rename, so an interrupted update never leaves a half-written executable.
package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mjovanovic0/kubectl-toolbox/internal/version"
)

const (
	// Repo is the source of releases.
	Repo = "mjovanovic0/kubectl-toolbox"
	// BinaryName is what the archive contains and what ends up on PATH.
	BinaryName = "kubectl-toolbox"

	// checkTimeout bounds the background check. Hosts that run this tool are
	// sometimes air-gapped, so the check must give up quickly and quietly rather
	// than delay the command the user actually asked for.
	checkTimeout = 3 * time.Second
	// downloadTimeout bounds an explicit update, which moves ~10 MiB.
	downloadTimeout = 5 * time.Minute
	// cacheTTL is how long a check result is reused. Without this, every
	// invocation would spend an API call and could hit GitHub's unauthenticated
	// rate limit of 60/hour.
	cacheTTL = 12 * time.Hour

	// maxArchiveBytes caps what will be read from a release asset.
	maxArchiveBytes = 256 << 20
)

// ErrOffline reports that the release list could not be reached at all.
var ErrOffline = errors.New("could not reach GitHub")

// Release is the subset of a GitHub release we use.
type Release struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

// AssetName is the archive for this platform, matching what the release workflow
// produces.
func AssetName(tag string) string {
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("%s_%s_%s_%s.%s", BinaryName, tag, runtime.GOOS, runtime.GOARCH, ext)
}

// Checker looks for newer releases.
type Checker struct {
	HTTP     *http.Client
	CacheDir string
	// Now is injectable so cache expiry is testable.
	Now func() time.Time
	// BaseURL is the GitHub API root, overridable in tests.
	BaseURL string
}

func New() *Checker {
	return &Checker{
		HTTP:     &http.Client{},
		CacheDir: defaultCacheDir(),
		Now:      time.Now,
		BaseURL:  "https://api.github.com",
	}
}

func defaultCacheDir() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "kubectl-toolbox")
	}
	return ""
}

// Disabled reports whether update checking has been turned off. Air-gapped and CI
// environments need a way to opt out that does not involve passing a flag to every
// invocation.
func Disabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("TOOLBOX_NO_UPDATE_CHECK")))
	return v == "1" || v == "true" || v == "yes"
}

type cacheEntry struct {
	CheckedAt time.Time `json:"checkedAt"`
	TagName   string    `json:"tagName"`
	HTMLURL   string    `json:"htmlUrl"`
}

func (c *Checker) cachePath() string {
	if c.CacheDir == "" {
		return ""
	}
	return filepath.Join(c.CacheDir, "latest-release.json")
}

func (c *Checker) readCache() (cacheEntry, bool) {
	p := c.cachePath()
	if p == "" {
		return cacheEntry{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return cacheEntry{}, false
	}
	var e cacheEntry
	if json.Unmarshal(b, &e) != nil || e.TagName == "" {
		return cacheEntry{}, false
	}
	if c.Now().Sub(e.CheckedAt) > cacheTTL {
		return e, false // present but stale
	}
	return e, true
}

func (c *Checker) writeCache(e cacheEntry) {
	p := c.cachePath()
	if p == "" {
		return
	}
	if os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	// Best effort: a cache write failure must never affect the command.
	_ = os.WriteFile(p, b, 0o644)
}

// Latest returns the newest release, using the cache unless force is set.
func (c *Checker) Latest(ctx context.Context, force bool) (Release, error) {
	if !force {
		if e, fresh := c.readCache(); fresh {
			return Release{TagName: e.TagName, HTMLURL: e.HTMLURL}, nil
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/repos/"+Repo+"/releases/latest", nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", BinaryName+"/"+version.Version)
	// An unauthenticated check is fine, but honour a token when one is present so
	// heavy users are not rate-limited.
	if tok := firstEnv("GH_TOKEN", "GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("%w: GitHub returned %s", ErrOffline, resp.Status)
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return Release{}, err
	}
	if rel.TagName == "" {
		return Release{}, errors.New("release has no tag name")
	}
	c.writeCache(cacheEntry{CheckedAt: c.Now(), TagName: rel.TagName, HTMLURL: rel.HTMLURL})
	return rel, nil
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// Available reports a newer release, or an empty tag when up to date.
func (c *Checker) Available(ctx context.Context, force bool) (Release, bool, error) {
	if !version.IsRelease() {
		// A dev or dirty build has no meaningful position in the version order.
		return Release{}, false, nil
	}
	rel, err := c.Latest(ctx, force)
	if err != nil {
		return Release{}, false, err
	}
	newer, err := version.Newer(version.Version, rel.TagName)
	if err != nil {
		return Release{}, false, err
	}
	return rel, newer, nil
}

// NotifyInBackground starts the check and returns a function that reports the
// result. It never returns an error: a failed update check must not affect the
// exit status or output of the command the user actually ran.
func (c *Checker) NotifyInBackground(ctx context.Context) func() string {
	if Disabled() || !version.IsRelease() {
		return func() string { return "" }
	}
	type result struct{ msg string }
	ch := make(chan result, 1)

	go func() {
		cctx, cancel := context.WithTimeout(ctx, checkTimeout)
		defer cancel()
		rel, newer, err := c.Available(cctx, false)
		if err != nil || !newer {
			ch <- result{}
			return
		}
		ch <- result{fmt.Sprintf("A newer release is available: %s (you have %s). Run `%s update` to install it.",
			rel.TagName, version.Version, strings.TrimPrefix(BinaryName, "kubectl-"))}
	}()

	return func() string {
		select {
		case r := <-ch:
			return r.msg
		case <-time.After(checkTimeout + time.Second):
			// Do not wait on a hung network for longer than the check allows.
			return ""
		}
	}
}
