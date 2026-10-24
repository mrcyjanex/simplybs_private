package pack

import (
	"bytes"
	"crypto"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/mrcyjanek/simplybs/host"
)

func TestDirectDependentsOfNativeToolchain(t *testing.T) {
	all := GetAllPackages()
	names := map[string]bool{"native/_": true}
	android := packageNameSet(DirectDependents(all, names, host.SupportedHosts["aarch64-linux-android"]))
	linux := packageNameSet(DirectDependents(all, names, host.SupportedHosts["x86_64-linux-gnu"]))

	if !android["zlib"] || !linux["zlib"] {
		t.Fatal("zlib lists native/_ on every host")
	}
	if android["native/android-clang"] || linux["native/android-clang"] {
		t.Fatal("native/android-clang depends on native/_/_, not native/_")
	}
	if android["libxcb@1_17_0"] {
		t.Fatal("libxcb@1_17_0 depends on native/_ only for linux-gnu")
	}
	if !linux["libxcb@1_17_0"] {
		t.Fatal("libxcb@1_17_0 should be a linux-gnu dependent of native/_")
	}
}

func TestAndroidToolchainEnvIsHostScoped(t *testing.T) {
	zlib, err := FindPackage("zlib")
	if err != nil {
		t.Fatal(err)
	}
	android := zlib.GeneratePackageInfo(host.SupportedHosts["aarch64-linux-android"])
	linux := zlib.GeneratePackageInfo(host.SupportedHosts["x86_64-linux-gnu"])
	if !strings.Contains(android, "ANDROID_TOOLCHAIN_ROOT") {
		t.Fatal("android zlib package info missing ANDROID_TOOLCHAIN_ROOT from native/_")
	}
	if strings.Contains(linux, "ANDROID_TOOLCHAIN_ROOT") {
		t.Fatal("linux-gnu zlib package info includes ANDROID_TOOLCHAIN_ROOT")
	}
}

func TestCollectNeededPackagesIncludesDeps(t *testing.T) {
	pkg, err := FindPackage("zlib")
	if err != nil {
		t.Fatal(err)
	}
	h := host.SupportedHosts["x86_64-linux-gnu"]
	needed := CollectNeededPackages([]*Package{pkg}, h)
	found := map[string]bool{}
	for _, p := range needed {
		found[p.Package] = true
	}
	if !found["zlib"] {
		t.Fatal("missing zlib")
	}
	if !found["native/make"] {
		t.Fatal("expected transitive native/make dependency")
	}
}

