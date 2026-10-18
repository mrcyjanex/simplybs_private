package host

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mrcyjanek/simplybs/crash"
)

// RepoCloneURL is the simplybs checkout installed by `packages sync`.
const RepoCloneURL = "https://github.com/mrcyjanex/simplybs_private"

const packagesMarker = "_.yaml"

func resolvePackagesDir() string {
	if dir := checkoutPackagesDir(); dir != "" {
		return dir
	}
	dir := installedPackagesDir()
	marker := filepath.Join(dir, "native", packagesMarker)
	if _, err := os.Stat(marker); err != nil {
		crash.Handle(fmt.Errorf("packages not found at %s; run: simplybs packages sync", dir))
	}
	return dir
}

func checkoutPackagesDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if dir := findPackagesDir(wd); dir != "" {
		return dir
	}
	if !runningUnderGoRun() {
		return ""
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok || !filepath.IsAbs(file) {
		return ""
	}
	return findPackagesDir(filepath.Dir(file))
}

func runningUnderGoRun() bool {
	return strings.Contains(os.Args[0], "go-build")
}

// findPackagesDir walks up from start and returns the packages directory
// whose native/_.yaml marker is present.
func findPackagesDir(start string) string {
	dir := start
	for {
		marker := filepath.Join(dir, "packages", "native", packagesMarker)
		if st, err := os.Stat(marker); err == nil && !st.IsDir() {
			return filepath.Join(dir, "packages")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func installedRepoDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(string(os.PathSeparator), "tmp", "simplybs", "repo")
	}
	return filepath.Join(home, ".local", "share", "simplybs", "repo")
}

func installedPackagesDir() string {
	return filepath.Join(installedRepoDir(), "packages")
}

// SyncPackages clones simplybs into ~/.local/share/simplybs/repo, or
// fast-forwards that clone. A checkout and `go run` keep reading their own
// packages/ tree.
func SyncPackages() error {
	dest := installedRepoDir()
	gitDir := filepath.Join(dest, ".git")
	if st, err := os.Stat(gitDir); err != nil || !st.IsDir() {
		if _, statErr := os.Stat(dest); statErr == nil {
			return fmt.Errorf("%s exists but is not a git clone", dest)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		return runGit("clone", "--depth", "1", RepoCloneURL, dest)
	}
	return runGit("-C", dest, "pull", "--ff-only")
}

func runGit(args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}
