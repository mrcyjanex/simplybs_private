package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if res.Status != "next" || res.Package != "zlib" || res.Host != "aarch64-linux-gnu" {
		t.Fatalf("got %+v", res)
	}
}

func TestDefaultHostsAreLinuxCIHosts(t *testing.T) {
	skip := map[string]bool{"armv7a-linux-androideabi": true}
	if DefaultHosts[0] != "x86_64-linux-gnu" {
		t.Fatalf("linux-gnu should stay first, got %s", DefaultHosts[0])
	}
	seen := map[string]bool{}
	for i, h := range DefaultHosts {
		if skip[h] {
			t.Fatalf("DefaultHosts[%d]=%q should not be in Linux CI yet", i, h)
		}
		if host.SupportedHosts[h] == nil {
			t.Fatalf("DefaultHosts[%d]=%q is not in SupportedHosts", i, h)
		}
		if seen[h] {
			t.Fatalf("duplicate host %q", h)
		}
		seen[h] = true
	}
	for triplet := range host.SupportedHosts {
		if skip[triplet] {
			continue
		}
		if !seen[triplet] {
			t.Fatalf("SupportedHosts %q missing from DefaultHosts", triplet)
		}
	}
}

func TestPackagesYmlHostsMatchDefaultHosts(t *testing.T) {
	chdirRepoRoot(t)
	b, err := os.ReadFile(filepath.Join(".github", "workflows", "packages.yml"))
	if err != nil {
		t.Fatal(err)
	}
	want := "hosts: " + strings.Join(DefaultHosts, ",")
	if !bytes.Contains(b, []byte(want)) {
		t.Fatalf("packages.yml missing %q", want)
	}
	if n := bytes.Count(b, []byte(want)); n != 10 {
		t.Fatalf("expected 10 batch host lists, found %d", n)
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

func TestNextQueueDoesNotRecheckSharedDeps(t *testing.T) {
	chdirRepoRoot(t)
	type key struct{ pkg, host string }
	calls := map[key]int{}
	_, err := nextQueue(queueOpts{
		changedFiles: []string{"packages/zlib.json", "packages/curl.json"},
		hosts:        []string{"x86_64-linux-gnu", "aarch64-linux-gnu"},
		cached: func(p *pack.Package, h *host.Host) (bool, error) {
			calls[key{p.Package, h.Triplet}]++
			return false, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for k, n := range calls {
		if n != 1 {
			t.Fatalf("cache lookup %s %s ran %d times", k.pkg, k.host, n)
		}
	}
}

func TestNextQueueDedupesSharedNativeArtifacts(t *testing.T) {
	chdirRepoRoot(t)
	m4, err := pack.FindPackage("native/m4")
	if err != nil {
		t.Fatal(err)
	}
	a := strings.Join(m4.BuiltRelPaths(host.SupportedHosts["x86_64-linux-gnu"]), "|")
	b := strings.Join(m4.BuiltRelPaths(host.SupportedHosts["aarch64-linux-gnu"]), "|")
	if a != b {
		t.Skipf("native/m4 artifacts differ across linux hosts:\n%s\n%s", a, b)
	}
	res, err := nextQueue(queueOpts{
		changedFiles: []string{"packages/native/m4.json"},
		hosts:        []string{"x86_64-linux-gnu", "aarch64-linux-gnu"},
		cached:       missCache(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	if res.Package == "native/m4" {
		seen++
	}
	for _, it := range res.Remaining {
		if it.Package == "native/m4" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("native/m4 queued %d times (pick=%s/%s needed=%d)", seen, res.Package, res.Host, res.Needed)
	}
}

func TestNextQueueMavenTreeFinishesQuickly(t *testing.T) {
	chdirRepoRoot(t)
	var files []string
	for _, p := range pack.GetAllPackages() {
		if strings.HasPrefix(p.Package, "native/jdk") || p.Package == "native/graalvm" || p.Package == "hellostaticlib" || p.Package == "graalvm-clibraries" {
			files = append(files, "packages/"+p.Package+".json")
		}
	}
	if len(files) < 10 {
		t.Skip("no jdk/graal packages in this checkout")
	}
	start := time.Now()
	res, err := nextQueue(queueOpts{
		changedFiles: files,
		hosts:        DefaultHosts,
		cached:       missCache(t),
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 45*time.Second {
		t.Fatalf("nextQueue took %s (status=%s needed=%d files=%d); Linux CI looked hung at ~26min for this tree", elapsed, res.Status, res.Needed, len(files))
	}
	t.Logf("nextQueue %s needed=%d pick=%s/%s files=%d", elapsed, res.Needed, res.Package, res.Host, len(files))
}

func TestRenderCommentHugeRemainingStaysUnderGitHubLimit(t *testing.T) {
	rem := make([]Item, 5000)
	for i := range rem {
		rem[i] = Item{
			Package: "native/jdk@22/bootstrap-maven/pom/error_prone_parent@2.47.0",
			Host:    "aarch64-apple-ios-simulator",
		}
	}
	body := renderComment(CommentState{SHA: "abc", Remaining: rem, RemainingCount: len(rem)}, "")
	if len(body) > 65536 {
		t.Fatalf("comment body %d bytes exceeds GitHub 64KiB limit", len(body))
	}
	st, ok := parseState(body, commentMarker)
	if !ok {
		t.Fatal("parse failed")
	}
	if st.RemainingCount != 5000 {
		t.Fatalf("remaining_count=%d", st.RemainingCount)
	}
	if len(st.Remaining) > remainingPreviewLimit {
		t.Fatalf("stored remaining %d", len(st.Remaining))
	}
	if !strings.Contains(body, "5000 still queued") {
		t.Fatalf("missing count:\n%s", body)
	}
}

func TestOutputResultTruncatesRemaining(t *testing.T) {
	res := Result{Remaining: make([]Item, 100)}
	out := outputResult(res)
	if out.RemainingCount != 100 {
		t.Fatalf("count=%d", out.RemainingCount)
	}
	if len(out.Remaining) != remainingPreviewLimit {
		t.Fatalf("preview=%d", len(out.Remaining))
	}
}
