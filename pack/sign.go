package pack

import (
	"bytes"
	"crypto"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	sbsconfig "github.com/mrcyjanek/simplybs/config"
	"github.com/mrcyjanek/simplybs/host"
)

const signatureSuffix = ".sig.v1.txt"

func (p *Package) signatureRel(h *host.Host) string {
	rels := p.BuiltRelPaths(h)
	base := strings.TrimSuffix(rels[0], ".info.txt")
	return base + signatureSuffix
}

func (p *Package) signaturePath(h *host.Host) string {
	return p.GenerateBuildPath(h, "built") + signatureSuffix
}

func (p *Package) writePackSignature(h *host.Host) error {
	rels := p.BuiltRelPaths(h)
	root := host.BuiltDir()
	paths := [3]string{}
	names := [3]string{}
	for i, rel := range rels {
		paths[i] = filepath.Join(root, filepath.FromSlash(rel))
		names[i] = AssetNameForRel(rel)
	}
	return writePackSignatureFiles(paths, names, p.signaturePath(h))
}

func (p *Package) verifyLocalSignature(h *host.Host) error {
	sigPath := p.signaturePath(h)
	if _, err := os.Stat(sigPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	rels := p.BuiltRelPaths(h)
	root := host.BuiltDir()
	var paths [3]string
	var names [3]string
	for i, rel := range rels {
		paths[i] = filepath.Join(root, filepath.FromSlash(rel))
		names[i] = AssetNameForRel(rel)
	}
	return verifyPackFiles(paths, names, sigPath)
}

func signBuiltTree(root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".info.txt") {
			return err
		}
		base := strings.TrimSuffix(path, ".info.txt")
		paths := [3]string{path, base + ".tar.gz", base + "_native.tar.gz"}
		for _, p := range paths {
			if _, err := os.Stat(p); err != nil {
				return nil
			}
		}
		sig := base + signatureSuffix
		if _, err := os.Stat(sig); err == nil {
			return nil
		}
		var names [3]string
		for i, p := range paths {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			names[i] = AssetNameForRel(rel)
		}
		return writePackSignatureFiles(paths, names, sig)
	})
}

func writePackSignatureFiles(paths [3]string, names [3]string, sigPath string) error {
	if _, err := os.Stat(sigPath); err == nil {
		return nil
	}
	manifest, err := packManifest(paths, names)
	if err != nil {
		return err
	}
	entity, err := sbsconfig.PrivateKey()
	if err != nil {
		return err
	}
	signed, err := clearsignManifest(entity, manifest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(sigPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(sigPath, signed, 0644)
}

func verifyPackFiles(paths [3]string, names [3]string, sigPath string) error {
	sig, err := os.ReadFile(sigPath)
	if err != nil {
		return fmt.Errorf("signature missing: %w", err)
	}
	manifest, err := packManifest(paths, names)
	if err != nil {
		return err
	}
	entity, err := sbsconfig.PrivateKey()
	if err != nil {
		return err
	}
	if err := verifyClearsign(entity, sig, manifest); err != nil {
		return fmt.Errorf("signature mismatch: %w", err)
	}
	return nil
}

func packManifest(paths [3]string, names [3]string) (string, error) {
	type row struct {
		name string
		sum  string
	}
	rows := make([]row, 3)
	for i := range paths {
		sum, err := hashFile(paths[i])
		if err != nil {
			return "", err
		}
		rows[i] = row{name: names[i], sum: sum}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%s  %s\n", r.sum, r.name)
	}
	return b.String(), nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha512.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func clearsignManifest(entity *openpgp.Entity, manifest string) ([]byte, error) {
	var buf bytes.Buffer
	w, err := clearsign.Encode(&buf, entity.PrivateKey, &packet.Config{DefaultHash: crypto.SHA512})
	if err != nil {
		return nil, err
	}
	if _, err := w.Write([]byte(manifest)); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func verifyClearsign(entity *openpgp.Entity, signed []byte, manifest string) error {
	block, _ := clearsign.Decode(signed)
	if block == nil {
		return fmt.Errorf("not a clearsigned message")
	}
	if _, err := block.VerifySignature(openpgp.EntityList{entity}, &packet.Config{DefaultHash: crypto.SHA512}); err != nil {
		return err
	}
	got := strings.ReplaceAll(string(block.Plaintext), "\r\n", "\n")
	if got != manifest {
		return fmt.Errorf("signed checksums do not match file contents")
	}
	return nil
}
