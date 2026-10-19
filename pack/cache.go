package pack

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	sbsconfig "github.com/mrcyjanek/simplybs/config"
	"github.com/mrcyjanek/simplybs/host"
)

// Remote build-cache objects live on sbscache. Paths under BuiltDir are
// flattened with '+' so object names stay a single path segment:
//
//	x86_64-linux-gnu/zlib-1.3.1-deadbeef.tar.gz
//	  -> x86_64-linux-gnu+zlib-1.3.1-deadbeef.tar.gz
//
// One tag holds every object. Cache is enabled when a token is configured.
// A missing token logs a warning and skips the cloud. A signature mismatch
// exits 1. Signed manifests use these names, including '@' in package names.
// The HTTP client encodes '@' before it talks to sbscache.

const cacheAssetSuffixes = 3 // .info.txt, .tar.gz, _native.tar.gz

var (
	remoteAssetsOnce sync.Once
	remoteMu         sync.Mutex
	remoteIndex      *remoteAssetIndex
	remoteAssetsErr  error
	cacheWarnOnce    sync.Once
)

type remoteAssetIndex struct {
	objects map[string]remoteObject
}

func (idx *remoteAssetIndex) has(name string) bool {
	if idx == nil {
		return false
	}
	remoteMu.Lock()
	defer remoteMu.Unlock()
	_, ok := idx.objects[name]
	return ok
}

func (idx *remoteAssetIndex) add(obj remoteObject) {
	remoteMu.Lock()
	defer remoteMu.Unlock()
	if idx.objects == nil {
		idx.objects = map[string]remoteObject{}
	}
	idx.objects[obj.Name] = obj
}

func (idx *remoteAssetIndex) nameSet() map[string]bool {
	if idx == nil {
		return map[string]bool{}
	}
	remoteMu.Lock()
	defer remoteMu.Unlock()
	out := make(map[string]bool, len(idx.objects))
	for n := range idx.objects {
		out[n] = true
	}
	return out
}

func (idx *remoteAssetIndex) stats() (n int, bytes int64) {
	if idx == nil {
		return 0, 0
	}
	remoteMu.Lock()
	defer remoteMu.Unlock()
	for _, obj := range idx.objects {
		n++
		bytes += obj.Size
	}
	return n, bytes
}

// CacheTag returns the sbscache tag.
func CacheTag() string {
	tag, err := sbsconfig.Tag()
	if err != nil {
		log.Printf("cache: tag: %v", err)
		return ""
	}
	return tag
}

// CacheEnabled reports whether a cloud token is configured.
func CacheEnabled() bool {
	tok, err := sbsconfig.Token()
	if err != nil {
		log.Printf("cache: config: %v", err)
		return false
	}
	return tok != ""
}

func warnCacheDisabled() {
	cacheWarnOnce.Do(func() {
		path, err := sbsconfig.Path()
		if err != nil {
			path = "$HOME/.config/simplybs/config.json"
		}
		log.Printf("cache: cloud cache not configured (no token in %s or SIMPLYBS_TOKEN).", path)
	})
}

// AssetNameForRel converts a path relative to BuiltDir into a flat object name.
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

