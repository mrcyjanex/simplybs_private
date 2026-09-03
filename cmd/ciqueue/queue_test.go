package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrcyjanek/simplybs/host"
	"github.com/mrcyjanek/simplybs/pack"
)

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
			t.Fatal("could not find repo root")
		}
		wd = parent
	}
}

func missCache(t *testing.T) cacheFn {
	t.Helper()
	return func(*pack.Package, *host.Host) (bool, error) { return false, nil }
}

func TestPackagesFromPaths(t *testing.T) {
	chdirRepoRoot(t)
	names := []string{}
	for _, p := range pack.GetAllPackages() {
		names = append(names, p.Package)
	}
	got := packagesFromPaths([]string{
		"packages/zlib.json",
		"patches/zlib/does-not-need-to-exist.patch",
		"patches/native/make/foo.patch",
		"README.md",
		"cmd/ciqueue/queue.go",
	}, names)
	for _, want := range []string{"zlib", "native/make"} {
		if !got[want] {
			t.Fatalf("expected %s in %v", want, sortedKeys(got))
		}
	}
	if got["cmd/ciqueue"] {
		t.Fatal("Go source should not map to a package")
	}
}

func TestNextQueueEmptyDiffIsDone(t *testing.T) {
	chdirRepoRoot(t)
	res, err := nextQueue(queueOpts{
		changedFiles: []string{"README.md", ".github/workflows/ci.yml"},
		hosts:        DefaultHosts,
		cached:       missCache(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "done" {
		t.Fatalf("status=%s pkg=%s", res.Status, res.Package)
	}
}

func TestNextQueueZlibPicksNativeFirst(t *testing.T) {
	chdirRepoRoot(t)
	res, err := nextQueue(queueOpts{
		changedFiles: []string{"packages/zlib.json"},
		hosts:        DefaultHosts,
		cached:       missCache(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "next" {
		t.Fatalf("status=%s msg=%s", res.Status, res.Message)
	}
	if res.Package == "zlib" {
		t.Fatal("expected a native dependency before zlib")
	}
	pkg, err := pack.FindPackage(res.Package)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Type != "native" {
		t.Fatalf("expected native seed, got %s type=%s", res.Package, pkg.Type)
	}
	if res.Needed < 2 {
		t.Fatalf("expected zlib plus deps, needed=%d", res.Needed)
	}
}

func TestNextQueueZlibWhenDepsCached(t *testing.T) {
	chdirRepoRoot(t)
	res, err := nextQueue(queueOpts{
		changedFiles: []string{"packages/zlib.json"},
		hosts:        DefaultHosts,
		cached: func(p *pack.Package, h *host.Host) (bool, error) {
			return p.Package != "zlib", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "next" || res.Package != "zlib" {
		t.Fatalf("got %+v", res)
	}
	if res.Host != "x86_64-linux-gnu" {
		t.Fatalf("expected linux-gnu first, got %s", res.Host)
	}
}

func TestNextQueueZlibSecondHostAfterFirstCached(t *testing.T) {
	chdirRepoRoot(t)
	res, err := nextQueue(queueOpts{
		changedFiles: []string{"packages/zlib.json"},
		hosts:        DefaultHosts,
		cached: func(p *pack.Package, h *host.Host) (bool, error) {
			if p.Package != "zlib" {
				return true, nil
			}
			return h.Triplet == "x86_64-linux-gnu", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "next" || res.Package != "zlib" || res.Host != "aarch64-linux-android" {
		t.Fatalf("got %+v", res)
	}
}

func TestNextQueueZlibDarwinHost(t *testing.T) {
	chdirRepoRoot(t)
	res, err := nextQueue(queueOpts{
		changedFiles: []string{"packages/zlib.json"},
		hosts:        []string{"aarch64-apple-darwin"},
		cached: func(p *pack.Package, h *host.Host) (bool, error) {
			return p.Package != "zlib", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "next" || res.Package != "zlib" || res.Host != "aarch64-apple-darwin" {
		t.Fatalf("got %+v", res)
	}
}

func TestRenderCommentContainsMarker(t *testing.T) {
	body := renderComment(CommentState{
		SHA: "abc",
		Runs: []CommentRun{{
			Package:    "zlib",
			Host:       "x86_64-linux-gnu",
			Conclusion: "success",
			RunURL:     "https://example.test/run/1",
		}},
		Remaining: []Item{{Package: "curl", Host: "x86_64-linux-gnu"}},
	}, "linux-amd64")
	st, ok := parseState(body, stateMarker("linux-amd64"))
	if !ok {
		t.Fatalf("parse failed:\n%s", body)
	}
	if st.SHA != "abc" || len(st.Runs) != 1 || st.Runs[0].Package != "zlib" {
		t.Fatalf("%+v", st)
	}
	if len(st.Remaining) != 1 || st.Remaining[0].Package != "curl" {
		t.Fatalf("remaining %+v", st.Remaining)
	}
}

func TestRenderCommentQueueMarkersDoNotCollide(t *testing.T) {
	linux := renderComment(CommentState{SHA: "abc"}, "linux-amd64")
	macos := renderComment(CommentState{SHA: "abc"}, "macos")
	arm64 := renderComment(CommentState{SHA: "abc"}, "linux-arm64")
	if !strings.Contains(linux, stateMarker("linux-amd64")) {
		t.Fatalf("linux-amd64 comment missing marker:\n%s", linux)
	}
	if !strings.Contains(linux, "## simplybs package queue (linux-amd64)") {
		t.Fatalf("linux-amd64 title missing:\n%s", linux)
	}
	if !strings.Contains(macos, stateMarker("macos")) {
		t.Fatalf("macos comment missing marker:\n%s", macos)
	}
	if !strings.Contains(macos, "## simplybs package queue (macos)") {
		t.Fatalf("macos title missing:\n%s", macos)
	}
	if !strings.Contains(arm64, stateMarker("linux-arm64")) {
		t.Fatalf("linux-arm64 comment missing marker:\n%s", arm64)
	}
	if !strings.Contains(arm64, "## simplybs package queue (linux-arm64)") {
		t.Fatalf("linux-arm64 title missing:\n%s", arm64)
	}
	if _, ok := parseState(linux, stateMarker("linux-amd64")); !ok {
		t.Fatalf("linux-amd64 parser missed linux-amd64 comment:\n%s", linux)
	}
	if _, ok := parseState(macos, stateMarker("macos")); !ok {
		t.Fatalf("macos parser missed macos comment:\n%s", macos)
	}
	if _, ok := parseState(arm64, stateMarker("linux-arm64")); !ok {
		t.Fatalf("linux-arm64 parser missed linux-arm64 comment:\n%s", arm64)
	}
	if _, ok := parseState(linux, stateMarker("macos")); ok {
		t.Fatal("macos parser matched linux-amd64 comment")
	}
	if _, ok := parseState(linux, stateMarker("linux-arm64")); ok {
		t.Fatal("linux-arm64 parser matched linux-amd64 comment")
	}
	if _, ok := parseState(macos, stateMarker("linux-amd64")); ok {
		t.Fatal("linux-amd64 parser matched macos comment")
	}
	if _, ok := parseState(arm64, stateMarker("linux-amd64")); ok {
		t.Fatal("linux-amd64 parser matched linux-arm64 comment")
	}
	if _, ok := parseState(macos, stateMarker("linux-arm64")); ok {
		t.Fatal("linux-arm64 parser matched macos comment")
	}
	if _, ok := parseState(arm64, stateMarker("macos")); ok {
		t.Fatal("macos parser matched linux-arm64 comment")
	}
	if _, ok := parseState(linux, commentMarker); ok {
		t.Fatal("legacy unlabeled parser matched linux-amd64 comment")
	}
}

func TestNextQueueZlibLinuxArm64HostsSkipAndroid(t *testing.T) {
	chdirRepoRoot(t)
	hosts := []string{
		"aarch64-apple-darwin",
		"x86_64-apple-darwin",
		"aarch64-apple-ios",
		"aarch64-apple-ios-simulator",
		"x86_64-w64-mingw32",
		"x86_64-linux-gnu",
		"aarch64-linux-gnu",
	}
	res, err := nextQueue(queueOpts{
		changedFiles: []string{"packages/zlib.json"},
		hosts:        hosts,
		cached: func(p *pack.Package, h *host.Host) (bool, error) {
			return p.Package != "zlib", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "next" || res.Package != "zlib" {
		t.Fatalf("got %+v", res)
	}
	if res.Host != hosts[0] {
		t.Fatalf("expected first non-android host %s, got %s", hosts[0], res.Host)
	}
	for _, it := range res.Remaining {
		if strings.Contains(it.Host, "android") {
			t.Fatalf("android host leaked into remaining: %+v", it)
		}
	}
	if strings.Contains(res.Host, "android") {
		t.Fatalf("android host selected: %s", res.Host)
	}
}

func TestNextQueueZlibDarwinHostsSkipLinuxGnuAndMingw(t *testing.T) {
	chdirRepoRoot(t)
	hosts := []string{
		"aarch64-apple-darwin",
		"x86_64-apple-darwin",
		"aarch64-apple-ios",
		"aarch64-apple-ios-simulator",
		"aarch64-linux-android",
		"x86_64-linux-android",
		"armv7a-linux-androideabi",
	}
	res, err := nextQueue(queueOpts{
		changedFiles: []string{"packages/zlib.json"},
		hosts:        hosts,
		cached: func(p *pack.Package, h *host.Host) (bool, error) {
			return p.Package != "zlib", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "next" || res.Package != "zlib" {
		t.Fatalf("got %+v", res)
	}
	if res.Host != hosts[0] {
		t.Fatalf("expected first darwin host %s, got %s", hosts[0], res.Host)
	}
	for _, it := range append([]Item{{Package: res.Package, Host: res.Host}}, res.Remaining...) {
		if strings.Contains(it.Host, "linux-gnu") || strings.Contains(it.Host, "mingw") {
			t.Fatalf("linux-gnu/mingw host leaked: %+v", it)
		}
	}
}
