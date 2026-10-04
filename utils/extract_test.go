package utils

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateTarGzReproducible(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "bin", "tool"), []byte("#!/bin/sh\necho hi\n"), 0755)
	mustWriteFile(t, filepath.Join(src, "share", "data.txt"), []byte("payload\n"), 0640)
	mustWriteFile(t, filepath.Join(src, "share", "private.txt"), []byte("secret\n"), 0600)
	if err := os.MkdirAll(filepath.Join(src, "include"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../share/data.txt", filepath.Join(src, "include", "data.link")); err != nil {
		t.Fatal(err)
	}

	first := filepath.Join(t.TempDir(), "a.tar.gz")
	second := filepath.Join(t.TempDir(), "b.tar.gz")
	if err := CreateTarGz(src, first); err != nil {
		t.Fatal(err)
	}
	if err := CreateTarGz(src, second); err != nil {
		t.Fatal(err)
	}

	sum1 := fileSHA256(t, first)
	sum2 := fileSHA256(t, second)
	if sum1 != sum2 {
		t.Fatalf("rebuild checksums differ: %s vs %s", sum1, sum2)
	}

	assertReproducibleGzip(t, first)
	assertReproducibleTar(t, first)

	out := t.TempDir()
	if err := ExtractTarGz(first, out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, "share", "data.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "payload\n" {
		t.Fatalf("extracted data.txt: %q", data)
	}
	link, err := os.Readlink(filepath.Join(out, "include", "data.link"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.ToSlash(link) != "../share/data.txt" {
		t.Fatalf("symlink target: %q", link)
	}
}

func TestCreateTarGzIgnoresOwnerAndUmaskBits(t *testing.T) {
	srcA := t.TempDir()
	srcB := t.TempDir()
	mustWriteFile(t, filepath.Join(srcA, "lib", "foo.a"), []byte("obj"), 0664)
	mustWriteFile(t, filepath.Join(srcB, "lib", "foo.a"), []byte("obj"), 0644)

	outA := filepath.Join(t.TempDir(), "a.tar.gz")
	outB := filepath.Join(t.TempDir(), "b.tar.gz")
	if err := CreateTarGz(srcA, outA); err != nil {
		t.Fatal(err)
	}
	if err := CreateTarGz(srcB, outB); err != nil {
		t.Fatal(err)
	}
	if fileSHA256(t, outA) != fileSHA256(t, outB) {
		t.Fatal("group-writable vs world-readable file produced different archives")
	}
}

func assertReproducibleGzip(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if !zr.ModTime.IsZero() {
		t.Fatalf("gzip mtime: got %v, want zero", zr.ModTime)
	}
	if zr.Name != "" {
		t.Fatalf("gzip name: got %q, want empty", zr.Name)
	}
	if zr.OS != 255 {
		t.Fatalf("gzip OS: got %d, want 255", zr.OS)
	}
}

func assertReproducibleTar(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen[hdr.Name] = true
		if hdr.Uid != 0 || hdr.Gid != 0 {
			t.Fatalf("%s: uid/gid = %d/%d, want 0/0", hdr.Name, hdr.Uid, hdr.Gid)
		}
		if hdr.Uname != "" || hdr.Gname != "" {
			t.Fatalf("%s: uname/gname = %q/%q, want empty", hdr.Name, hdr.Uname, hdr.Gname)
		}
		if !hdr.ModTime.Equal(tarArchiveEpoch) {
			t.Fatalf("%s: mtime = %v, want %v", hdr.Name, hdr.ModTime, tarArchiveEpoch)
		}
		if !hdr.AccessTime.IsZero() || !hdr.ChangeTime.IsZero() {
			t.Fatalf("%s: atime/ctime should be unset, got %v / %v", hdr.Name, hdr.AccessTime, hdr.ChangeTime)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if hdr.Mode&0777 != 0755 {
				t.Fatalf("%s: dir mode %#o, want 0755", hdr.Name, hdr.Mode)
			}
		case tar.TypeReg:
			perm := hdr.Mode & 0777
			if perm != 0644 && perm != 0755 {
				t.Fatalf("%s: file mode %#o, want 0644 or 0755", hdr.Name, hdr.Mode)
			}
		}
	}
	if !seen["share/private.txt"] || !seen["bin/tool"] || !seen["include/"] {
		t.Fatalf("missing expected entries: %v", seen)
	}
}

func mustWriteFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}