// BuiltRelPaths returns the paths (relative to BuiltDir) for a package's
// cache artifacts on host h. The signature is separate.
func (p *Package) BuiltRelPaths(h *host.Host) []string {
	base := p.GenerateBuildPath(h, "built")
	builtRoot := host.BuiltDir()
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

func collectCachePullPackages(pkgs []*Package, h *host.Host, remote map[string]bool) []*Package {
	return collectPackages(pkgs, h, remote)
}

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
	builtRoot := host.BuiltDir()
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

func loadRemoteAssets() (*remoteAssetIndex, error) {
	remoteAssetsOnce.Do(func() {
		c, err := cacheClient()
		if err != nil {
			remoteAssetsErr = err
			return
		}
		tag := CacheTag()
		objs, err := c.List(tag)
		if err != nil {
			remoteAssetsErr = err
			return
		}
		idx := &remoteAssetIndex{objects: map[string]remoteObject{}}
		for _, obj := range objs {
			if obj.Name != "" {
				idx.objects[obj.Name] = obj
			}
		}
		remoteIndex = idx
		log.Printf("cache: loaded %d objects (tag=%s)", len(idx.objects), tag)
	})
	return remoteIndex, remoteAssetsErr
}

func resetRemoteAssetsCache() {
	remoteAssetsOnce = sync.Once{}
	remoteIndex = nil
	remoteAssetsErr = nil
}

func exitSignatureMismatch(err error) {
	log.Printf("cache: signature mismatch: %v", err)
	os.Exit(1)
}

// PackageCacheOnRelease reports whether all built artifacts for p on host h
// already exist on the configured tag. When cache is disabled, it returns false.
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

// TryPullPackageCache downloads missing built artifacts for one package.
// Returns true when a complete local cache is present afterwards.
func TryPullPackageCache(p *Package, h *host.Host) bool {
	if !CacheEnabled() {
		warnCacheDisabled()
		return false
	}
	idx, err := loadRemoteAssets()
	if err != nil {
		log.Printf("cache: failed to list objects: %v", err)
		return false
	}
	downloaded, err := pullPackage(p, h, idx)
	if err != nil {
		log.Printf("[%s][%s] cache pull failed: %v", h.Triplet, p.Package, err)
		return false
	}
	if downloaded > 0 || p.localBuiltArtifactsPresent(h) {
		names := existingAssetNames(p, h)
		if err := keepNames(names); err != nil {
			log.Printf("[%s][%s] cache keep: %v", h.Triplet, p.Package, err)
		}
	}
	return p.localBuiltArtifactsPresent(h)
}

func pullPackage(p *Package, h *host.Host, idx *remoteAssetIndex) (int, error) {
	rels := p.BuiltRelPaths(h)
	root := host.BuiltDir()
	type item struct {
		name   string
		dest   string
		local  bool
		remote bool
	}
	payloads := make([]item, len(rels))
	allLocal := true
	allAvailable := true
	for i, rel := range rels {
		dest := filepath.Join(root, filepath.FromSlash(rel))
		it := item{name: AssetNameForRel(rel), dest: dest, remote: idx.has(AssetNameForRel(rel))}
		if _, err := os.Stat(dest); err == nil {
			it.local = true
		} else {
			allLocal = false
		}
		if !it.local && !it.remote {
			allAvailable = false
		}
		payloads[i] = it
	}
	sigRel := p.signatureRel(h)
	sigName := AssetNameForRel(sigRel)
	sigDest := filepath.Join(root, filepath.FromSlash(sigRel))
	sigLocal := false
	if _, err := os.Stat(sigDest); err == nil {
		sigLocal = true
	}
	sigRemote := idx.has(sigName)

	if allLocal {
		return 0, nil
	}
	c, err := cacheClient()
	if err != nil {
		return 0, err
	}
	tag := CacheTag()
	if !allAvailable {
		n := 0
		for _, it := range payloads {
			if it.local || !it.remote {
				continue
			}
			log.Printf("[%s][%s] cache pull: %s", h.Triplet, p.Package, it.name)
			if err := c.Download(tag, it.name, it.dest); err != nil {
				return n, err
			}
			n++
		}
		return n, nil
	}
	if !sigLocal && !sigRemote {
		exitSignatureMismatch(fmt.Errorf("signature missing for %s", sigName))
	}

	var temps []string
	cleanup := func() {
		for _, tmp := range temps {
			os.Remove(tmp)
		}
	}
	staged := map[string]string{}
	stage := func(name, dest string, local bool) error {
		if local {
			return nil
		}
		tmp := dest + ".tmp"
		log.Printf("[%s][%s] cache pull: %s", h.Triplet, p.Package, name)
		if err := c.Download(tag, name, tmp); err != nil {
			return err
		}
		temps = append(temps, tmp)
		staged[dest] = tmp
		return nil
	}
	for _, it := range payloads {
		if err := stage(it.name, it.dest, it.local); err != nil {
			cleanup()
			return 0, err
		}
	}
	if err := stage(sigName, sigDest, sigLocal); err != nil {
		cleanup()
		if os.IsNotExist(err) || errorsIsNotFound(err) {
			exitSignatureMismatch(fmt.Errorf("signature missing for %s", sigName))
		}
		return 0, err
	}

	var paths [3]string
	var names [3]string
	for i, it := range payloads {
		names[i] = it.name
		if tmp, ok := staged[it.dest]; ok {
			paths[i] = tmp
		} else {
			paths[i] = it.dest
		}
	}
	sigPath := sigDest
	if tmp, ok := staged[sigDest]; ok {
		sigPath = tmp
	}
	if err := verifyPackFiles(paths, names, sigPath); err != nil {
		cleanup()
		exitSignatureMismatch(err)
	}
	for dest, tmp := range staged {
		if err := os.Rename(tmp, dest); err != nil {
			cleanup()
			return 0, err
		}
	}
	return len(staged), nil
}

func errorsIsNotFound(err error) bool {
	return err != nil && (os.IsNotExist(err) || err == errNotFound || strings.Contains(err.Error(), errNotFound.Error()))
}

// CachePull downloads built artifacts needed for pkgs on host h.
func CachePull(pkgs []*Package, h *host.Host) error {
	if !CacheEnabled() {
		warnCacheDisabled()
		return nil
	}
	idx, err := loadRemoteAssets()
	if err != nil {
		return fmt.Errorf("cache: list objects: %w", err)
	}
	list := collectCachePullPackages(pkgs, h, idx.nameSet())
	downloaded := 0
	skipped := 0
	missing := 0
	var firstErr error
	var keep []string
	for _, p := range list {
		if p.localBuiltArtifactsPresent(h) {
			skipped += cacheAssetSuffixes
			keep = append(keep, existingAssetNames(p, h)...)
			continue
		}
		n, err := pullPackage(p, h, idx)
		downloaded += n
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			log.Printf("cache: download %s failed: %v", p.Package, err)
		}
		if !p.localBuiltArtifactsPresent(h) {
			missing++
		}
		keep = append(keep, existingAssetNames(p, h)...)
	}
	if err := keepNames(keep); err != nil && firstErr == nil {
		firstErr = err
	}
	nObj, nBytes := idx.stats()
	log.Printf("cache pull: downloaded=%d already-local-packages=%d incomplete=%d needed-packages=%d remote-objects=%d remote-bytes=%d tag=%s",
		downloaded, skipped/cacheAssetSuffixes, missing, len(list), nObj, nBytes, CacheTag())
	return firstErr
}