func TestLocalBuiltCacheHitRequiresAllArtifacts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	pkg, err := FindPackage("native/zlib")
	if err != nil {
		t.Fatal(err)
	}
	h := host.SupportedHosts["x86_64-linux-gnu"]
	if pkg.localBuiltArtifactsPresent(h) || pkg.localBuiltCacheHit(h) {
		t.Fatal("empty built dir must not be a cache hit")
	}

	infoPath := pkg.GenerateBuildPath(h, "built") + ".info.txt"
	if err := os.MkdirAll(filepath.Dir(infoPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(infoPath, []byte(pkg.GeneratePackageInfo(h)), 0644); err != nil {
		t.Fatal(err)
	}
	if pkg.localBuiltArtifactsPresent(h) {
		t.Fatal("lone info.txt must not count as a complete cache")
	}
	if pkg.localBuiltCacheHit(h) {
		t.Fatal("lone info.txt must not skip the build")
	}

	for _, rel := range pkg.BuiltRelPaths(h) {
		path := filepath.Join(host.BuiltDir(), filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		payload := []byte("payload")
		if strings.HasSuffix(rel, ".info.txt") {
			payload = []byte(pkg.GeneratePackageInfo(h))
		}
		if err := os.WriteFile(path, payload, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if !pkg.localBuiltArtifactsPresent(h) {
		t.Fatal("expected all three artifacts to be present")
	}
	if !pkg.localBuiltCacheHit(h) {
		t.Fatal("matching info.txt plus both archives should be a cache hit")
	}
}

func TestSignatureMismatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SIMPLYBS_TOKEN", "")
	dir := t.TempDir()
	var paths [3]string
	var names [3]string
	for i, name := range []string{"pkg.info.txt", "pkg.tar.gz", "pkg_native.tar.gz"} {
		paths[i] = filepath.Join(dir, name)
		names[i] = "triplet+" + name
		if err := os.WriteFile(paths[i], []byte("body-"+name), 0644); err != nil {
			t.Fatal(err)
		}
	}
	sig := filepath.Join(dir, "pkg.sig.v1.txt")
	if err := writePackSignatureFiles(paths, names, sig); err != nil {
		t.Fatal(err)
	}
	if err := verifyPackFiles(paths, names, sig); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths[1], []byte("tampered"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := verifyPackFiles(paths, names, sig); err == nil {
		t.Fatal("expected signature mismatch")
	}
}

func TestRetryableStatus(t *testing.T) {
	if !retryable(&statusError{code: 502, msg: "bad gateway"}) {
		t.Fatal("502 should retry")
	}
	if retryable(errNotFound) {
		t.Fatal("404 should not retry")
	}
	if retryable(&statusError{code: 401, msg: "unauthorized"}) {
		t.Fatal("401 should not retry")
	}
	if !retryable(fmt.Errorf("unexpected EOF")) {
		t.Fatal("EOF should retry")
	}
	if !retryable(fmt.Errorf(`Put "https://sbs.example/parts/2": remote error: tls: bad record MAC`)) {
		t.Fatal("tls bad record MAC should retry")
	}
}

type fakeCache struct {
	mu        sync.Mutex
	objects   map[string][]byte
	uploads   map[string]*fakeUpload
	downloads []string
	ranges    []string
	keeps     [][]string
	partPuts  int
	cutOnce   map[string]bool
}

type fakeUpload struct {
	name  string
	parts map[int][]byte
}

func newFakeCache() *fakeCache {
	return &fakeCache{
		objects: map[string][]byte{},
		uploads: map[string]*fakeUpload{},
		cutOnce: map[string]bool{},
	}
}

func (f *fakeCache) handler() http.Handler {
	return http.HandlerFunc(f.serve)
}

func (f *fakeCache) serve(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// v1 tags {tag} objects [...]
	// v1 tags {tag} keep
	if len(parts) < 4 || parts[0] != "v1" || parts[1] != "tags" {
		http.NotFound(w, r)
		return
	}
	if len(parts) >= 5 && parts[3] == "objects" && !validCacheObjectName(parts[4]) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid object name"}`))
		return
	}
	switch {
	case len(parts) == 4 && parts[3] == "keep" && r.Method == http.MethodPost:
		f.serveKeep(w, r)
	case len(parts) == 4 && parts[3] == "objects" && r.Method == http.MethodGet:
		f.serveList(w)
	case len(parts) == 5 && parts[3] == "objects" && r.Method == http.MethodGet:
		f.serveDownload(w, r, parts[4])
	case len(parts) == 6 && parts[5] == "uploads" && r.Method == http.MethodPost:
		f.serveCreate(w, parts[4])
	case len(parts) == 8 && parts[5] == "uploads" && parts[7] == "complete" && r.Method == http.MethodPost:
		f.serveComplete(w, parts[6])
	case len(parts) == 7 && parts[5] == "uploads" && r.Method == http.MethodDelete:
		f.serveAbort(w, parts[6])
	case len(parts) == 9 && parts[5] == "uploads" && parts[7] == "parts" && r.Method == http.MethodPut:
		n, _ := strconv.Atoi(parts[8])
		f.servePart(w, r, parts[6], n)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeCache) serveList(w http.ResponseWriter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.objects))
	for name := range f.objects {
		names = append(names, name)
	}
	sort.Strings(names)
	var objs []remoteObject
	for _, name := range names {
		objs = append(objs, remoteObject{Name: name, Size: int64(len(f.objects[name]))})
	}
	if objs == nil {
		objs = []remoteObject{}
	}
	json.NewEncoder(w).Encode(map[string]any{"objects": objs})
}

func (f *fakeCache) serveDownload(w http.ResponseWriter, r *http.Request, name string) {
	f.mu.Lock()
	data, ok := f.objects[name]
	cut := f.cutOnce[name]
	if cut {
		delete(f.cutOnce, name)
	}
	if ok {
		f.downloads = append(f.downloads, name)
		if rng := r.Header.Get("Range"); rng != "" {
			f.ranges = append(f.ranges, rng)
		}
	}
	f.mu.Unlock()
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"not found"}`))
		return
	}
	if cut {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", 500)
			return
		}
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			return
		}
		fmt.Fprintf(bufrw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", len(data))
		bufrw.Write(data[:4])
		bufrw.Flush()
		conn.Close()
		return
	}
	start := 0
	if rng := r.Header.Get("Range"); rng != "" {
		fmt.Sscanf(rng, "bytes=%d-", &start)
		if start > len(data) {
			start = len(data)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(data)-1, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[start:])
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func (f *fakeCache) serveCreate(w http.ResponseWriter, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("up-%d", len(f.uploads)+1)
	f.uploads[id] = &fakeUpload{name: name, parts: map[int][]byte{}}
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"uploadId": id})
}

func (f *fakeCache) servePart(w http.ResponseWriter, r *http.Request, id string, n int) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if r.ContentLength != int64(len(body)) {
		http.Error(w, "bad length", 400)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	up := f.uploads[id]
	if up == nil {
		http.NotFound(w, r)
		return
	}
	up.parts[n] = body
	f.partPuts++
	json.NewEncoder(w).Encode(map[string]any{"partNumber": n, "etag": "\"abc\""})
}

func (f *fakeCache) serveComplete(w http.ResponseWriter, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	up := f.uploads[id]
	if up == nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	var buf bytes.Buffer
	for i := 1; i <= len(up.parts); i++ {
		part, ok := up.parts[i]
		if !ok {
			http.Error(w, `{"error":"gap"}`, 400)
			return
		}
		buf.Write(part)
	}
	f.objects[up.name] = buf.Bytes()
	delete(f.uploads, id)
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{"name": up.name, "size": buf.Len(), "etag": "\"done\""})
}

func (f *fakeCache) serveAbort(w http.ResponseWriter, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.uploads, id)
	json.NewEncoder(w).Encode(map[string]bool{"aborted": true})
}

func (f *fakeCache) serveKeep(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Names []string `json:"names"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	f.mu.Lock()
	f.keeps = append(f.keeps, append([]string{}, body.Names...))
	f.mu.Unlock()
	json.NewEncoder(w).Encode(map[string][]string{"updated": body.Names, "unknown": nil})
}

func useFakeCache(t *testing.T, f *fakeCache) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(func() {
		srv.Close()
		cacheBaseURL = ""
		cachePartSize = cachePartSizeDefault
		resetRemoteAssetsCache()
	})
	cacheBaseURL = srv.URL
	resetRemoteAssetsCache()
}

