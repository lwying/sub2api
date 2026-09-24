//go:build unit

package service

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Archive extraction is the point where a release payload stops being data and
// starts being the file the operating system will execute, so these tests pin
// the structural guarantees rather than just the happy path.

type testZipEntry struct {
	name string
	body []byte
	mode os.FileMode
}

func writeTestZip(t *testing.T, entries ...testZipEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "release.zip")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		w, err := zw.CreateHeader(header)
		require.NoError(t, err)
		_, err = w.Write(entry.body)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
	return path
}

// writeTestZipWithDeclaredSize writes a stored entry whose header claims a size
// the payload does not have, the shape a decompression bomb relies on.
func writeTestZipWithDeclaredSize(t *testing.T, name string, body []byte, declared uint64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "release.zip")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	header := &zip.FileHeader{
		Name:               name,
		Method:             zip.Store,
		CRC32:              crc32.ChecksumIEEE(body),
		UncompressedSize64: declared,
		CompressedSize64:   uint64(len(body)),
	}
	w, err := zw.CreateRaw(header)
	require.NoError(t, err)
	_, err = w.Write(body)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
	return path
}

func writeTestTarGz(t *testing.T, entries []tar.Header, bodies [][]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "release.tar.gz")

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	for i, header := range entries {
		h := header
		if h.Size == 0 && len(bodies[i]) > 0 {
			h.Size = int64(len(bodies[i]))
		}
		require.NoError(t, tw.WriteHeader(&h))
		if len(bodies[i]) > 0 {
			_, err := tw.Write(bodies[i])
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
	return path
}

func TestExtractBinaryFromZipExtractsBinaryFromReleaseLayout(t *testing.T) {
	archive := writeTestZip(t,
		testZipEntry{name: "sub2api_0.3.0_windows_amd64/", mode: os.ModeDir | 0o755},
		testZipEntry{name: "sub2api_0.3.0_windows_amd64/LICENSE", body: []byte("license")},
		testZipEntry{name: "sub2api_0.3.0_windows_amd64/sub2api.exe", body: []byte("MZ-binary")},
	)
	dest := filepath.Join(t.TempDir(), "sub2api")

	require.NoError(t, extractBinaryFromArchive(archive, dest))

	body, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, "MZ-binary", string(body))
}

func TestExtractBinaryFromZipRejectsPathTraversal(t *testing.T) {
	for _, name := range []string{
		"../sub2api.exe",
		"sub2api_0.3.0_windows_amd64/../../sub2api.exe",
		`..\sub2api.exe`,
	} {
		t.Run(name, func(t *testing.T) {
			archive := writeTestZip(t, testZipEntry{name: name, body: []byte("evil")})
			dest := filepath.Join(t.TempDir(), "sub2api")

			err := extractBinaryFromArchive(archive, dest)

			require.Error(t, err)
			require.Contains(t, err.Error(), "path traversal")
			_, statErr := os.Stat(dest)
			require.True(t, os.IsNotExist(statErr), "nothing may be written for a traversing entry")
		})
	}
}

func TestExtractBinaryFromZipRejectsAbsoluteEntryPath(t *testing.T) {
	for _, name := range []string{"/sub2api.exe", "/tmp/sub2api.exe"} {
		t.Run(name, func(t *testing.T) {
			archive := writeTestZip(t, testZipEntry{name: name, body: []byte("evil")})
			dest := filepath.Join(t.TempDir(), "sub2api")

			err := extractBinaryFromArchive(archive, dest)

			require.Error(t, err)
			require.Contains(t, err.Error(), "absolute path")
		})
	}
}

