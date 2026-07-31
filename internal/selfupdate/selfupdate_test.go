package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mjovanovic0/kubectl-toolbox/internal/version"
)

func testChecker(t *testing.T, baseURL string) *Checker {
	t.Helper()
	return &Checker{
		HTTP:     baseURL2Client(),
		CacheDir: t.TempDir(),
		Now:      time.Now,
		BaseURL:  baseURL,
	}
}

func baseURL2Client() *http.Client { return &http.Client{Timeout: 5 * time.Second} }

func TestChecksumForParsesSha256sumFormat(t *testing.T) {
	sums := []byte(
		"aaaa  kubectl-toolbox_v0.2.0_linux_amd64.tar.gz\n" +
			"bbbb *kubectl-toolbox_v0.2.0_darwin_arm64.tar.gz\n" +
			"cccc  checksums.txt\n")

	got, err := checksumFor(sums, "kubectl-toolbox_v0.2.0_linux_amd64.tar.gz")
	if err != nil || got != "aaaa" {
		t.Errorf("linux entry: got %q err %v", got, err)
	}
	// sha256sum's binary marker must not defeat the match.
	got, err = checksumFor(sums, "kubectl-toolbox_v0.2.0_darwin_arm64.tar.gz")
	if err != nil || got != "bbbb" {
		t.Errorf("starred entry: got %q err %v", got, err)
	}
	if _, err := checksumFor(sums, "absent.tar.gz"); err == nil {
		t.Error("a missing entry must be an error, not an empty hash")
	}
}

// tarGz builds a release-shaped archive containing a fake binary.
func tarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	// A README alongside it, as the real archives have.
	for _, f := range []struct {
		n string
		b []byte
	}{{"README.md", []byte("readme")}, {name, content}} {
		if err := tw.WriteHeader(&tar.Header{Name: "./" + f.n, Mode: 0o755, Size: int64(len(f.b)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.b); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func TestExtractBinaryFindsItAmongOtherFiles(t *testing.T) {
	want := BinaryName
	if runtime.GOOS == "windows" {
		want += ".exe"
	}
	archive := tarGz(t, want, []byte("#!/bin/sh\necho hi\n"))
	got, err := extractBinary(archive, "x.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "echo hi") {
		t.Errorf("wrong file extracted: %q", got)
	}
}

func TestExtractBinaryErrorsWhenAbsent(t *testing.T) {
	archive := tarGz(t, "some-other-tool", []byte("x"))
	if _, err := extractBinary(archive, "x.tar.gz"); err == nil {
		t.Error("an archive without the binary must error")
	}
}

// The whole point of the checksum step: a tampered archive is refused.
func TestApplyRefusesOnChecksumMismatch(t *testing.T) {
	bin := "#!/bin/sh\nexit 0\n"
	archive := tarGz(t, BinaryName, []byte(bin))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "checksums.txt"):
			// Deliberately wrong hash.
			fmt.Fprintf(w, "%s  %s\n", strings.Repeat("0", 64), AssetName("v9.9.9"))
		default:
			w.Write(archive)
		}
	}))
	defer srv.Close()

	c := testChecker(t, srv.URL)
	rel := Release{TagName: "v9.9.9"}
	rel.Assets = append(rel.Assets,
		struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		}{AssetName("v9.9.9"), srv.URL + "/archive", 0},
		struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		}{"checksums.txt", srv.URL + "/checksums.txt", 0})

	dir := t.TempDir()
	victim := filepath.Join(dir, BinaryName)
	if err := os.WriteFile(victim, []byte("ORIGINAL"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := c.Apply(context.Background(), rel, Target{Path: victim, Writable: true})
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("want ErrChecksumMismatch, got %v", err)
	}
	// And crucially the existing binary must be untouched.
	b, _ := os.ReadFile(victim)
	if string(b) != "ORIGINAL" {
		t.Errorf("a failed update must not modify the installed binary, got %q", b)
	}
}

// A release without checksums must be refused rather than installed unverified.
func TestApplyRefusesWithoutChecksums(t *testing.T) {
	c := testChecker(t, "http://127.0.0.1:1")
	rel := Release{TagName: "v9.9.9"}
	rel.Assets = append(rel.Assets, struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	}{AssetName("v9.9.9"), "http://127.0.0.1:1/a", 0})

	err := c.Apply(context.Background(), rel, Target{Path: filepath.Join(t.TempDir(), "b"), Writable: true})
	if err == nil || !strings.Contains(err.Error(), "no checksums.txt") {
		t.Fatalf("want a refusal mentioning checksums, got %v", err)
	}
}

