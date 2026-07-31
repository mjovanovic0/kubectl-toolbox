// Package version holds the build stamp and the comparison rules used to decide
// whether a newer release exists.
package version

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"

	"golang.org/x/mod/semver"
)

// describeSuffix matches the "-<commits>-g<sha>" tail that `git describe` appends
// when the build is past a tag.
var describeSuffix = regexp.MustCompile(`-\d+-g[0-9a-f]{7,}$`)

// These are set at build time with -ldflags. The zero values are what a plain
// `go build` or `go install` from source produces.
var (
	Version   = "dev"
	Commit    = ""
	BuildDate = ""
)

// Info describes this build.
type Info struct {
	Version   string
	Commit    string
	BuildDate string
	GoVersion string
	Platform  string
}

func Current() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		BuildDate: BuildDate,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}

func (i Info) String() string {
	s := i.Version
	if i.Commit != "" {
		s += " (" + i.Commit + ")"
	}
	return s
}

// IsRelease reports whether this build came from a clean release tag.
//
// Anything else -- "dev", a `git describe` string with commits-since-tag, or a
// dirty tree -- is a local build. Offering to overwrite a developer's own binary
// with a published release would be obnoxious and probably wrong, so update checks
// are skipped for those.
func IsRelease() bool { return isRelease(Version) }

func isRelease(v string) bool {
	if v == "" || v == "dev" {
		return false
	}
	if strings.Contains(v, "-dirty") {
		return false
	}
	// `git describe` yields v0.1.0-7-gabc1234 for a build past a tag. A genuine
	// prerelease (v0.2.0-rc.1) has no such "-<n>-g<sha>" tail.
	if describeSuffix.MatchString(v) {
		return false
	}
	return semver.IsValid(canonical(v))
}

// canonical normalises a tag into something semver understands: it requires a
// leading "v".
func canonical(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}

// Newer reports whether have < want, using semver ordering so that prereleases
// sort below their release (v0.2.0-rc.1 < v0.2.0) and numeric segments compare
// numerically (v0.10.0 > v0.9.0), which a string compare gets wrong.
func Newer(have, want string) (bool, error) {
	h, w := canonical(have), canonical(want)
	if !semver.IsValid(h) {
		return false, fmt.Errorf("current version %q is not valid semver", have)
	}
	if !semver.IsValid(w) {
		return false, fmt.Errorf("candidate version %q is not valid semver", want)
	}
	return semver.Compare(h, w) < 0, nil
}
