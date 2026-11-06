package host

import (
	"os"
	"path/filepath"
)

// OptRoot is the install prefix for native and target trees.
const OptRoot = "/opt/_"

type Host struct {
	Triplet string
}

// GetPackagesDir is the checkout packages/ tree when that tree is on disk
// or this process was started with go run. Otherwise it is the packages
// directory inside the ~/.local clone from `simplybs packages sync`.
func GetPackagesDir() string {
	return resolvePackagesDir()
}

func cacheRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(string(os.PathSeparator), "tmp", "simplybs")
	}
	return filepath.Join(home, ".cache", "simplybs")
}

// BuiltDir holds build archives and downloaded cache artifacts.
func BuiltDir() string {
	return filepath.Join(cacheRoot(), "built")
}

// SourcesDir holds downloaded package sources.
func SourcesDir() string {
	return filepath.Join(cacheRoot(), "sources")
}

// WorkDir holds temporary build trees.
func WorkDir() string {
	return filepath.Join(cacheRoot(), "work")
}

// StagingDir holds temporary staged installs.
func StagingDir() string {
	return filepath.Join(cacheRoot(), "staging")
}

func (h *Host) GetNativeEnvPath() string {
	return filepath.Join(OptRoot, "native")
}

func (h *Host) GetEnvPath() string {
	return filepath.Join(OptRoot, h.Triplet)
}

var SupportedHosts = map[string]*Host{
	"aarch64-apple-darwin":        {Triplet: "aarch64-apple-darwin"},
	"x86_64-apple-darwin":         {Triplet: "x86_64-apple-darwin"},
	"aarch64-apple-ios":           {Triplet: "aarch64-apple-ios"},
	"aarch64-apple-ios-simulator": {Triplet: "aarch64-apple-ios-simulator"},
	"x86_64-w64-mingw32":          {Triplet: "x86_64-w64-mingw32"},
	"x86_64-linux-gnu":            {Triplet: "x86_64-linux-gnu"},
	"aarch64-linux-gnu":           {Triplet: "aarch64-linux-gnu"},
	"aarch64-linux-android":       {Triplet: "aarch64-linux-android"},
	"x86_64-linux-android":        {Triplet: "x86_64-linux-android"},
	"armv7a-linux-androideabi":    {Triplet: "armv7a-linux-androideabi"},
}
