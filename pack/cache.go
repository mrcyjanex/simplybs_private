package pack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mrcyjanek/simplybs/host"
)

// Remote build-cache assets live on GitHub Releases. Paths under
// DataDir()/built are flattened with '+' so release asset names stay flat:
//
//	x86_64-linux-gnu/zlib-1.3.1-deadbeef.tar.gz
//	  -> x86_64-linux-gnu+zlib-1.3.1-deadbeef.tar.gz
//
// GitHub caps each Release at 1000 assets. SIMPLYBS_CACHE_TAG is the base
// tag (e.g. v0-sbs-ci-linux-amd64); when it fills, uploads overflow to
// $TAG.s1, $TAG.s2, … automatically. Lookups union every shard, so a
// package triple can complete across releases. Callers still set only
// SIMPLYBS_CACHE_TAG and SIMPLYBS_CACHE_REPO.
//
// Pull requests artifacts for packages that will actually be extracted or
// rebuilt. Walking stops at a package whose current-hash files are already
// local or fully present on the cache: BuildPackage only extracts direct
// dependencies, so a cache hit (e.g. rust@1_96_0) does not download that
// package's own bootstrap chain. Push only uploads local files that are
// missing from the cache (content changes produce a new short-hash name).
//
// Cache is enabled only when both SIMPLYBS_CACHE_TAG and SIMPLYBS_CACHE_REPO
// are set; EnsureBuilt then auto-pulls missing artifacts.

const (
	cacheAssetSuffixes = 3 // .info.txt, .tar.gz, _native.tar.gz
	maxCacheShards     = 256
)

// GitHub Release asset downloads flake with HTTP 500. Retry a few times
// before treating the file as missing (EnsureBuilt then rebuilds).
var (
	cacheDownloadRetries = 5
	cacheDownloadBackoff = time.Second
)

// githubReleaseAssetLimit is GitHub's hard cap on assets per Release.
// Tests lower this to exercise overflow without creating 1000 dummy files.
var githubReleaseAssetLimit = 1000

var (
	remoteAssetsOnce sync.Once
	remoteMu         sync.Mutex
	remoteIndex      *remoteAssetIndex
	remoteAssetsErr  error
)

type remoteAssetIndex struct {
	// assets maps asset name -> shard tag that holds it.
	assets map[string]string
	shards []string
	counts map[string]int
}

func (idx *remoteAssetIndex) has(name string) bool {
	if idx == nil {
		return false
	}
	remoteMu.Lock()
	defer remoteMu.Unlock()
	_, ok := idx.assets[name]
	return ok
}

func (idx *remoteAssetIndex) tagFor(name string) (string, bool) {
	if idx == nil {
		return "", false
	}
	remoteMu.Lock()
	defer remoteMu.Unlock()
	tag, ok := idx.assets[name]
	return tag, ok
}

func (idx *remoteAssetIndex) nameSet() map[string]bool {
	if idx == nil {
		return map[string]bool{}
	}
	remoteMu.Lock()
	defer remoteMu.Unlock()
	out := make(map[string]bool, len(idx.assets))
	for n := range idx.assets {
		out[n] = true
	}
	return out
}

func (idx *remoteAssetIndex) stats() (assets, shards int) {
	if idx == nil {
		return 0, 0
	}
	remoteMu.Lock()
	defer remoteMu.Unlock()
	return len(idx.assets), len(idx.shards)
}

func (idx *remoteAssetIndex) addShard() string {
	base := CacheTag()
	next := 0
	if len(idx.shards) > 0 {
		if i, ok := parseCacheShardIndex(base, idx.shards[len(idx.shards)-1]); ok {
			next = i + 1
		} else {
			next = len(idx.shards)
		}
	}
	tag := cacheShardTag(base, next)
	idx.shards = append(idx.shards, tag)
	if idx.counts == nil {
		idx.counts = map[string]int{}
	}
	idx.counts[tag] = 0
	return tag
}