func enableCache(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SIMPLYBS_TOKEN", "sbs_test")
	t.Setenv("SIMPLYBS_CACHE_TAG", "v0-sbs-test")
	old := cacheDownloadBackoff
	cacheDownloadBackoff = 0
	t.Cleanup(func() { cacheDownloadBackoff = old })
}

func TestUploadUsesMultipleParts(t *testing.T) {
	enableCache(t)
	f := newFakeCache()
	useFakeCache(t, f)
	cachePartSize = 8
	dir := t.TempDir()
	path := filepath.Join(dir, "blob.bin")
	payload := bytes.Repeat([]byte("abcdefghij"), 2) // 20 bytes, 3 parts of 8
	if err := os.WriteFile(path, payload, 0644); err != nil {
		t.Fatal(err)
	}
	c, err := cacheClient()
	if err != nil {
		t.Fatal(err)
	}
	name := "triplet+pkg.tar.gz"
	if err := c.Upload("v0-sbs-test", name, path); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.partPuts != 3 {
		t.Fatalf("parts %d", f.partPuts)
	}
	if !bytes.Equal(f.objects[name], payload) {
		t.Fatalf("stored %q", f.objects[name])
	}
}

func TestDownloadResumesRange(t *testing.T) {
	enableCache(t)
	f := newFakeCache()
	name := "triplet+pkg.tar.gz"
	f.objects[name] = []byte("0123456789abcdef")
	f.cutOnce[name] = true
	useFakeCache(t, f)
	c, err := cacheClient()
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "out.bin")
	if err := c.Download("v0-sbs-test", name, dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "0123456789abcdef" {
		t.Fatalf("got %q", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ranges) != 1 || f.ranges[0] != "bytes=4-" {
		t.Fatalf("ranges %v", f.ranges)
	}
}

func TestDownload404DoesNotWriteBody(t *testing.T) {
	enableCache(t)
	f := newFakeCache()
	useFakeCache(t, f)
	c, err := cacheClient()
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "missing.bin")
	err = c.Download("v0-sbs-test", "nope.tar.gz", dest)
	if err == nil || !errorsIsNotFound(err) {
		t.Fatalf("err %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		data, _ := os.ReadFile(dest)
		t.Fatalf("dest exists (%v): %q", statErr, data)
	}
}

func TestObjectNameEscapesPlus(t *testing.T) {
	c := &sbsClient{base: "https://sbs.example", token: "sbs_x"}
	got := c.objectURL("v0-sbs-test", "x86_64-linux-gnu+zlib.tar.gz")
	if !strings.Contains(got, "x86_64-linux-gnu%2Bzlib.tar.gz") {
		t.Fatalf("url %s", got)
	}
}

func TestCacheObjectNameRoundTrip(t *testing.T) {
	cases := []string{
		"x86_64-linux-gnu+zlib-1.3.1-deadbeef.tar.gz",
		"native_libc++-21.1.3-deadbeef.tar.gz",
		"native_rust@1_91_0-1.91.0-4c3e595e.info.txt",
		"aarch64-linux-android+monero@fcmp-stage-fcmp-stage-3c051e42.info.txt",
		"foo.at.bar.tar.gz",
		"foo.at.bar@baz.tar.gz",
		"foo.at.at.bar.tar.gz",
		"foo@bar@baz.info.txt",
	}
	for _, logical := range cases {
		wire := cacheObjectName(logical)
		if !validCacheObjectName(wire) {
			t.Fatalf("wire name %q from %q is not a valid object name", wire, logical)
		}
		if strings.Contains(wire, "@") {
			t.Fatalf("wire name still has @: %q", wire)
		}
		if got := cacheNameFromObject(wire); got != logical {
			t.Fatalf("round-trip %q -> %q -> %q", logical, wire, got)
		}
	}
	if got := cacheObjectName("native_zlib-1.3.1-deadbeef.tar.gz"); got != "native_zlib-1.3.1-deadbeef.tar.gz" {
		t.Fatalf("name without @ changed: %q", got)
	}
}

func TestAtSignObjectNameRoundTrip(t *testing.T) {
	enableCache(t)
	f := newFakeCache()
	useFakeCache(t, f)
	c, err := cacheClient()
	if err != nil {
		t.Fatal(err)
	}
	logical := "native_rust@1_91_0-1.91.0-4c3e595e.info.txt"
	wire := "native_rust.at.1_91_0-1.91.0-4c3e595e.info.txt"
	path := filepath.Join(t.TempDir(), "info.txt")
	if err := os.WriteFile(path, []byte("info"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := c.Upload("v0-sbs-test", logical, path); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if _, ok := f.objects[wire]; !ok {
		t.Fatalf("stored %#v", f.objects)
	}
	if _, ok := f.objects[logical]; ok {
		t.Fatal("stored the name that contains @")
	}
	f.mu.Unlock()

	listed, err := c.List("v0-sbs-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Name != logical {
		t.Fatalf("list %#v", listed)
	}
	dest := filepath.Join(t.TempDir(), "out.txt")
	if err := c.Download("v0-sbs-test", logical, dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "info" {
		t.Fatalf("body %q", got)
	}
	if err := c.Keep("v0-sbs-test", []string{logical}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.keeps) != 1 || len(f.keeps[0]) != 1 || f.keeps[0][0] != wire {
		t.Fatalf("keep %#v", f.keeps)
	}
}

func TestCachePullDownloadsOnlyNeededAssets(t *testing.T) {
	enableCache(t)
	pkg, err := FindPackage("zlib")
	if err != nil {
		t.Fatal(err)
	}
	h := host.SupportedHosts["x86_64-linux-gnu"]
	full := neededAssetNames([]*Package{pkg}, h)
	if len(full) < 6 {
		t.Fatalf("expected a non-trivial zlib tree, got %d assets", len(full))
	}
	f := newFakeCache()
	for _, name := range full {
		f.objects[name] = []byte("remote-" + name)
	}
	for i := 0; i < 50; i++ {
		f.objects[fmt.Sprintf("unrelated-pkg-%d-deadbeef.tar.gz", i)] = []byte("x")
	}
	var paths [3]string
	var names [3]string
	rels := pkg.BuiltRelPaths(h)
	for i, rel := range rels {
		names[i] = AssetNameForRel(rel)
		paths[i] = filepath.Join(t.TempDir(), filepath.Base(rel))
		if err := os.WriteFile(paths[i], f.objects[names[i]], 0644); err != nil {
			t.Fatal(err)
		}
	}
	sigName := AssetNameForRel(pkg.signatureRel(h))
	sigPath := filepath.Join(t.TempDir(), "pkg.sig.v1.txt")
	if err := writePackSignatureFiles(paths, names, sigPath); err != nil {
		t.Fatal(err)
	}
	sig, err := os.ReadFile(sigPath)
	if err != nil {
		t.Fatal(err)
	}
	f.objects[sigName] = sig
	useFakeCache(t, f)

	if err := CachePull([]*Package{pkg}, h); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	downloads := append([]string{}, f.downloads...)
	keeps := append([][]string{}, f.keeps...)
	f.mu.Unlock()
	got := map[string]bool{}
	for _, name := range downloads {
		if strings.Contains(name, "unrelated-pkg-") {
			t.Fatalf("downloaded unrelated %s", name)
		}
		got[name] = true
	}
	for _, name := range names {
		if !got[name] {
			t.Fatalf("missing download %s in %v", name, downloads)
		}
	}
	if !got[sigName] {
		t.Fatalf("missing signature download in %v", downloads)
	}
	if len(got) != 4 {
		t.Fatalf("downloaded %d names, want 4: %v", len(got), downloads)
	}
	if len(keeps) == 0 {
		t.Fatal("expected keep after pull")
	}

	resetRemoteAssetsCache()
	f.mu.Lock()
	f.downloads = nil
	f.mu.Unlock()
	if err := CachePull([]*Package{pkg}, h); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.downloads) != 0 {
		t.Fatalf("second pull downloaded %v", f.downloads)
	}
}

func TestPullReplacesUnknownSigner(t *testing.T) {
	enableCache(t)
	pkg, err := FindPackage("native/zlib")
	if err != nil {
		t.Fatal(err)
	}
	h := host.SupportedHosts["x86_64-linux-gnu"]
	rels := pkg.BuiltRelPaths(h)
	var paths [3]string
	var names [3]string
	dir := t.TempDir()
	for i, rel := range rels {
		names[i] = AssetNameForRel(rel)
		paths[i] = filepath.Join(dir, filepath.Base(rel))
		if err := os.WriteFile(paths[i], []byte("signed-by-other-"+rel), 0644); err != nil {
			t.Fatal(err)
		}
	}
	sigPath := filepath.Join(dir, "pkg.sig.v1.txt")
	if err := writePackSignatureFiles(paths, names, sigPath); err != nil {
		t.Fatal(err)
	}
	oldSig, err := os.ReadFile(sigPath)
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeCache()
	for i, name := range names {
		body, err := os.ReadFile(paths[i])
		if err != nil {
			t.Fatal(err)
		}
		f.objects[name] = body
	}
	sigName := AssetNameForRel(pkg.signatureRel(h))
	f.objects[sigName] = oldSig
	useFakeCache(t, f)

	other, err := armorPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "simplybs-ci-key.txt"), []byte(other), 0600); err != nil {
		t.Fatal(err)
	}

	if err := CachePull([]*Package{pkg}, h); err != nil {
		t.Fatal(err)
	}
	for _, rel := range rels {
		if _, err := os.Stat(filepath.Join(host.BuiltDir(), filepath.FromSlash(rel))); err == nil {
			t.Fatalf("installed untrusted artifact %s", rel)
		}
	}
	idx, err := loadRemoteAssets()
	if err != nil {
		t.Fatal(err)
	}
	if idx.has(sigName) {
		t.Fatal("unknown signature stayed in the index")
	}

	for _, rel := range rels {
		path := filepath.Join(host.BuiltDir(), filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("rebuilt-"+rel), 0644); err != nil {
			t.Fatal(err)
		}
	}
	TryPushPackageCache(pkg, h)

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, name := range names {
		if !bytes.HasPrefix(f.objects[name], []byte("rebuilt-")) {
			t.Fatalf("object %s was not replaced: %q", name, f.objects[name])
		}
	}
	if bytes.Equal(f.objects[sigName], oldSig) {
		t.Fatal("signature was not replaced")
	}
}

func armorPrivateKey() (string, error) {
	cfg := &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, DefaultHash: crypto.SHA512}
	entity, err := openpgp.NewEntity("simplybs", "cache signing", "simplybs@localhost", cfg)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	if err != nil {
		return "", err
	}
	if err := entity.SerializePrivate(w, nil); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func TestTryPushPackageCacheUploadsOnlyMissing(t *testing.T) {
	enableCache(t)
	pkg, err := FindPackage("native/zlib")
	if err != nil {
		t.Fatal(err)
	}
	h := host.SupportedHosts["x86_64-linux-gnu"]
	rels := pkg.BuiltRelPaths(h)
	for _, rel := range rels {
		path := filepath.Join(host.BuiltDir(), filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("payload-"+rel), 0644); err != nil {
			t.Fatal(err)
		}
	}
	f := newFakeCache()
	already := AssetNameForRel(rels[0])
	f.objects[already] = []byte("already")
	useFakeCache(t, f)
	TryPushPackageCache(pkg, h)

	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objects[already]; !ok {
		t.Fatal("existing object disappeared")
	}
	if bytes.Equal(f.objects[already], []byte("already")) == false {
		t.Fatal("re-uploaded existing object")
	}
	for _, rel := range rels[1:] {
		name := AssetNameForRel(rel)
		if _, ok := f.objects[name]; !ok {
			t.Fatalf("missing upload %s", name)
		}
	}
	sigName := AssetNameForRel(pkg.signatureRel(h))
	if _, ok := f.objects[sigName]; !ok {
		t.Fatal("missing signature upload")
	}
	if len(f.keeps) == 0 {
		t.Fatal("expected keep")
	}
	resetRemoteAssetsCache()
	before := len(f.objects)
	f.mu.Unlock()
	TryPushPackageCache(pkg, h)
	f.mu.Lock()
	if len(f.objects) != before {
		t.Fatalf("second push added objects: %d -> %d", before, len(f.objects))
	}
}

func TestCachePushKeepsAllPackagesInOneRequest(t *testing.T) {
	enableCache(t)
	h := host.SupportedHosts["x86_64-linux-gnu"]
	var pkgs []*Package
	var want []string
	for _, name := range []string{"native/zlib", "native/bzip2"} {
		pkg, err := FindPackage(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, rel := range pkg.BuiltRelPaths(h) {
			path := filepath.Join(host.BuiltDir(), filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("payload-"+rel), 0644); err != nil {
				t.Fatal(err)
			}
		}
		pkgs = append(pkgs, pkg)
		rels := append(append([]string{}, pkg.BuiltRelPaths(h)...), pkg.signatureRel(h))
		for _, rel := range rels {
			want = append(want, cacheObjectName(AssetNameForRel(rel)))
		}
	}
	f := newFakeCache()
	useFakeCache(t, f)
	if err := CachePush(pkgs, h); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.keeps) != 1 {
		t.Fatalf("keep requests %d, want 1", len(f.keeps))
	}
	got := map[string]bool{}
	for _, name := range f.keeps[0] {
		got[name] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Fatalf("keep missing %s", name)
		}
	}
}

func TestTryPushDisabledWithoutToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SIMPLYBS_TOKEN", "")
	t.Setenv("SIMPLYBS_CACHE_TAG", "")
	TryPushPackageCache(&Package{Package: "zlib", Version: "1.0"}, host.SupportedHosts["x86_64-linux-gnu"])
}

func TestPackageCached(t *testing.T) {
	enableCache(t)
	pkg, err := FindPackage("native/zlib")
	if err != nil {
		t.Fatal(err)
	}
	h := host.SupportedHosts["x86_64-linux-gnu"]
	f := newFakeCache()
	for _, rel := range pkg.BuiltRelPaths(h) {
		f.objects[AssetNameForRel(rel)] = []byte("x")
	}
	useFakeCache(t, f)
	ok, err := PackageCached(pkg, h)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected cache hit")
	}
}