func TestExtractBinaryFromZipRejectsWindowsStyleNamesOnAnyHost(t *testing.T) {
	tests := []struct {
		name    string
		entry   string
		wantErr string
	}{
		{name: "drive letter backslash", entry: `C:\Windows\sub2api.exe`, wantErr: "drive-qualified"},
		{name: "drive letter forward slash", entry: "C:/Windows/sub2api.exe", wantErr: "drive-qualified"},
		{name: "relative drive", entry: "C:sub2api.exe", wantErr: "drive-qualified"},
		{name: "NTFS alternate data stream", entry: "sub2api.exe:hidden", wantErr: "stream-qualified"},
		{name: "UNC with backslashes", entry: `\\server\share\sub2api.exe`, wantErr: "absolute path"},
		{name: "UNC with forward slashes", entry: "//server/share/sub2api.exe", wantErr: "absolute path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			archive := writeTestZip(t, testZipEntry{name: tt.entry, body: []byte("evil")})
			dest := filepath.Join(t.TempDir(), "sub2api")

			err := extractBinaryFromArchive(archive, dest)

			require.Error(t, err, "a Windows-style entry name must be refused wherever the updater runs")
			require.Contains(t, err.Error(), tt.wantErr)
			_, statErr := os.Stat(dest)
			require.True(t, os.IsNotExist(statErr), "nothing may be written for a refused entry")
		})
	}
}

func TestExtractBinaryFromZipRejectsEmptyBinaryEntry(t *testing.T) {
	archive := writeTestZip(t, testZipEntry{name: "sub2api_0.3.0_windows_amd64/sub2api.exe"})
	dest := filepath.Join(t.TempDir(), "sub2api")

	err := extractBinaryFromArchive(archive, dest)

	require.Error(t, err, "an empty payload must not be installed over the running executable")
	_, statErr := os.Stat(dest)
	require.True(t, os.IsNotExist(statErr))
}

func TestWriteBoundedRejectsEmptyAndShortEntries(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "sub2api")

		err := writeBounded(dest, bytes.NewReader(nil), 0)

		require.Error(t, err)
		require.Contains(t, err.Error(), "is empty")
		_, statErr := os.Stat(dest)
		require.True(t, os.IsNotExist(statErr))
	})

	t.Run("short of its declared size", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "sub2api")

		err := writeBounded(dest, bytes.NewReader([]byte("tiny")), 4096)

		require.Error(t, err)
		require.Contains(t, err.Error(), "declares 4096")
		_, statErr := os.Stat(dest)
		require.True(t, os.IsNotExist(statErr))
	})
}

func TestExtractBinaryFromZipNeverMaterialisesSymlinkEntry(t *testing.T) {
	archive := writeTestZip(t,
		testZipEntry{name: "sub2api.exe", mode: os.ModeSymlink | 0o777, body: []byte("../../etc/passwd")},
		testZipEntry{name: "sub2api_0.3.0_windows_amd64/LICENSE", body: []byte("license")},
	)
	dest := filepath.Join(t.TempDir(), "sub2api")

	err := extractBinaryFromArchive(archive, dest)

	require.Error(t, err)
	require.Contains(t, err.Error(), "binary not found")
	_, statErr := os.Stat(dest)
	require.True(t, os.IsNotExist(statErr), "a symlink entry must not become the executable")
}

func TestExtractBinaryFromZipIgnoresDirectoryNamedLikeBinary(t *testing.T) {
	archive := writeTestZip(t,
		testZipEntry{name: "sub2api.exe/", mode: os.ModeDir | 0o755},
	)
	dest := filepath.Join(t.TempDir(), "sub2api")

	err := extractBinaryFromArchive(archive, dest)

	require.Error(t, err)
	require.Contains(t, err.Error(), "binary not found")
}

