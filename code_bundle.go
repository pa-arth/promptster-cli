package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// BundleResult describes a tarball ready to upload.
type BundleResult struct {
	Path      string   // temp file path; caller is responsible for removing
	Size      int64    // bytes on disk (gzip-compressed)
	SHA256Hex string   // SHA-256 of the compressed tarball
	FileCount int      // number of files included after filters
	FileTree  []string // post-filter relative paths included
}

// bundleWorkspace tars+gzips the candidate's workspace into a temp file
// suitable for HTTP PUT to a signed S3-compatible URL. Walks `git ls-files`
// output (respects .gitignore), applies shouldBundleFile, skips symlinks.
//
// Files that differ from baseSha — the candidate's own content, the only
// bytes not already public in the problem repo — are secret-scrubbed via
// redactBytes before they go into the tar; everything else streams through
// untouched. The SHA-256 is computed over the final (scrubbed) tarball, so
// integrity verification is unaffected.
func bundleWorkspace(taskRoot, baseSha string) (BundleResult, error) {
	if !isGitRepository(taskRoot) {
		return BundleResult{}, errors.New("not a git repository")
	}

	files, err := listTrackedFiles(taskRoot)
	if err != nil {
		return BundleResult{}, fmt.Errorf("git ls-files: %w", err)
	}

	// nil means "couldn't determine the changed set" — scrub every file
	// rather than risk shipping an unscanned secret.
	scrubSet := changedFileSet(taskRoot, baseSha)

	tmpFile, err := os.CreateTemp("", "promptster-bundle-*.tar.gz")
	if err != nil {
		return BundleResult{}, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	hasher := sha256.New()
	writer := io.MultiWriter(tmpFile, hasher)
	gz, err := gzip.NewWriterLevel(writer, gzip.DefaultCompression)
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return BundleResult{}, fmt.Errorf("gzip writer: %w", err)
	}
	tw := tar.NewWriter(gz)

	var totalBytes int64
	included := make([]string, 0, len(files))

	for _, rel := range files {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		full := filepath.Join(taskRoot, rel)

		info, err := os.Lstat(full)
		if err != nil {
			// File gone since ls-files ran — skip silently.
			continue
		}
		// Symlinks are a security hazard during extract (could point outside
		// the workspace). Skip them entirely.
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if !shouldBundleFile(rel, info.Size()) {
			continue
		}

		// Scrub before the size accounting so the total cap reflects the
		// bytes actually written to the tar, which redaction can change.
		var scrubbed []byte
		doScrub := scrubSet == nil || scrubSet[rel]
		entrySize := info.Size()
		if doScrub {
			scrubbed, err = readScrubbedFile(full)
			if err != nil {
				tw.Close()
				gz.Close()
				tmpFile.Close()
				os.Remove(tmpPath)
				return BundleResult{}, fmt.Errorf("tar entry %s: %w", rel, err)
			}
			entrySize = int64(len(scrubbed))
		}

		totalBytes += entrySize
		if totalBytes > maxBundleTotalBytes {
			tw.Close()
			gz.Close()
			tmpFile.Close()
			os.Remove(tmpPath)
			return BundleResult{}, fmt.Errorf(
				"workspace too large — exceeds %d MB after exclusions",
				maxBundleTotalBytes/(1024*1024),
			)
		}

		if doScrub {
			err = writeBundleEntryBytes(tw, rel, scrubbed)
		} else {
			err = writeBundleEntry(tw, full, rel, info.Size())
		}
		if err != nil {
			tw.Close()
			gz.Close()
			tmpFile.Close()
			os.Remove(tmpPath)
			return BundleResult{}, fmt.Errorf("tar entry %s: %w", rel, err)
		}
		included = append(included, rel)
	}

	if err := tw.Close(); err != nil {
		gz.Close()
		tmpFile.Close()
		os.Remove(tmpPath)
		return BundleResult{}, fmt.Errorf("tar close: %w", err)
	}
	if err := gz.Close(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return BundleResult{}, fmt.Errorf("gzip close: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return BundleResult{}, fmt.Errorf("close temp: %w", err)
	}

	if len(included) == 0 {
		os.Remove(tmpPath)
		return BundleResult{}, errors.New("no files to submit — did you make any changes?")
	}

	stat, err := os.Stat(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		return BundleResult{}, fmt.Errorf("stat temp: %w", err)
	}

	return BundleResult{
		Path:      tmpPath,
		Size:      stat.Size(),
		SHA256Hex: hex.EncodeToString(hasher.Sum(nil)),
		FileCount: len(included),
		FileTree:  included,
	}, nil
}

// changedFileSet returns the repo-relative paths that differ between the
// working tree and the assessment's base commit — i.e. the candidate's own
// content, the only part of the bundle that can carry their secrets. Returns
// nil when no base is determinable (callers then scrub everything).
func changedFileSet(taskRoot, baseSha string) map[string]bool {
	args := []string{"-C", taskRoot, "diff", "--name-only", "-z"}
	switch {
	case baseSha != "":
		args = append(args, baseSha)
	case hasHeadParent(taskRoot):
		args = append(args, "HEAD~1")
	default:
		return nil
	}
	// Any non-zero exit means the set is unknowable — partial stdout from a
	// failing git would otherwise be treated as the complete changed set and
	// every file missing from it would ship verbatim, unscrubbed. (Plain
	// `git diff --name-only` exits 0 when it succeeds; only --exit-code
	// variants use exit 1 to signal "differences found".)
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return nil
	}
	set := make(map[string]bool)
	for _, p := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if p != "" {
			set[p] = true
		}
	}
	return set
}

// readScrubbedFile reads the file fully and runs secret redaction over text
// content. Binary content is returned verbatim — pattern replacement inside
// binaries would corrupt them, and key material in binary formats isn't
// recoverable by a regex scanner anyway.
func readScrubbedFile(fullPath string) ([]byte, error) {
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, err
	}
	if !looksBinary(data) {
		data = redactBytes(data)
	}
	return data, nil
}

// writeBundleEntryBytes writes an in-memory (already scrubbed) file as a tar
// entry, sized to the actual content.
func writeBundleEntryBytes(tw *tar.Writer, relPath string, data []byte) error {
	header := &tar.Header{
		Name:     relPath,
		Mode:     0644,
		Size:     int64(len(data)),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// looksBinary sniffs for a NUL byte in the first 8KB — the same heuristic
// git itself uses to classify files as binary.
func looksBinary(data []byte) bool {
	n := len(data)
	if n > 8192 {
		n = 8192
	}
	return bytes.IndexByte(data[:n], 0) >= 0
}

func listTrackedFiles(taskRoot string) ([]string, error) {
	cmd := exec.Command("git", "-C", taskRoot, "ls-files", "-z")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	results := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			results = append(results, p)
		}
	}
	return results, nil
}

func writeBundleEntry(tw *tar.Writer, fullPath, relPath string, size int64) error {
	header := &tar.Header{
		Name:     relPath,
		Mode:     0644,
		Size:     size,
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	f, err := os.Open(fullPath)
	if err != nil {
		return err
	}
	defer f.Close()
	written, err := io.Copy(tw, f)
	if err != nil {
		return err
	}
	// Defensive: if the file shrank between Lstat and Open, pad with zeros so
	// the tar entry's declared Size matches what was written. (Underflow here
	// would make the tar file structurally invalid.)
	if written < size {
		pad := make([]byte, size-written)
		if _, err := tw.Write(pad); err != nil {
			return err
		}
	}
	return nil
}