func existingAssetNames(p *Package, h *host.Host) []string {
	root := host.BuiltDir()
	rels := append(append([]string{}, p.BuiltRelPaths(h)...), p.signatureRel(h))
	var names []string
	for _, rel := range rels {
		dest := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := os.Stat(dest); err == nil {
			names = append(names, AssetNameForRel(rel))
		}
	}
	return names
}

// TryPushPackageCache uploads this package's built artifacts that are missing
// from the cache.
func TryPushPackageCache(p *Package, h *host.Host) {
	if !CacheEnabled() {
		warnCacheDisabled()
		return
	}
	idx, err := loadRemoteAssets()
	if err != nil {
		log.Printf("[%s][%s] cache push: list objects: %v", h.Triplet, p.Package, err)
		return
	}
	names, err := uploadPackage(p, h, idx)
	if err != nil {
		log.Printf("[%s][%s] cache push failed: %v", h.Triplet, p.Package, err)
		return
	}
	if err := keepNames(names); err != nil {
		log.Printf("[%s][%s] cache keep: %v", h.Triplet, p.Package, err)
	}
}

func uploadPackage(p *Package, h *host.Host, idx *remoteAssetIndex) ([]string, error) {
	if p.localBuiltArtifactsPresent(h) {
		if err := p.writePackSignature(h); err != nil {
			return nil, err
		}
	}
	root := host.BuiltDir()
	rels := append(append([]string{}, p.BuiltRelPaths(h)...), p.signatureRel(h))
	c, err := cacheClient()
	if err != nil {
		return nil, err
	}
	tag := CacheTag()
	for _, rel := range rels {
		name := AssetNameForRel(rel)
		if idx.has(name) {
			continue
		}
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := os.Stat(abs); err != nil {
			continue
		}
		log.Printf("[%s][%s] cache push: %s", h.Triplet, p.Package, name)
		if err := c.Upload(tag, name, abs); err != nil {
			return nil, err
		}
		st, _ := os.Stat(abs)
		var size int64
		if st != nil {
			size = st.Size()
		}
		idx.add(remoteObject{Name: name, Size: size})
	}
	return existingAssetNames(p, h), nil
}