func TestExtractBinaryFromZipBoundsExtractedSize(t *testing.T) {
	restore := maxExtractedBinarySize
	maxExtractedBinarySize = 64
	t.Cleanup(func() { maxExtractedBinarySize = restore })

	// A highly compressible payload far above the bound: a decompression bomb.
	archive := writeTestZip(t, testZipEntry{
		name: "sub2api_0.3.0_linux_amd64/sub2api",
		body: bytes.Repeat([]byte("a"), 4096),
	})
	dest := filepath.Join(t.TempDir(), "sub2api")

	err := extractBinaryFromArchive(archive, dest)

	require.Error(t, err)
	require.Contains(t, err.Error(), "binary too large")
	_, statErr := os.Stat(dest)
	require.True(t, os.IsNotExist(statErr), "an oversized extraction must not leave a partial binary behind")
}

func TestWriteBoundedRefusesMoreBytesThanDeclared(t *testing.T) {
	restore := maxExtractedBinarySize
	maxExtractedBinarySize = 1024
	t.Cleanup(func() { maxExtractedBinarySize = restore })

	// The reader lies about its size (a header cannot be trusted), so the bytes
	// actually produced must be bounded independently of the declared size.
	dest := filepath.Join(t.TempDir(), "sub2api")

	err := writeBounded(dest, bytes.NewReader(bytes.Repeat([]byte("a"), 4096)), 64)

	require.Error(t, err)
	require.Contains(t, err.Error(), "binary too large")
	_, statErr := os.Stat(dest)
	require.True(t, os.IsNotExist(statErr), "an oversized extraction must not leave a partial binary behind")
}

func TestExtractBinaryFromZipRejectsDeclaredOversizeEntry(t *testing.T) {
	archive := writeTestZipWithDeclaredSize(t, "sub2api.exe", []byte("small"), 1<<40)
	dest := filepath.Join(t.TempDir(), "sub2api")

	err := extractBinaryFromArchive(archive, dest)

	require.Error(t, err)
	require.Contains(t, err.Error(), "binary too large")
}

func TestExtractBinaryFromTarGzKeepsExistingBehaviour(t *testing.T) {
	entries := []tar.Header{
		{Name: "sub2api_0.3.0_linux_amd64/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "sub2api_0.3.0_linux_amd64/LICENSE", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "sub2api_0.3.0_linux_amd64/bin", Typeflag: tar.TypeSymlink, Linkname: "/bin/sh"},
		{Name: "sub2api_0.3.0_linux_amd64/sub2api", Typeflag: tar.TypeReg, Mode: 0o755},
	}
	bodies := [][]byte{nil, []byte("license"), nil, []byte("elf-binary")}
	archive := writeTestTarGz(t, entries, bodies)
	dest := filepath.Join(t.TempDir(), "sub2api")

	require.NoError(t, extractBinaryFromArchive(archive, dest))

	body, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, "elf-binary", string(body))
}

func TestExtractBinaryFromTarRejectsTraversalAndAbsolutePaths(t *testing.T) {
	tests := []struct {
		name    string
		entry   string
		wantErr string
	}{
		{name: "traversal", entry: "../sub2api", wantErr: "path traversal"},
		{name: "traversal inside directories", entry: "sub2api_0.3.0/../../sub2api", wantErr: "path traversal"},
		{name: "absolute", entry: "/sub2api", wantErr: "absolute path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries := []tar.Header{{Name: tt.entry, Typeflag: tar.TypeReg, Mode: 0o755}}
			archive := writeTestTarGz(t, entries, [][]byte{[]byte("evil")})
			dest := filepath.Join(t.TempDir(), "sub2api")

			err := extractBinaryFromArchive(archive, dest)

			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestExtractBinaryFromPlainFileUsesSizeBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub2api")
	require.NoError(t, os.WriteFile(path, []byte("plain-binary"), 0o644))
	dest := filepath.Join(t.TempDir(), "out")

	require.NoError(t, extractBinaryFromArchive(path, dest))
	body, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, "plain-binary", string(body))

	restore := maxExtractedBinarySize
	maxExtractedBinarySize = 4
	t.Cleanup(func() { maxExtractedBinarySize = restore })

	err = extractBinaryFromArchive(path, filepath.Join(t.TempDir(), "out2"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "binary too large")
}
