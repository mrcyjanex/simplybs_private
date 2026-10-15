package pack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrcyjanek/simplybs/host"
)

func TestMain(m *testing.M) {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "packages", "native", "_.json")); err == nil {
			break
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			panic("could not find repo root")
		}
		wd = parent
	}
	if err := os.Chdir(wd); err != nil {
		panic(err)
	}
	home, err := os.MkdirTemp("", "simplybs-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Unsetenv("SIMPLYBS_TOKEN")
	os.Unsetenv("SIMPLYBS_CACHE_TAG")
	os.Exit(m.Run())
}

func chdirRepoRoot(t *testing.T) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "packages", "native", "_.json")); err == nil {
			if err := os.Chdir(wd); err != nil {
				t.Fatal(err)
			}
			return
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			t.Fatal("could not find repo root (packages/native/_.json)")
		}
		wd = parent
	}
}

func TestAssetNameRoundTrip(t *testing.T) {
	cases := []string{
		"zlib-1.3.1-deadbeef.tar.gz",
		"x86_64-linux-gnu/zlib-1.3.1-deadbeef.tar.gz",
		"x86_64-linux-gnu/native_foo/bar-1.0-abcd1234.info.txt",
	}
	for _, rel := range cases {
		name := AssetNameForRel(rel)
		got := RelFromAssetName(name)
		if filepath.ToSlash(got) != filepath.ToSlash(rel) {
			t.Fatalf("round-trip %q -> %q -> %q", rel, name, got)
		}
		if rel != filepath.Base(rel) && !containsPlus(name) {
			t.Fatalf("expected '+' in asset name for nested path, got %q", name)
		}
	}
}

func containsPlus(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '+' {
			return true
		}
	}
	return false
}

func TestCacheEnabledRequiresToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SIMPLYBS_TOKEN", "")
	t.Setenv("SIMPLYBS_CACHE_TAG", "")
	if CacheEnabled() {
		t.Fatal("expected cache disabled without a token")
	}
	t.Setenv("SIMPLYBS_CACHE_TAG", "v0-sbs-test")
	if CacheEnabled() {
		t.Fatal("expected cache disabled with only a tag")
	}
	t.Setenv("SIMPLYBS_TOKEN", "sbs_test")
	if !CacheEnabled() {
		t.Fatal("expected cache enabled when a token is set")
	}
	if CacheTag() != "v0-sbs-test" {
		t.Fatalf("CacheTag = %q", CacheTag())
	}
}

func TestPackageCacheOnReleaseDisabled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SIMPLYBS_TOKEN", "")
	t.Setenv("SIMPLYBS_CACHE_TAG", "")
	ok, err := PackageCacheOnRelease(&Package{Package: "zlib", Version: "1.3.1"}, host.SupportedHosts["x86_64-linux-gnu"])
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected miss when cache is disabled")
	}
}

func TestCacheTagFallsBackWhenUnset(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SIMPLYBS_TOKEN", "")
	t.Setenv("SIMPLYBS_CACHE_TAG", "")
	t.Setenv("USER", "tester")
	tag := CacheTag()
	if !strings.HasPrefix(tag, "v0-sbs-tester-") {
		t.Fatalf("CacheTag = %q", tag)
	}
}

func TestBuiltRelPathsNativeAndHost(t *testing.T) {
	chdirRepoRoot(t)
	t.Setenv("HOME", t.TempDir())
	pkg, err := FindPackage("zlib")
	if err != nil {
		t.Fatal(err)
	}
	h := host.SupportedHosts["x86_64-linux-gnu"]
	rels := pkg.BuiltRelPaths(h)
	if len(rels) != 3 {
		t.Fatalf("expected 3 paths, got %v", rels)
	}
	for _, rel := range rels {
		if filepath.ToSlash(rel) != rel {
			t.Fatalf("expected slash paths, got %q", rel)
		}
		if rel == filepath.Base(rel) {
			t.Fatalf("host package should be under triplet dir, got %q", rel)
		}
		name := AssetNameForRel(rel)
		if RelFromAssetName(name) != rel {
			t.Fatalf("asset encoding broken for %q", rel)
		}
	}

	native, err := FindPackage("native/zlib")
	if err != nil {
		t.Fatal(err)
	}
	nrels := native.BuiltRelPaths(h)
	for _, rel := range nrels {
		if rel != filepath.Base(rel) {
			t.Fatalf("native package should be flat under built/, got %q", rel)
		}
	}
}

