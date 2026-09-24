package service

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Release archive extraction for the updater.
//
// An archive is attacker-influenced input even when it comes from the fork, so
// extraction is bounded and structural rather than "find a file and copy it":
// only a regular file whose base name is the sub2api binary is accepted, entry
// names may not traverse out of the archive, symlinks and other special entries
// are never materialised, and both the declared and the actual uncompressed size
// are capped so a decompression bomb cannot fill the disk.
//
// .zip (windows) and .tar/.tar.gz/.tgz (linux, darwin) are both handled; the
// dispatch is by extension, and anything else is copied through under the same
// size bound.

// maxExtractedBinarySize caps the uncompressed size of the accepted binary. It
// is a variable so tests can exercise the bound without materialising 500MB.
var maxExtractedBinarySize int64 = 500 * 1024 * 1024

// archiveBinaryNames are the file names the updater accepts as the new binary.
var archiveBinaryNames = []string{"sub2api", "sub2api.exe"}

func isArchiveBinaryName(name string) bool {
	for _, want := range archiveBinaryNames {
		if name == want {
			return true
		}
	}
	return false
}

// safeArchiveEntryName rejects entry names that could address anything outside
// the archive's own directory tree and returns the entry's base name.
//
// Absolute paths, drive-qualified paths (C:\...), UNC paths and any name
// containing ".." are refused instead of being sanitised, so a malformed archive
// fails the update rather than being silently rewritten.
//
// The checks are deliberately independent of the host: the archive is inspected
// on whatever platform the updater runs on, so a Windows-style name must be
// rejected on Linux too, where filepath.VolumeName("C:\\x") is empty. Any ":"
// is refused for the same reason — it also covers NTFS alternate data streams
// ("sub2api.exe:evil"), which name a hidden stream rather than the binary.
func safeArchiveEntryName(name string) (string, error) {
	if strings.Contains(name, "..") {
		return "", fmt.Errorf("path traversal attempt detected: %s", name)
	}
	if strings.ContainsRune(name, ':') {
		return "", fmt.Errorf("drive-qualified or stream-qualified path in archive: %s", name)
	}

	// "\" and "/" both start an absolute or UNC path once normalised.
	normalised := strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(normalised, "/") ||
		filepath.IsAbs(normalised) ||
		filepath.VolumeName(name) != "" ||
		filepath.VolumeName(normalised) != "" {
		return "", fmt.Errorf("absolute path in archive: %s", name)
	}
	if strings.TrimSpace(normalised) == "" {
		return "", fmt.Errorf("empty entry name in archive")
	}

	return filepath.Base(normalised), nil
}

// extractBinaryFromArchive extracts the sub2api binary from a release archive
// into destPath.
func extractBinaryFromArchive(archivePath, destPath string) error {
	switch {
	case strings.HasSuffix(strings.ToLower(archivePath), ".zip"):
		return extractBinaryFromZip(archivePath, destPath)
	case strings.Contains(archivePath, ".tar"):
		return extractBinaryFromTar(archivePath, destPath)
	default:
		return extractBinaryFromPlainFile(archivePath, destPath)
	}
}

func extractBinaryFromTar(archivePath, destPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	var reader io.Reader = f
	if strings.HasSuffix(archivePath, ".gz") || strings.HasSuffix(archivePath, ".tgz") {
		gzr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer func() { _ = gzr.Close() }()
		reader = gzr
	}

	tr := tar.NewReader(reader)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		baseName, err := safeArchiveEntryName(hdr.Name)
		if err != nil {
			return err
		}

		// Only a regular file can become the running binary: directories and
		// special entries (symlinks, devices, fifos) are skipped.
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if !isArchiveBinaryName(baseName) {
			continue
		}

		return writeBounded(destPath, tr, hdr.Size)
	}
	return fmt.Errorf("binary not found in archive")
}

func extractBinaryFromZip(archivePath, destPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	stat, err := f.Stat()
	if err != nil {
		return err
	}

	zr, err := zip.NewReader(f, stat.Size())
	if err != nil {
		return err
	}

	for _, entry := range zr.File {
		baseName, err := safeArchiveEntryName(entry.Name)
		if err != nil {
			return err
		}

		// Reject anything that is not a plain regular file. A symlink entry
		// named "sub2api" must never be followed or materialised, and a
		// mode-less entry (no type bits) is the conventional regular file.
		mode := entry.Mode()
		if mode&os.ModeSymlink != 0 || mode&os.ModeType != 0 {
			continue
		}
		if !isArchiveBinaryName(baseName) {
			continue
		}

		// Bound the declared size before decompressing, then bound the bytes
		// actually produced: a header can lie in either direction.
		if entry.UncompressedSize64 > uint64(maxExtractedBinarySize) {
			return fmt.Errorf("binary too large: %d bytes (max %d)", entry.UncompressedSize64, maxExtractedBinarySize)
		}

		rc, err := entry.Open()
		if err != nil {
			return err
		}
		writeErr := writeBounded(destPath, rc, int64(entry.UncompressedSize64))
		closeErr := rc.Close()
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	return fmt.Errorf("binary not found in archive")
}

// extractBinaryFromPlainFile copies a release payload that is not an archive,
// under the same size bound.
func extractBinaryFromPlainFile(archivePath, destPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	stat, err := f.Stat()
	if err != nil {
		return err
	}
	return writeBounded(destPath, f, stat.Size())
}

// writeBounded copies r into destPath, refusing to write more than
// maxExtractedBinarySize bytes even when the source overstates or understates
// its own size, and refusing a result that does not match the size its header
// declared. An archive entry that yields nothing, or less than it promised, must
// never be installed over the running executable.
func writeBounded(destPath string, r io.Reader, declaredSize int64) error {
	if declaredSize > maxExtractedBinarySize {
		return fmt.Errorf("binary too large: %d bytes (max %d)", declaredSize, maxExtractedBinarySize)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}

	written, copyErr := io.Copy(out, io.LimitReader(r, maxExtractedBinarySize+1))
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(destPath)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(destPath)
		return closeErr
	}
	if written > maxExtractedBinarySize {
		_ = os.Remove(destPath)
		return fmt.Errorf("binary too large: extracted more than %d bytes", maxExtractedBinarySize)
	}
	if written == 0 {
		_ = os.Remove(destPath)
		return fmt.Errorf("archive entry for the binary is empty")
	}
	if written != declaredSize {
		_ = os.Remove(destPath)
		return fmt.Errorf("archive entry holds %d bytes but declares %d", written, declaredSize)
	}
	return nil
}
