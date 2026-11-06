package pack

import (
	"strings"
	"testing"

	"github.com/mrcyjanek/simplybs/host"
)

func TestSelfAppendKeys(t *testing.T) {
	skip := selfAppendKeys([]string{
		"CFLAGS=$CFLAGS -I/own",
		"CXXFLAGS=${CXXFLAGS} -fPIC",
		"CC_FOR_BUILD=$CC",
		"CC_BUILD=$CC $CFLAGS",
		"PKG_CONFIG_PATH=$NATIVEPREFIX/lib/pkgconfig",
		"FOO=$CFLAGS_EXTRA",
	})
	for _, key := range []string{"CFLAGS", "CXXFLAGS"} {
		if !skip[key] {
			t.Errorf("%s should be a self-append", key)
		}
	}
	for _, key := range []string{"CC_FOR_BUILD", "CC_BUILD", "PKG_CONFIG_PATH", "FOO", "CC", "CFLAGS_EXTRA"} {
		if skip[key] {
			t.Errorf("%s should still come from dependencies", key)
		}
	}
}

func TestToolchainExportKeepsBootstrapCompilerWithoutBootstrapFlags(t *testing.T) {
	h := host.SupportedHosts["aarch64-linux-android"]
	bootstrap, err := FindPackage("native/_/_")
	if err != nil {
		t.Fatal(err)
	}
	toolchain, err := FindPackage("native/_")
	if err != nil {
		t.Fatal(err)
	}
	boot := bootstrap.GetExportEnv(h)
	got := toolchain.GetExportEnv(h)
	if got["CC_FOR_BUILD"] == "" || got["CC_FOR_BUILD"] != boot["CC"] {
		t.Fatalf("CC_FOR_BUILD=%q, bootstrap CC=%q", got["CC_FOR_BUILD"], boot["CC"])
	}
	if cflags := strings.TrimSpace(boot["CFLAGS"]); cflags != "" && strings.Contains(got["CFLAGS"], cflags) {
		t.Fatalf("CFLAGS absorbed bootstrap flags %q into %q", cflags, got["CFLAGS"])
	}
	if lib := boot["LIBRARY_PATH"]; lib != "" && strings.Contains(got["LIBRARY_PATH"], lib) {
		t.Fatalf("LIBRARY_PATH absorbed bootstrap path %q into %q", lib, got["LIBRARY_PATH"])
	}
}