// CachePush uploads local built artifacts that are not yet on the cache.
func CachePush(pkgs []*Package, h *host.Host) error {
	if !CacheEnabled() {
		warnCacheDisabled()
		return nil
	}
	resetRemoteAssetsCache()
	idx, err := loadRemoteAssets()
	if err != nil {
		return fmt.Errorf("cache: list objects: %w", err)
	}
	if len(pkgs) > 0 && h != nil {
		var keep []string
		var firstErr error
		for _, p := range CollectNeededPackages(pkgs, h) {
			names, err := uploadPackage(p, h, idx)
			if err != nil {
				firstErr = err
				break
			}
			keep = append(keep, names...)
		}
		if err := keepNames(keep); err != nil && firstErr == nil {
			firstErr = err
		}
		return firstErr
	}
	return uploadBuiltTree(idx)
}

func uploadBuiltTree(idx *remoteAssetIndex) error {
	root := host.BuiltDir()
	if _, err := os.Stat(root); os.IsNotExist(err) {
		log.Printf("cache: built dir not found: %s", root)
		return nil
	}
	if err := signBuiltTree(root); err != nil {
		return err
	}
	c, err := cacheClient()
	if err != nil {
		return err
	}
	tag := CacheTag()
	var keep []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name := AssetNameForRel(rel)
		keep = append(keep, name)
		if idx.has(name) {
			return nil
		}
		log.Printf("cache push: %s", name)
		if err := c.Upload(tag, name, path); err != nil {
			return err
		}
		st, _ := os.Stat(path)
		var size int64
		if st != nil {
			size = st.Size()
		}
		idx.add(remoteObject{Name: name, Size: size})
		return nil
	})
	if err != nil {
		return err
	}
	if len(keep) == 0 {
		n, b := idx.stats()
		log.Printf("cache push: nothing to upload (tag=%s remote-objects=%d remote-bytes=%d)", CacheTag(), n, b)
		return nil
	}
	return keepNames(keep)
}

func keepNames(names []string) error {
	if len(names) == 0 || !CacheEnabled() {
		return nil
	}
	seen := map[string]bool{}
	uniq := make([]string, 0, len(names))
	for _, n := range names {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		uniq = append(uniq, n)
	}
	c, err := cacheClient()
	if err != nil {
		return err
	}
	return c.Keep(CacheTag(), uniq)
}

// Status prints local and cloud cache sizes.
func Status() {
	bn, bb := dirStats(host.BuiltDir())
	sn, sb := dirStats(host.SourcesDir())
	fmt.Printf("local built: %d files, %s (%s)\n", bn, humanBytes(bb), host.BuiltDir())
	fmt.Printf("local sources: %d files, %s (%s)\n", sn, humanBytes(sb), host.SourcesDir())
	if !CacheEnabled() {
		warnCacheDisabled()
		fmt.Println("cloud: not configured")
		return
	}
	idx, err := loadRemoteAssets()
	if err != nil {
		fmt.Printf("cloud: list failed: %v\n", err)
		return
	}
	n, b := idx.stats()
	fmt.Printf("cloud: %d objects, %s (tag %s)\n", n, humanBytes(b), CacheTag())
}

// CleanLocal removes local cache trees. It does not touch /opt/_.
func CleanLocal() {
	for _, dir := range []string{host.BuiltDir(), host.SourcesDir(), host.WorkDir(), host.StagingDir()} {
		fmt.Printf("removing %s\n", dir)
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("clean: %s: %v", dir, err)
		}
	}
}

// Sync uploads local built files that are missing from the cloud and refreshes
// retention for every local object name.
func Sync() error {
	return CachePush(nil, nil)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func dirStats(root string) (n int, bytes int64) {
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		n++
		if st, err := d.Info(); err == nil {
			bytes += st.Size()
		}
		return nil
	})
	return n, bytes
}
