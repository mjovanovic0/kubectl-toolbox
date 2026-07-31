package version

import "testing"

// A string compare gets these wrong, which is why semver ordering is used.
func TestNewerUsesSemverOrdering(t *testing.T) {
	cases := []struct {
		have, want string
		newer      bool
	}{
		{"v0.1.0", "v0.2.0", true},
		{"v0.2.0", "v0.1.0", false},
		{"v0.2.0", "v0.2.0", false},
		// Numeric, not lexical: "v0.10.0" < "v0.9.0" as strings.
		{"v0.9.0", "v0.10.0", true},
		{"v0.10.0", "v0.9.0", false},
		// A prerelease sorts below its release.
		{"v0.2.0-rc.1", "v0.2.0", true},
		{"v0.2.0", "v0.2.0-rc.1", false},
		{"v1.0.0-rc.1", "v1.0.0-rc.2", true},
		// A missing "v" is tolerated on either side.
		{"0.1.0", "v0.2.0", true},
		{"v0.1.0", "0.2.0", true},
	}
	for _, c := range cases {
		got, err := Newer(c.have, c.want)
		if err != nil {
			t.Errorf("Newer(%q,%q) errored: %v", c.have, c.want, err)
			continue
		}
		if got != c.newer {
			t.Errorf("Newer(%q,%q) = %v, want %v", c.have, c.want, got, c.newer)
		}
	}
}

func TestNewerRejectsNonSemver(t *testing.T) {
	for _, bad := range []string{"dev", "", "not-a-version", "v1..2"} {
		if _, err := Newer(bad, "v1.0.0"); err == nil {
			t.Errorf("Newer(%q, ...) should have errored", bad)
		}
	}
}

// A `git describe` string parses as valid semver -- "v0.1.0-7-gabc1234" has
// prerelease "7-gabc1234" -- and therefore sorts BELOW the v0.1.0 release. If such
// a build were ever compared, the tool would offer to overwrite a developer's own
// binary with an older published one. IsRelease is the guard that prevents it, so
// assert both halves of that contract explicitly.
func TestDescribeBuildsSortBelowTheirTagAndAreGatedOut(t *testing.T) {
	const dev = "v0.1.0-7-gabc1234"

	newer, err := Newer(dev, "v0.1.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !newer {
		t.Fatal("precondition: a describe string sorts below its tag")
	}
	// Which is precisely why it must never reach a comparison.
	if isRelease(dev) {
		t.Error("a describe build must not be treated as a release, or it would be 'upgraded' to its own tag")
	}
}

// Only a clean release tag may trigger an update offer. Overwriting a developer's
// own build with a published binary would be wrong.
func TestIsReleaseRejectsLocalBuilds(t *testing.T) {
	releases := []string{"v0.1.0", "v1.2.3", "v0.2.0-rc.1", "0.1.0"}
	locals := []string{
		"dev",
		"",
		"v0.1.0-dirty",
		"v0.1.0-7-gd6f89d6",       // git describe past a tag
		"v0.1.0-7-gd6f89d6-dirty", // ... and dirty
	}
	for _, v := range releases {
		if !isRelease(v) {
			t.Errorf("isRelease(%q) = false, want true", v)
		}
	}
	for _, v := range locals {
		if isRelease(v) {
			t.Errorf("isRelease(%q) = true, want false", v)
		}
	}
}