func TestApplyRefusesUnwritableTarget(t *testing.T) {
	c := testChecker(t, "http://127.0.0.1:1")
	err := c.Apply(context.Background(), Release{TagName: "v9.9.9"},
		Target{Path: "/usr/local/bin/kubectl-toolbox", Writable: false})
	if err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("want a permissions message, got %v", err)
	}
}

// replaceExecutable must leave a working binary or none at all.
func TestReplaceExecutableIsAtomicAndPreservesMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits and rename semantics differ on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, BinaryName)
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho old\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A stand-in that satisfies verifyRuns: it must print the binary name.
	newBin := []byte("#!/bin/sh\necho " + BinaryName + " version v9.9.9\n")
	if err := replaceExecutable(path, newBin); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "v9.9.9") {
		t.Errorf("binary not replaced: %q", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("mode not preserved: %v", fi.Mode().Perm())
	}
	// No staging files left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".toolbox-new-") {
			t.Errorf("staging file left behind: %s", e.Name())
		}
	}
}

// A download that cannot execute must not replace a working binary.
func TestReplaceExecutableRejectsABinaryThatDoesNotRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec semantics differ on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, BinaryName)
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho old\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceExecutable(path, []byte("\x00\x01not an executable")); err == nil {
		t.Fatal("expected a refusal")
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "echo old") {
		t.Errorf("the original binary must survive a bad download, got %q", got)
	}
}

// The cache exists so routine invocations do not spend a GitHub API call each.
func TestLatestUsesCacheUntilItExpires(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]string{"tag_name": "v9.9.9", "html_url": "http://example/x"})
	}))
	defer srv.Close()

	now := time.Now()
	c := testChecker(t, srv.URL)
	c.Now = func() time.Time { return now }

	if _, err := c.Latest(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Latest(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("second call should have been served from cache, got %d API calls", calls)
	}

	// force must bypass it.
	if _, err := c.Latest(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force should bypass the cache, got %d calls", calls)
	}

	// And so must expiry.
	now = now.Add(cacheTTL + time.Minute)
	if _, err := c.Latest(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("a stale cache should be refreshed, got %d calls", calls)
	}
}

// Being offline is normal on the hosts this runs on: it must be reported as such,
// never as a crash.
func TestLatestReportsOfflineCleanly(t *testing.T) {
	c := testChecker(t, "http://127.0.0.1:1")
	_, err := c.Latest(context.Background(), true)
	if !errors.Is(err, ErrOffline) {
		t.Fatalf("want ErrOffline, got %v", err)
	}
}

// The background notifier must stay silent for local builds and when disabled,
// and must never block the command.
func TestNotifyInBackgroundIsSilentWhenNotApplicable(t *testing.T) {
	c := testChecker(t, "http://127.0.0.1:1")

	// version.Version is "dev" under test, i.e. a local build.
	if msg := c.NotifyInBackground(context.Background())(); msg != "" {
		t.Errorf("a local build must not be offered updates, got %q", msg)
	}

	t.Setenv("TOOLBOX_NO_UPDATE_CHECK", "1")
	if !Disabled() {
		t.Error("TOOLBOX_NO_UPDATE_CHECK=1 should disable checking")
	}
	if msg := c.NotifyInBackground(context.Background())(); msg != "" {
		t.Errorf("disabled checking must produce no message, got %q", msg)
	}
}

func TestAssetNameMatchesTheReleaseWorkflow(t *testing.T) {
	got := AssetName("v0.2.0")
	want := fmt.Sprintf("kubectl-toolbox_v0.2.0_%s_%s", runtime.GOOS, runtime.GOARCH)
	if !strings.HasPrefix(got, want) {
		t.Fatalf("AssetName = %q, want prefix %q", got, want)
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(got, ".zip") {
		t.Errorf("windows assets are zips, got %q", got)
	}
	if runtime.GOOS != "windows" && !strings.HasSuffix(got, ".tar.gz") {
		t.Errorf("unix assets are tarballs, got %q", got)
	}
}

// Guard the sha256 helper wiring used by Apply.
func TestSha256Hex(t *testing.T) {
	sum := sha256.Sum256([]byte("abc"))
	if hex.EncodeToString(sum[:]) != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Error("sha256 hex encoding is not what Apply compares against")
	}
}

var _ = version.Current
