package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ErrChecksumMismatch means the downloaded archive is not the published one.
var ErrChecksumMismatch = errors.New("checksum mismatch")

// Target describes where the running binary lives and whether it can be replaced.
type Target struct {
	Path     string
	Writable bool
}

// Locate resolves the running executable, following symlinks so that a symlinked
// install is replaced at its real location rather than clobbering the link.
func Locate() (Target, error) {
	exe, err := os.Executable()
	if err != nil {
		return Target{}, fmt.Errorf("locating the running binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	t := Target{Path: exe}

	// Writability of the directory is what matters: the replacement is a rename
	// within it, not a write to the existing file.
	dir := filepath.Dir(exe)
	probe, err := os.CreateTemp(dir, ".toolbox-probe-*")
	if err == nil {
		probe.Close()
		os.Remove(probe.Name())
		t.Writable = true
	}
	return t, nil
}

// Apply downloads the release asset for this platform, verifies it against the
// release checksums, and replaces the running binary.
func (c *Checker) Apply(ctx context.Context, rel Release, target Target) error {
	if !target.Writable {
		return fmt.Errorf("%s is not writable by this user; re-run with sudo, or reinstall to a directory you own",
			filepath.Dir(target.Path))
	}

	asset := AssetName(rel.TagName)
	var archiveURL, sumsURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case asset:
			archiveURL = a.URL
		case "checksums.txt":
			sumsURL = a.URL
		}
	}
	if archiveURL == "" {
		return fmt.Errorf("release %s has no asset %q (unsupported platform %s/%s?)",
			rel.TagName, asset, runtime.GOOS, runtime.GOARCH)
	}
	if sumsURL == "" {
		// Refuse rather than install unverified content.
		return fmt.Errorf("release %s publishes no checksums.txt, so the download cannot be verified; "+
			"install manually from %s", rel.TagName, rel.HTMLURL)
	}

	dctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	sums, err := c.fetch(dctx, sumsURL, 1<<20)
	if err != nil {
		return fmt.Errorf("fetching checksums: %w", err)
	}
	want, err := checksumFor(sums, asset)
	if err != nil {
		return err
	}

	archive, err := c.fetch(dctx, archiveURL, maxArchiveBytes)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", asset, err)
	}

	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%w for %s: expected %s, got %s",
			ErrChecksumMismatch, asset, want, hex.EncodeToString(got[:]))
	}

	bin, err := extractBinary(archive, asset)
	if err != nil {
		return err
	}
	return replaceExecutable(target.Path, bin)
}

func (c *Checker) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", BinaryName)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// checksumFor pulls one entry out of a sha256sum-format file.
func checksumFor(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		// sha256sum writes "<hash>  <name>", with a possible "*" binary marker.
		if strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s", name)
}

// extractBinary pulls the executable out of a release archive.
func extractBinary(archive []byte, assetName string) ([]byte, error) {
	want := BinaryName
	if runtime.GOOS == "windows" {
		want += ".exe"
	}
	if strings.HasSuffix(assetName, ".zip") {
		return fromZip(archive, want)
	}
	return fromTarGz(archive, want)
}

func fromTarGz(archive []byte, want string) ([]byte, error) {
	zr, err := gzipReader(archive)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading archive: %w", err)
		}
		if filepath.Base(h.Name) != want || h.Typeflag != tar.TypeReg {
			continue
		}
		return io.ReadAll(io.LimitReader(tr, maxArchiveBytes))
	}
	return nil, fmt.Errorf("archive contains no %s", want)
}

func fromZip(archive []byte, want string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if filepath.Base(f.Name) != want {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, maxArchiveBytes))
	}
	return nil, fmt.Errorf("archive contains no %s", want)
}

// replaceExecutable installs newBin at path atomically.
//
// The new binary is written beside the target and renamed over it, so the
// filesystem never exposes a partially written executable. Windows cannot unlink a
// running image, so the old one is moved aside first and cleaned up on the next
// run.
func replaceExecutable(path string, newBin []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".toolbox-new-*")
	if err != nil {
		return fmt.Errorf("staging the new binary in %s: %w", dir, err)
	}
	staged := tmp.Name()
	defer os.Remove(staged) // no-op once the rename succeeds

	if _, err := tmp.Write(newBin); err != nil {
		tmp.Close()
		return fmt.Errorf("writing the new binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Preserve the mode of what we are replacing, falling back to 0755.
	mode := os.FileMode(0o755)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.Chmod(staged, mode); err != nil {
		return err
	}
	if err := verifyRuns(staged); err != nil {
		return fmt.Errorf("the downloaded binary does not run, refusing to install it: %w", err)
	}

	if runtime.GOOS == "windows" {
		old := path + ".old"
		os.Remove(old)
		if err := os.Rename(path, old); err != nil {
			return fmt.Errorf("moving the running binary aside: %w", err)
		}
		if err := os.Rename(staged, path); err != nil {
			os.Rename(old, path) // put it back
			return fmt.Errorf("installing the new binary: %w", err)
		}
		return nil
	}
	if err := os.Rename(staged, path); err != nil {
		return fmt.Errorf("installing the new binary over %s: %w", path, err)
	}
	return nil
}

// verifyRuns executes the staged binary so a corrupt or wrong-architecture
// download is caught before it replaces a working tool.
func verifyRuns(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	if !strings.Contains(string(out), BinaryName) {
		return fmt.Errorf("unexpected --version output: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// CleanupWindowsLeftover removes the previous binary a Windows update moved aside.
func CleanupWindowsLeftover() {
	if runtime.GOOS != "windows" {
		return
	}
	if exe, err := os.Executable(); err == nil {
		os.Remove(exe + ".old")
	}
}