// pickShardCapacity chooses a shard with room for want files (or as many as
// fit). Small batches (a package triple) stay together on one shard.
func (idx *remoteAssetIndex) pickShardCapacity(want int) (tag string, n int) {
	if want < 1 {
		return "", 0
	}
	take := want
	if take > githubReleaseAssetLimit {
		take = githubReleaseAssetLimit
	}
	for _, t := range idx.shards {
		room := githubReleaseAssetLimit - idx.counts[t]
		if room >= take {
			return t, take
		}
	}
	if take <= cacheAssetSuffixes {
		return idx.addShard(), take
	}
	for _, t := range idx.shards {
		room := githubReleaseAssetLimit - idx.counts[t]
		if room > 0 {
			if room > take {
				room = take
			}
			return t, room
		}
	}
	return idx.addShard(), take
}

// cacheShardTag returns the base cache tag (n==0) or "$base.sN".
func cacheShardTag(base string, n int) string {
	if n <= 0 {
		return base
	}
	return fmt.Sprintf("%s.s%d", base, n)
}

func parseCacheShardIndex(base, tag string) (int, bool) {
	if tag == base {
		return 0, true
	}
	rest, ok := strings.CutPrefix(tag, base+".s")
	if !ok || rest == "" {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// CacheTag returns SIMPLYBS_CACHE_TAG (empty if unset).
func CacheTag() string {
	return os.Getenv("SIMPLYBS_CACHE_TAG")
}

// CacheRepo returns SIMPLYBS_CACHE_REPO (owner/repo; empty if unset).
func CacheRepo() string {
	return os.Getenv("SIMPLYBS_CACHE_REPO")
}

// CacheEnabled reports whether remote build cache is configured.
// Both SIMPLYBS_CACHE_TAG and SIMPLYBS_CACHE_REPO must be set.
func CacheEnabled() bool {
	return CacheTag() != "" && CacheRepo() != ""
}

func requireCacheConfig() (tag, repo string, err error) {
	tag = CacheTag()
	repo = CacheRepo()
	if tag == "" || repo == "" {
		return "", "", fmt.Errorf("cache: set both SIMPLYBS_CACHE_TAG and SIMPLYBS_CACHE_REPO")
	}
	return tag, repo, nil
}

func ghCmd(args ...string) *exec.Cmd {
	bin := os.Getenv("SIMPLYBS_GH")
	if bin == "" {
		bin = "gh"
	}
	repo := CacheRepo()
	if repo == "" {
		return exec.Command(bin, args...)
	}
	return exec.Command(bin, append([]string{"-R", repo}, args...)...)
}

// AssetNameForRel converts a path relative to DataDir()/built into a flat
// release asset name.
func AssetNameForRel(rel string) string {
	rel = filepath.ToSlash(rel)
	if strings.Contains(rel, "/") {
		return strings.ReplaceAll(rel, "/", "+")
	}
	return rel
}

// RelFromAssetName reverses AssetNameForRel.
func RelFromAssetName(name string) string {
	return strings.ReplaceAll(name, "+", "/")
}

// BuiltRelPaths returns the paths (relative to DataDir()/built) for a
// package's cache artifacts on host h.
func (p *Package) BuiltRelPaths(h *host.Host) []string {
	base := p.GenerateBuildPath(h, "built")
	builtRoot := filepath.Join(host.DataDir(), "built")
	relBase, err := filepath.Rel(builtRoot, base)
	if err != nil {
		log.Fatalf("cache: relative path for %s: %v", base, err)
	}
	relBase = filepath.ToSlash(relBase)
	return []string{
		relBase + ".info.txt",
		relBase + ".tar.gz",
		relBase + "_native.tar.gz",
	}
}

// CollectNeededPackages returns pkg plus its transitive dependencies for h.
func CollectNeededPackages(pkgs []*Package, h *host.Host) []*Package {
	return collectPackages(pkgs, h, nil)
}

// collectCachePullPackages returns packages whose artifacts should be
// downloaded for pkgs on h. A package whose current-hash artifacts are
// already local or complete on the release is included (so it can be
// fetched), but its dependencies are not: those are only needed to rebuild
// it, and a cache hit means it will not be rebuilt.
func collectCachePullPackages(pkgs []*Package, h *host.Host, remote map[string]bool) []*Package {
	return collectPackages(pkgs, h, remote)
}

// collectPackages walks pkgs and their dependencies. When remote is non-nil
// (cache pull), walking stops at a package that is already usable locally or
// fully present on the release. When remote is nil (push / CI queue), the
// full transitive tree is returned.
func collectPackages(pkgs []*Package, h *host.Host, remote map[string]bool) []*Package {
	seen := map[string]*Package{}
	var walk func(*Package)
	walk = func(p *Package) {
		if p == nil {
			return
		}
		if _, ok := seen[p.Package]; ok {
			return
		}
		seen[p.Package] = p
		if remote != nil {
			localOK, remoteOK := packageCacheState(p, h, remote)
			if localOK || remoteOK {
				return
			}
		}
		for _, dep := range filteredDependencyPackages(p.Dependencies, h) {
			walk(dep)
		}
	}
	for _, p := range pkgs {
		walk(p)
	}
	out := make([]*Package, 0, len(seen))
	for _, p := range seen {
		out = append(out, p)
	}
	return out
}

func packageCacheState(p *Package, h *host.Host, remote map[string]bool) (localComplete, remoteComplete bool) {
	builtRoot := filepath.Join(host.DataDir(), "built")
	localComplete = true
	remoteComplete = true
	for _, rel := range p.BuiltRelPaths(h) {
		dest := filepath.Join(builtRoot, filepath.FromSlash(rel))
		if _, err := os.Stat(dest); err != nil {
			localComplete = false
		}
		if remote == nil || !remote[AssetNameForRel(rel)] {
			remoteComplete = false
		}
	}
	return localComplete, remoteComplete
}

// localBuiltArtifactsPresent is true when .info.txt, .tar.gz, and
// _native.tar.gz all exist locally. A lone .info.txt (partial GitHub
// download) is not a cache hit — ExtractEnv panics on the missing tars.
func (p *Package) localBuiltArtifactsPresent(h *host.Host) bool {
	local, _ := packageCacheState(p, h, nil)
	return local
}

func (p *Package) localBuiltCacheHit(h *host.Host) bool {
	if !p.localBuiltArtifactsPresent(h) {
		return false
	}
	info, err := os.ReadFile(p.GenerateBuildPath(h, "built") + ".info.txt")
	return err == nil && string(info) == p.GeneratePackageInfo(h)
}

func assetNamesFor(pkgs []*Package, h *host.Host) []string {
	names := make([]string, 0, len(pkgs)*cacheAssetSuffixes)
	for _, p := range pkgs {
		for _, rel := range p.BuiltRelPaths(h) {
			names = append(names, AssetNameForRel(rel))
		}
	}
	return names
}

func neededAssetNames(pkgs []*Package, h *host.Host) []string {
	return assetNamesFor(CollectNeededPackages(pkgs, h), h)
}

func neededPullAssetNames(pkgs []*Package, h *host.Host, remote map[string]bool) []string {
	return assetNamesFor(collectCachePullPackages(pkgs, h, remote), h)
}

var errReleaseNotFound = errors.New("cache: release not found")

func isNotFoundMsg(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "could not find") ||
		strings.Contains(msg, "http 404")
}

func loadRemoteAssets() (*remoteAssetIndex, error) {
	remoteAssetsOnce.Do(func() {
		idx := &remoteAssetIndex{
			assets: map[string]string{},
			counts: map[string]int{},
		}
		base := CacheTag()
		for i := 0; i < maxCacheShards; i++ {
			tag := cacheShardTag(base, i)
			names, err := viewReleaseAssetNames(tag)
			if err != nil {
				if errors.Is(err, errReleaseNotFound) {
					if i == 0 {
						log.Printf("cache: release %q not found; treating as empty", tag)
						idx.shards = []string{base}
						idx.counts[base] = 0
					}
					break
				}
				remoteAssetsErr = err
				return
			}
			for _, name := range names {
				idx.assets[name] = tag
			}
			idx.shards = append(idx.shards, tag)
			idx.counts[tag] = len(names)
		}
		if len(idx.shards) == 0 {
			idx.shards = []string{base}
			idx.counts[base] = 0
		}
		remoteIndex = idx
		log.Printf("cache: loaded %d assets across %d shard(s) (base=%s)",
			len(idx.assets), len(idx.shards), base)
	})
	return remoteIndex, remoteAssetsErr
}

func viewReleaseAssetNames(tag string) ([]string, error) {
	cmd := ghCmd("release", "view", tag, "--json", "assets")
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = string(exitErr.Stderr)
		}
		msg := stderr + string(out) + err.Error()
		if isNotFoundMsg(msg) {
			return nil, errReleaseNotFound
		}
		return nil, fmt.Errorf("gh release view %s: %w%s", tag, err, formatStderr(stderr))
	}
	var parsed struct {
		Assets []struct {
			Name string `json:"name"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(parsed.Assets))
	for _, a := range parsed.Assets {
		if a.Name != "" {
			names = append(names, a.Name)
		}
	}
	return names, nil
}

func formatStderr(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	return ": " + stderr
}

func resetRemoteAssetsCache() {
	remoteAssetsOnce = sync.Once{}
	remoteIndex = nil
	remoteAssetsErr = nil
}

func ensureRelease(tag string) error {
	view := ghCmd("release", "view", tag)
	if err := view.Run(); err == nil {
		return nil
	}
	create := ghCmd("release", "create", tag,
		"--title", "simplybs build cache",
		"--notes", "Rolling cache of simplybs build artifacts (per-package, content-addressed by short hash). Overflow shards use the same base tag plus .sN because GitHub limits each Release to 1000 assets.",
	)
	create.Stdout = os.Stdout
	create.Stderr = os.Stderr
	if err := create.Run(); err != nil {
		// Another process may have created this shard first.
		if view := ghCmd("release", "view", tag); view.Run() == nil {
			return nil
		}
		return err
	}
	return nil
}

func isRetryableCacheDownload(err error, stderr string) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(stderr + " " + err.Error())
	if strings.Contains(msg, "http 404") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "could not find") ||
		strings.Contains(msg, "no assets match") {
		return false
	}
	return strings.Contains(msg, "http 5") ||
		strings.Contains(msg, "http 429") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "timed out") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "internal_error") ||
		strings.Contains(msg, "temporarily") ||
		strings.Contains(msg, "tls handshake") ||
		strings.Contains(msg, "eof") ||
		strings.Contains(msg, "exit status")
}

func downloadAsset(tag, assetName, destPath string) error {
	var last error
	for attempt := 1; attempt <= cacheDownloadRetries; attempt++ {
		err, stderr := downloadAssetOnce(tag, assetName, destPath)
		if err == nil {
			return nil
		}
		last = err
		if !isRetryableCacheDownload(err, stderr) {
			if stderr != "" {
				return fmt.Errorf("%w%s", err, formatStderr(stderr))
			}
			return err
		}
		if attempt < cacheDownloadRetries {
			delay := cacheDownloadBackoff * time.Duration(1<<(attempt-1))
			log.Printf("cache: retry %d/%d downloading %s after %v: %v",
				attempt, cacheDownloadRetries, assetName, delay, err)
			time.Sleep(delay)
		}
	}
	return last
}

func downloadAssetOnce(tag, assetName, destPath string) (err error, stderr string) {
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return err, ""
	}
	staging, err := os.MkdirTemp("", "simplybs-cache-*")
	if err != nil {
		return err, ""
	}
	defer os.RemoveAll(staging)

	cmd := ghCmd("release", "download", tag, "-p", assetName, "-D", staging)
	cmd.Stdout = os.Stdout
	var errBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &errBuf)
	if err := cmd.Run(); err != nil {
		return err, errBuf.String()
	}
	src := filepath.Join(staging, assetName)
	// gh may write the literal asset name; if the pattern matched a file with
	// a different on-disk name, pick the sole file in staging.
	if _, err := os.Stat(src); err != nil {
		entries, readErr := os.ReadDir(staging)
		if readErr != nil {
			return err, errBuf.String()
		}
		if len(entries) != 1 || entries[0].IsDir() {
			return fmt.Errorf("cache: expected one downloaded file for %s", assetName), errBuf.String()
		}
		src = filepath.Join(staging, entries[0].Name())
	}
	tmp := destPath + ".tmp"
	if err := copyFile(src, tmp); err != nil {
		return err, ""
	}
	return os.Rename(tmp, destPath), ""
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// PackageCacheOnRelease reports whether all built artifacts for p on host h
// already exist on the configured GitHub Release cache (any shard). When
// cache is disabled, it returns false.
func PackageCacheOnRelease(p *Package, h *host.Host) (bool, error) {
	if !CacheEnabled() {
		return false, nil
	}
	idx, err := loadRemoteAssets()
	if err != nil {
		return false, err
	}
	for _, rel := range p.BuiltRelPaths(h) {
		if !idx.has(AssetNameForRel(rel)) {
			return false, nil
		}
	}
	return true, nil
}

// TryPullPackageCache downloads missing built artifacts for one package from
// the GitHub release cache. Returns true when a complete local cache is present
// afterwards (either already was, or was fetched).
func TryPullPackageCache(p *Package, h *host.Host) bool {
	if !CacheEnabled() {
		return false
	}
	idx, err := loadRemoteAssets()
	if err != nil {
		log.Printf("cache: failed to list release assets: %v", err)
		return false
	}
	builtRoot := filepath.Join(host.DataDir(), "built")
	ok := true
	for _, rel := range p.BuiltRelPaths(h) {
		dest := filepath.Join(builtRoot, filepath.FromSlash(rel))
		if _, err := os.Stat(dest); err == nil {
			continue
		}
		name := AssetNameForRel(rel)
		tag, present := idx.tagFor(name)
		if !present {
			ok = false
			continue
		}
		log.Printf("[%s][%s] cache pull: %s", h.Triplet, p.Package, name)
		if err := downloadAsset(tag, name, dest); err != nil {
			log.Printf("[%s][%s] cache pull failed for %s: %v", h.Triplet, p.Package, name, err)
			ok = false
		}
	}
	return ok
}

// CachePull downloads built artifacts needed for pkgs on host h.
// It does not fetch dependency trees of packages that are already
// complete locally or on the release.
func CachePull(pkgs []*Package, h *host.Host) error {
	if _, _, err := requireCacheConfig(); err != nil {
		return err
	}
	idx, err := loadRemoteAssets()
	if err != nil {
		return fmt.Errorf("cache: list assets: %w", err)
	}

	builtRoot := filepath.Join(host.DataDir(), "built")
	names := neededPullAssetNames(pkgs, h, idx.nameSet())
	toDownload := 0
	skippedLocal := 0
	missingRemote := 0
	var firstErr error
	for _, name := range names {
		rel := RelFromAssetName(name)
		dest := filepath.Join(builtRoot, filepath.FromSlash(rel))
		if _, err := os.Stat(dest); err == nil {
			skippedLocal++
			continue
		}
		tag, present := idx.tagFor(name)
		if !present {
			missingRemote++
			continue
		}
		log.Printf("cache: download %s", name)
		if err := downloadAsset(tag, name, dest); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("cache: download %s: %w", name, err)
			}
			log.Printf("cache: download %s failed: %v", name, err)
			continue
		}
		toDownload++
	}
	nAssets, nShards := idx.stats()
	log.Printf("cache pull: downloaded=%d already-local=%d not-on-release=%d needed=%d remote-assets=%d shards=%d base=%s",
		toDownload, skippedLocal, missingRemote, len(names), nAssets, nShards, CacheTag())
	return firstErr
}

// TryPushPackageCache uploads this package's built artifacts that are missing
// from the cache. No-op when cache is disabled or everything is already remote.
func TryPushPackageCache(p *Package, h *host.Host) {
	if !CacheEnabled() {
		return
	}
	idx, err := loadRemoteAssets()
	if err != nil {
		log.Printf("[%s][%s] cache push: list assets: %v", h.Triplet, p.Package, err)
		return
	}
	builtRoot := filepath.Join(host.DataDir(), "built")
	var uploadPaths []string
	var uploadNames []string
	for _, rel := range p.BuiltRelPaths(h) {
		name := AssetNameForRel(rel)
		if idx.has(name) {
			continue
		}
		abs := filepath.Join(builtRoot, filepath.FromSlash(rel))
		if _, err := os.Stat(abs); err != nil {
			continue
		}
		uploadPaths = append(uploadPaths, abs)
		uploadNames = append(uploadNames, name)
	}
	if len(uploadPaths) == 0 {
		return
	}
	if err := uploadMissing(idx, uploadPaths, uploadNames); err != nil {
		log.Printf("[%s][%s] cache push failed: %v", h.Triplet, p.Package, err)
		return
	}
	for _, name := range uploadNames {
		log.Printf("[%s][%s] cache push: %s", h.Triplet, p.Package, name)
	}
}

func uploadMissing(idx *remoteAssetIndex, uploadPaths, uploadNames []string) error {
	if len(uploadPaths) != len(uploadNames) {
		return fmt.Errorf("cache: upload path/name count mismatch")
	}
	i := 0
	for i < len(uploadPaths) {
		remoteMu.Lock()
		tag, n := idx.pickShardCapacity(len(uploadPaths) - i)
		remoteMu.Unlock()
		if n < 1 {
			return fmt.Errorf("cache: no shard capacity for %d remaining file(s)", len(uploadPaths)-i)
		}
		batchPaths := uploadPaths[i : i+n]
		batchNames := uploadNames[i : i+n]
		if err := ensureRelease(tag); err != nil {
			return fmt.Errorf("cache: ensure release %s: %w", tag, err)
		}
		if err := uploadAssets(tag, batchPaths, batchNames); err != nil {
			if isReleaseFullError(err) {
				log.Printf("cache: release %s is full; overflowing to next shard", tag)
				remoteMu.Lock()
				idx.counts[tag] = githubReleaseAssetLimit
				remoteMu.Unlock()
				continue
			}
			return err
		}
		remoteMu.Lock()
		for _, name := range batchNames {
			idx.assets[name] = tag
		}
		idx.counts[tag] += n
		remoteMu.Unlock()
		i += n
	}
	return nil
}

func isReleaseFullError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "1000") && strings.Contains(msg, "asset") {
		return true
	}
	if strings.Contains(msg, "asset limit") || strings.Contains(msg, "too many assets") {
		return true
	}
	if strings.Contains(msg, "published asset") && strings.Contains(msg, "limit") {
		return true
	}
	return strings.Contains(msg, "exceeded") && strings.Contains(msg, "asset")
}

func uploadAssets(tag string, uploadPaths, uploadNames []string) error {
	staging, err := os.MkdirTemp("", "simplybs-cache-upload-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	staged := make([]string, 0, len(uploadPaths))
	for i, src := range uploadPaths {
		dst := filepath.Join(staging, uploadNames[i])
		if err := copyFile(src, dst); err != nil {
			return err
		}
		staged = append(staged, dst)
	}

	log.Printf("cache push: uploading %d file(s) to %s...", len(staged), tag)
	args := append([]string{"release", "upload", tag}, staged...)
	cmd := ghCmd(args...)
	cmd.Stdout = os.Stdout
	var stderr bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cache: upload: %w%s", err, formatStderr(stderr.String()))
	}
	log.Printf("cache push: uploaded=%d tag=%s", len(staged), tag)
	return nil
}

// CachePush uploads local built artifacts that are not yet on the cache.
// When pkgs is non-empty, only artifacts for those packages (and their deps)
// on host h are considered; otherwise every file under DataDir()/built is.
func CachePush(pkgs []*Package, h *host.Host) error {
	if _, _, err := requireCacheConfig(); err != nil {
		return err
	}
	resetRemoteAssetsCache()
	idx, err := loadRemoteAssets()
	if err != nil {
		return fmt.Errorf("cache: list assets: %w", err)
	}

	builtRoot := filepath.Join(host.DataDir(), "built")
	var uploadPaths []string
	var uploadNames []string

	consider := func(absPath, name string) {
		if idx.has(name) {
			return
		}
		uploadPaths = append(uploadPaths, absPath)
		uploadNames = append(uploadNames, name)
	}

	if len(pkgs) > 0 && h != nil {
		for _, name := range neededAssetNames(pkgs, h) {
			rel := RelFromAssetName(name)
			abs := filepath.Join(builtRoot, filepath.FromSlash(rel))
			if _, err := os.Stat(abs); err != nil {
				continue
			}
			consider(abs, name)
		}
	} else {
		if _, err := os.Stat(builtRoot); os.IsNotExist(err) {
			log.Printf("cache: built dir not found: %s", builtRoot)
			return nil
		}
		err := filepath.WalkDir(builtRoot, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(builtRoot, path)
			if err != nil {
				return err
			}
			consider(path, AssetNameForRel(rel))
			return nil
		})
		if err != nil {
			return err
		}
	}

	if len(uploadPaths) == 0 {
		nAssets, nShards := idx.stats()
		log.Printf("cache push: nothing to upload (base=%s remote-assets=%d shards=%d)",
			CacheTag(), nAssets, nShards)
		return nil
	}

	return uploadMissing(idx, uploadPaths, uploadNames)
}
