package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenAndTagPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USER", "tester")
	t.Setenv("SIMPLYBS_TOKEN", "")
	t.Setenv("SIMPLYBS_CACHE_TAG", "")

	f, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if f.PGPPrivateKey == "" || !strings.Contains(f.PGPPrivateKey, "BEGIN PGP PRIVATE KEY BLOCK") {
		t.Fatalf("expected a private key, got %q", f.PGPPrivateKey)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "simplybs", "config.json")); err != nil {
		t.Fatal(err)
	}
	tag, err := Tag()
	if err != nil {
		t.Fatal(err)
	}
	if tag != "v0-sbs-tester-linux-amd64" && !strings.HasPrefix(tag, "v0-sbs-tester-") {
		t.Fatalf("default tag %q", tag)
	}
	tok, err := Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "" {
		t.Fatalf("token %q", tok)
	}

	t.Setenv("SIMPLYBS_TOKEN", "sbs_testtoken")
	tok, err = Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "sbs_testtoken" {
		t.Fatalf("token %q", tok)
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "simplybs", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "sbs_testtoken") {
		t.Fatalf("token was not saved: %s", data)
	}

	t.Setenv("SIMPLYBS_CACHE_TAG", "v0-sbs-from-env")
	tag, err = Tag()
	if err != nil {
		t.Fatal(err)
	}
	if tag != "v0-sbs-from-env" {
		t.Fatalf("tag %q", tag)
	}

	ent, err := PrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	if ent.PrivateKey == nil {
		t.Fatal("missing private key")
	}
}

func TestTagFromFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SIMPLYBS_TOKEN", "")
	t.Setenv("SIMPLYBS_CACHE_TAG", "")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "simplybs", "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(data), "{", "{\n  \"tag\": \"v0-sbs-file\",\n", 1)
	if err := os.WriteFile(path, []byte(updated), 0600); err != nil {
		t.Fatal(err)
	}
	tag, err := Tag()
	if err != nil {
		t.Fatal(err)
	}
	if tag != "v0-sbs-file" {
		t.Fatalf("tag %q", tag)
	}
}
