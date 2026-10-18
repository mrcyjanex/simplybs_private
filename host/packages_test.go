package host

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindPackagesDir(t *testing.T) {
	root := t.TempDir()
	native := filepath.Join(root, "packages", "native")
	if err := os.MkdirAll(native, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(native, "_.yaml"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	got := findPackagesDir(nested)
	want := filepath.Join(root, "packages")
	if got != want {
		t.Fatalf("findPackagesDir(%s) = %s, want %s", nested, got, want)
	}
	if findPackagesDir(t.TempDir()) != "" {
		t.Fatal("findPackagesDir on an empty tree returned a directory")
	}
}

func TestGetPackagesDirUsesCheckout(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repo := findPackagesDir(wd)
	if repo == "" {
		t.Fatal("test is not inside a simplybs checkout")
	}
	sub := filepath.Join(filepath.Dir(repo), "host")
	t.Chdir(sub)
	got := GetPackagesDir()
	if got != repo {
		t.Fatalf("GetPackagesDir() = %s, want %s", got, repo)
	}
}
