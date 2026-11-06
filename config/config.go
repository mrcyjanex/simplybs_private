package config

import (
	"bytes"
	"crypto"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/mrcyjanek/simplybs/utils/uniparse"
)

// CacheURL is the sbscache API. It is not configurable.
const CacheURL = "https://sbs.mrcyjanek.net"

// ciKeyFile is an armored PGP private key at $HOME/simplybs-ci-key.txt.
// CI writes the SIMPLYBS_PGP_KEY secret there so every runner signs with the
// same key. When the file exists it replaces any key stored in the config.
const ciKeyFile = "simplybs-ci-key.txt"

// File is $HOME/.config/simplybs/config.json.
type File struct {
	Token         string `json:"token,omitempty"`
	Tag           string `json:"tag,omitempty"`
	PGPPrivateKey string `json:"pgpPrivateKey,omitempty"`
}

var loadMu sync.Mutex

// Path returns the config file path for the current user.
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "simplybs", "config.json"), nil
}

// Load reads the config file, applies SIMPLYBS_TOKEN when the file has no
// token, and generates an unencrypted PGP key when one is not stored.
func Load() (File, error) {
	loadMu.Lock()
	defer loadMu.Unlock()
	return loadLocked()
}

func loadLocked() (File, error) {
	path, err := Path()
	if err != nil {
		return File{}, err
	}
	var f File
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return File{}, err
	}
	if err == nil && len(bytes.TrimSpace(data)) > 0 {
		if err := uniparse.Unmarshal(data, &f); err != nil {
			return File{}, fmt.Errorf("config: %s: %w", path, err)
		}
	}
	changed := false
	if f.Token == "" {
		if tok := os.Getenv("SIMPLYBS_TOKEN"); tok != "" {
			f.Token = tok
			changed = true
		}
	}
	ciKey, ciKeySet, err := readCIKey()
	if err != nil {
		return File{}, err
	}
	if ciKeySet {
		if f.PGPPrivateKey != ciKey {
			f.PGPPrivateKey = ciKey
			changed = true
		}
	} else if f.PGPPrivateKey == "" {
		armored, err := generatePrivateKey()
		if err != nil {
			return File{}, err
		}
		f.PGPPrivateKey = armored
		changed = true
	}
	if changed {
		if err := saveLocked(path, f); err != nil {
			return File{}, err
		}
	}
	return f, nil
}

func saveLocked(path string, f File) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0600)
}

// Token returns SIMPLYBS_TOKEN when set, otherwise the token stored in the
// config file.
func Token() (string, error) {
	if tok := os.Getenv("SIMPLYBS_TOKEN"); tok != "" {
		if _, err := Load(); err != nil {
			return "", err
		}
		return tok, nil
	}
	f, err := Load()
	if err != nil {
		return "", err
	}
	return f.Token, nil
}

// Tag returns SIMPLYBS_CACHE_TAG when set, otherwise the tag in the config
// file, otherwise v0-sbs-$USER-$GOOS-$GOARCH.
func Tag() (string, error) {
	if tag := os.Getenv("SIMPLYBS_CACHE_TAG"); tag != "" {
		return tag, nil
	}
	f, err := Load()
	if err != nil {
		return "", err
	}
	if f.Tag != "" {
		return f.Tag, nil
	}
	return defaultTag(), nil
}

// PrivateKey returns the signing entity stored in the config file.
func PrivateKey() (*openpgp.Entity, error) {
	f, err := Load()
	if err != nil {
		return nil, err
	}
	ents, err := openpgp.ReadArmoredKeyRing(bytes.NewReader([]byte(f.PGPPrivateKey)))
	if err != nil {
		return nil, fmt.Errorf("config: pgp key: %w", err)
	}
	if len(ents) == 0 || ents[0].PrivateKey == nil {
		return nil, fmt.Errorf("config: pgp key is empty")
	}
	return ents[0], nil
}

func defaultTag() string {
	name := os.Getenv("USER")
	if name == "" {
		if u, err := user.Current(); err == nil && u.Username != "" {
			name = u.Username
		} else {
			name = "user"
		}
	}
	return fmt.Sprintf("v0-sbs-%s-%s-%s", name, runtime.GOOS, runtime.GOARCH)
}

func readCIKey() (armored string, ok bool, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, err
	}
	path := filepath.Join(home, ciKeyFile)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	armored = strings.TrimSpace(string(data))
	if armored == "" {
		return "", false, fmt.Errorf("config: %s is empty", path)
	}
	if err := validatePrivateKey(armored); err != nil {
		return "", false, fmt.Errorf("config: %s: %w", path, err)
	}
	return armored, true, nil
}

func validatePrivateKey(armored string) error {
	ents, err := openpgp.ReadArmoredKeyRing(bytes.NewReader([]byte(armored)))
	if err != nil {
		return err
	}
	if len(ents) == 0 || ents[0].PrivateKey == nil {
		return fmt.Errorf("not a private key")
	}
	return nil
}

func generatePrivateKey() (string, error) {
	cfg := &packet.Config{
		Algorithm:   packet.PubKeyAlgoEdDSA,
		DefaultHash: crypto.SHA512,
	}
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
