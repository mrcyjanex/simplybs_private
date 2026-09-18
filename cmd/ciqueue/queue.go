package main

import (
	"fmt"
	"log"
	"path"
	"sort"
	"strings"

	"github.com/mrcyjanek/simplybs/host"
	"github.com/mrcyjanek/simplybs/pack"
)

// DefaultHosts is the Linux CI host list, in queue order.
// Keep x86_64-linux-gnu first (native-ish on the GHA amd64 runner).
// 32-bit Android (armv7a-linux-androideabi) stays in host.SupportedHosts
// but is omitted from CI for now.
var DefaultHosts = []string{
	"x86_64-linux-gnu",
	"aarch64-linux-gnu",
	"aarch64-linux-android",
	"x86_64-linux-android",
	"x86_64-w64-mingw32",
	"aarch64-apple-darwin",
	"x86_64-apple-darwin",
	"aarch64-apple-ios",
	"aarch64-apple-ios-simulator",
}

// Item is one (package, host) node in the build queue.
type Item struct {
	Package string `json:"package"`
	Host    string `json:"host"`
}

func (it Item) key() string {
	return it.Package + "\x00" + it.Host
}

// Result is JSON written to stdout for the Actions worker.
type Result struct {
	Status         string   `json:"status"` // next | done | blocked
	Package        string   `json:"package,omitempty"`
	Host           string   `json:"host,omitempty"`
	Needed         int      `json:"needed"`
	Remaining      []Item   `json:"remaining,omitempty"`
	RemainingCount int      `json:"remaining_count,omitempty"`
	ChangedPkg     []string `json:"changed_packages,omitempty"`
	Message        string   `json:"message,omitempty"`
}

type cacheFn func(*pack.Package, *host.Host) (bool, error)

type queueOpts struct {
	changedFiles []string
	hosts        []string
	cached       cacheFn
	extraRoots   []string
}

func nextQueue(opts queueOpts) (Result, error) {
	hosts, err := resolveHosts(opts.hosts)
	if err != nil {
		return Result{}, err
	}
	cached := opts.cached
	if cached == nil {
		cached = pack.PackageCacheOnRelease
	}

	allPkgs := pack.GetAllPackages()
	names := make([]string, 0, len(allPkgs))
	byName := map[string]*pack.Package{}
	for _, p := range allPkgs {
		names = append(names, p.Package)
		byName[p.Package] = p
	}

	changed := packagesFromPaths(opts.changedFiles, names)
	for _, extra := range opts.extraRoots {
		extra = strings.TrimSpace(extra)
		if extra != "" {
			changed[extra] = true
		}
	}
	changedList := sortedKeys(changed)
	log.Printf("ciqueue: %d changed packages, %d hosts", len(changedList), len(hosts))

	roots := make([]*pack.Package, 0, len(changed))
	for _, name := range changedList {
		root, ok := byName[name]
		if !ok {
			continue
		}
		roots = append(roots, root)
	}

	// Cache artifact names already queued or known present. Native packages
	// often share one built/ path across -host triplets; listing them once
	// avoids N identical CI slots and keeps readiness correct.
	needed := map[string]Item{}
	seenArt := map[string]bool{}
	queuedArt := map[string]string{} // artifact key -> needed item key
	artMemo := map[string]string{}
	artifactOf := func(p *pack.Package, ht *host.Host) string {
		k := p.Package + "\x00" + ht.Triplet
		if a, ok := artMemo[k]; ok {
			return a
		}
		a := strings.Join(p.BuiltRelPaths(ht), "\x00")
		artMemo[k] = a
		return a
	}

	for _, h := range hosts {
		ht := host.SupportedHosts[h]
		if len(roots) == 0 {
			continue
		}
		tree := pack.CollectNeededPackages(roots, ht)
		cacheHits := 0
		added := 0
		for _, p := range tree {
			art := artifactOf(p, ht)
			if seenArt[art] {
				continue
			}
			seenArt[art] = true
			onRelease, err := cached(p, ht)
			if err != nil {
				return Result{}, fmt.Errorf("cache lookup %s %s: %w", p.Package, h, err)
			}
			if onRelease {
				cacheHits++
				continue
			}
			it := Item{Package: p.Package, Host: h}
			needed[it.key()] = it
			queuedArt[art] = it.key()
			added++
		}
		log.Printf("ciqueue: %s tree=%d queued+=%d cache-hit=%d needed=%d", h, len(tree), added, cacheHits, len(needed))
	}

	if len(needed) == 0 {
		return Result{
			Status:     "done",
			Needed:     0,
			ChangedPkg: changedList,
			Message:    "no uncached work for changed packages",
		}, nil
	}

	var ready []Item
	var blocked []Item
	for _, it := range needed {
		p, ok := byName[it.Package]
		if !ok {
			return Result{}, fmt.Errorf("unknown package %s", it.Package)
		}
		ht := host.SupportedHosts[it.Host]
		okReady := true
		for _, d := range pack.CollectNeededPackages([]*pack.Package{p}, ht) {
			if d.Package == p.Package {
				continue
			}
			art := artifactOf(d, ht)
			if k := queuedArt[art]; k != "" && k != it.key() {
				okReady = false
				break
			}
		}
		if okReady {
			ready = append(ready, it)
		} else {
			blocked = append(blocked, it)
		}
	}

	if len(ready) == 0 {
		return Result{
			Status:         "blocked",
			Needed:         len(needed),
			Remaining:      itemsSorted(needed, hosts, byName),
			RemainingCount: len(needed),
			ChangedPkg:     changedList,
			Message:        fmt.Sprintf("needed %d items but none are ready (%d blocked)", len(needed), len(blocked)),
		}, nil
	}

	sortItems(ready, hosts, byName)
	pick := ready[0]
	delete(needed, pick.key())
	remaining := itemsSorted(needed, hosts, byName)
	log.Printf("ciqueue: pick %s / %s (needed=%d ready=%d blocked=%d)", pick.Package, pick.Host, len(needed)+1, len(ready), len(blocked))
	return Result{
		Status:         "next",
		Package:        pick.Package,
		Host:           pick.Host,
		Needed:         len(needed) + 1,
		Remaining:      remaining,
		RemainingCount: len(remaining),
		ChangedPkg:     changedList,
	}, nil
}