func TestNeededAssetNamesIsSelective(t *testing.T) {
	chdirRepoRoot(t)
	t.Setenv("HOME", t.TempDir())
	pkg, err := FindPackage("zlib")
	if err != nil {
		t.Fatal(err)
	}
	h := host.SupportedHosts["x86_64-linux-gnu"]
	names := neededAssetNames([]*Package{pkg}, h)
	if len(names) < 3 {
		t.Fatalf("expected at least root package assets, got %d", len(names))
	}
	// Push still considers the full transitive tree, but remain far below a full-world dump.
	all := GetAllPackages()
	world := neededAssetNames(all, h)
	if len(names) >= len(world) {
		t.Fatalf("zlib tree (%d) should be smaller than world (%d)", len(names), len(world))
	}
	if len(names)%3 != 0 {
		t.Fatalf("asset count should be multiple of 3, got %d", len(names))
	}
}

func packageNameSet(pkgs []*Package) map[string]bool {
	found := map[string]bool{}
	for _, p := range pkgs {
		found[p.Package] = true
	}
	return found
}

func remoteAssetsFor(pkgs []*Package, h *host.Host, include func(*Package) bool) map[string]bool {
	remote := map[string]bool{}
	for _, p := range pkgs {
		if include != nil && !include(p) {
			continue
		}
		for _, rel := range p.BuiltRelPaths(h) {
			remote[AssetNameForRel(rel)] = true
		}
	}
	return remote
}

func TestCollectCachePullPackagesStopsAtCachedRoot(t *testing.T) {
	chdirRepoRoot(t)
	t.Setenv("HOME", t.TempDir())
	pkg, err := FindPackage("zlib")
	if err != nil {
		t.Fatal(err)
	}
	h := host.SupportedHosts["x86_64-linux-gnu"]
	full := CollectNeededPackages([]*Package{pkg}, h)
	if len(full) < 3 {
		t.Fatalf("expected zlib to have transitive deps, got %d", len(full))
	}
	remote := remoteAssetsFor(full, h, nil)
	got := collectCachePullPackages([]*Package{pkg}, h, remote)
	found := packageNameSet(got)
	if len(got) != 1 || !found["zlib"] {
		t.Fatalf("cached root should pull only itself, got %v", found)
	}
}

func TestCollectCachePullPackagesStopsAtCachedDirectDeps(t *testing.T) {
	chdirRepoRoot(t)
	t.Setenv("HOME", t.TempDir())
	pkg, err := FindPackage("native/rust@1_95_0")
	if err != nil {
		t.Fatal(err)
	}
	h := host.SupportedHosts["x86_64-linux-gnu"]
	full := CollectNeededPackages([]*Package{pkg}, h)
	foundFull := packageNameSet(full)
	if !foundFull["native/rust@1_94_0"] || !foundFull["native/rust@1_93_0"] {
		t.Fatalf("expected rust bootstrap chain in full tree, got %v", foundFull)
	}

	direct := packageNameSet(filteredDependencyPackages(pkg.Dependencies, h))
	if direct["native/rust@1_93_0"] {
		t.Fatal("native/rust@1_93_0 should not be a direct dep of rust@1_95_0")
	}

	// Everything except the root is on the release: pull the root + direct
	// deps (including rust@1_94_0), but not older bootstraps.
	remote := remoteAssetsFor(full, h, func(p *Package) bool {
		return p.Package != pkg.Package
	})
	got := collectCachePullPackages([]*Package{pkg}, h, remote)
	found := packageNameSet(got)
	t.Logf("native/rust@1_95_0: full tree=%d packages, pull with cached deps=%d", len(full), len(got))
	if !found[pkg.Package] {
		t.Fatal("missing root package")
	}
	if !found["native/rust@1_94_0"] {
		t.Fatal("expected direct dep native/rust@1_94_0")
	}
	if found["native/rust@1_93_0"] || found["native/rust@1_92_0"] || found["native/rust@1_91_0"] {
		t.Fatalf("pulled rust bootstraps past a cache hit: %v", found)
	}
	for name := range direct {
		if !found[name] {
			t.Fatalf("missing direct dep %s", name)
		}
	}
}