func resolveHosts(hosts []string) ([]string, error) {
	if len(hosts) == 0 {
		hosts = append([]string{}, DefaultHosts...)
	}
	out := make([]string, 0, len(hosts))
	seen := map[string]bool{}
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] {
			continue
		}
		if host.SupportedHosts[h] == nil {
			return nil, fmt.Errorf("unsupported host %q", h)
		}
		seen[h] = true
		out = append(out, h)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no hosts")
	}
	return out, nil
}

func itemsSorted(m map[string]Item, hosts []string, byName map[string]*pack.Package) []Item {
	out := make([]Item, 0, len(m))
	for _, it := range m {
		out = append(out, it)
	}
	sortItems(out, hosts, byName)
	return out
}

func sortItems(items []Item, hosts []string, byName map[string]*pack.Package) {
	hostIdx := map[string]int{}
	for i, h := range hosts {
		hostIdx[h] = i
	}
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		ap, bp := byName[a.Package], byName[b.Package]
		aNative, bNative := ap != nil && ap.Type == "native", bp != nil && bp.Type == "native"
		if aNative != bNative {
			return aNative
		}
		if a.Package != b.Package {
			return a.Package < b.Package
		}
		return hostIdx[a.Host] < hostIdx[b.Host]
	})
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func packagesFromPaths(files, packageNames []string) map[string]bool {
	nameSet := map[string]bool{}
	for _, n := range packageNames {
		nameSet[n] = true
	}
	out := map[string]bool{}
	for _, f := range files {
		f = strings.TrimSpace(strings.ReplaceAll(f, "\\", "/"))
		if f == "" {
			continue
		}
		for _, name := range packagesAffectedByPath(f, packageNames, nameSet) {
			out[name] = true
		}
	}
	return out
}

func packagesAffectedByPath(file string, packageNames []string, nameSet map[string]bool) []string {
	file = strings.TrimPrefix(file, "./")
	if strings.HasPrefix(file, "packages/") && strings.HasSuffix(file, ".json") {
		name := strings.TrimSuffix(strings.TrimPrefix(file, "packages/"), ".json")
		if nameSet[name] {
			return []string{name}
		}
		return nil
	}
	if !strings.HasPrefix(file, "patches/") {
		return nil
	}
	rest := strings.TrimPrefix(file, "patches/")
	dir := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		dir = rest[:i]
	}
	if dir == "" {
		return nil
	}

	// patches/native/foo/... → native/foo (possibly nested)
	if dir == "native" {
		rel := strings.TrimPrefix(rest, "native/")
		if rel == "" || rel == rest {
			return matchPatchPrefix("native", packageNames)
		}
		// longest package-name prefix of the patch path
		best := ""
		candidate := "native/" + rel
		for _, name := range packageNames {
			if name == "native" || strings.HasPrefix(name, "native/") {
				prefix := name
				if candidate == prefix || strings.HasPrefix(candidate, prefix+"/") {
					if len(name) > len(best) {
						best = name
					}
				}
			}
		}
		if best != "" {
			return []string{best}
		}
		return matchPatchPrefix("native/"+strings.Split(rel, "/")[0], packageNames)
	}

	if nameSet[dir] {
		return []string{dir}
	}
	if nameSet["native/"+dir] {
		return []string{"native/" + dir}
	}
	return matchPatchPrefix(dir, packageNames)
}

func matchPatchPrefix(prefix string, packageNames []string) []string {
	var out []string
	for _, name := range packageNames {
		if name == prefix || strings.HasPrefix(name, prefix+"/") || strings.HasPrefix(name, prefix+"@") || path.Base(name) == prefix {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