func TestCollectCachePullPackagesWalksPastCacheMiss(t *testing.T) {
	chdirRepoRoot(t)
	t.Setenv("HOME", t.TempDir())
	pkg, err := FindPackage("native/rust@1_95_0")
	if err != nil {
		t.Fatal(err)
	}
	h := host.SupportedHosts["x86_64-linux-gnu"]
	full := CollectNeededPackages([]*Package{pkg}, h)
	// rust@1_95_0 and rust@1_94_0 are both misses, so walk to rust@1_93_0.
	// rust@1_93_0 is cached, so rust@1_92_0 is not needed.
	skip := map[string]bool{
		"native/rust@1_95_0": true,
		"native/rust@1_94_0": true,
	}
	remote := remoteAssetsFor(full, h, func(p *Package) bool {
		return !skip[p.Package]
	})
	got := collectCachePullPackages([]*Package{pkg}, h, remote)
	found := packageNameSet(got)
	t.Logf("native/rust@1_95_0: full tree=%d, pull walking past rust@1_94_0 miss=%d", len(full), len(got))
	for _, name := range []string{"native/rust@1_95_0", "native/rust@1_94_0", "native/rust@1_93_0"} {
		if !found[name] {
			t.Fatalf("expected %s when walking past misses, got %v", name, found)
		}
	}
	if found["native/rust@1_92_0"] || found["native/rust@1_91_0"] {
		t.Fatalf("pulled rust bootstraps past rust@1_93_0 cache hit: %v", found)
	}
}

func TestCollectCachePullPackagesEmptyRemoteIsFullTree(t *testing.T) {
	chdirRepoRoot(t)
	t.Setenv("HOME", t.TempDir())
	pkg, err := FindPackage("zlib")
	if err != nil {
		t.Fatal(err)
	}
	h := host.SupportedHosts["x86_64-linux-gnu"]
	full := packageNameSet(CollectNeededPackages([]*Package{pkg}, h))
	got := packageNameSet(collectCachePullPackages([]*Package{pkg}, h, map[string]bool{}))
	if len(got) != len(full) {
		t.Fatalf("empty remote should walk the full tree: pull=%d full=%d", len(got), len(full))
	}
	for name := range full {
		if !got[name] {
			t.Fatalf("missing %s from uncached pull walk", name)
		}
	}
}

func TestCollectCachePullPackagesStopsAtLocalComplete(t *testing.T) {
	chdirRepoRoot(t)
	t.Setenv("HOME", t.TempDir())
	pkg, err := FindPackage("zlib")
	if err != nil {
		t.Fatal(err)
	}
	h := host.SupportedHosts["x86_64-linux-gnu"]
	builtRoot := host.BuiltDir()
	for _, rel := range pkg.BuiltRelPaths(h) {
		path := filepath.Join(builtRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("local"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	got := collectCachePullPackages([]*Package{pkg}, h, map[string]bool{})
	found := packageNameSet(got)
	if len(got) != 1 || !found["zlib"] {
		t.Fatalf("local complete root should not walk deps, got %v", found)
	}
}
