// Package config owns configuration and machine identity.
//
// Everything lives under ~/.shale — config, machine identity, the local store,
// and sync watermarks. This is a deliberate departure from the Observatory
// agent, which read ~/.claude/observatory.json so it could co-tenant with the
// JavaScript collector. shale is standalone, and during the porting period the
// JS collector keeps running against its own file: separate paths mean the two
// coexist without either clobbering the other's keys.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nonlinear-xyz/shale/internal/buildinfo"
)

const (
	stateDirName   = ".shale"
	configFileName = "config.json"
	machineFile    = "machine.json"
)

// Config is the subset of ~/.shale/config.json this binary reads. Unknown keys
// are ignored on read and preserved on write, so a future field added by a newer
// build is not destroyed by an older one.
type Config struct {
	URL    string `json:"url"`
	APIKey string `json:"apiKey"`
}

// Machine is this installation's identity, created once at first run.
//
// The ID is random, deliberately NOT the hostname. Hostnames collide across a
// team ("MacBook-Pro" appears three times in any org) and they leak —
// "nishu-macbook" showing up in a shared team graph is an identity disclosure
// nobody asked for. The hostname is kept only as a human-readable label the user
// can see and change without re-keying the machine.
type Machine struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Platform string `json:"platform"`
	Created  string `json:"created"`
}

// StateDirPath returns ~/.shale without creating it. Read-only previews use this
// to uphold their promise not to touch persistent state.
func StateDirPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, stateDirName), nil
}

// StateDir is ~/.shale, created on demand with owner-only permissions.
func StateDir() (string, error) {
	dir, err := StateDirPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("cannot create %s: %w", dir, err)
	}
	return dir, nil
}

// Path is ~/.shale/config.json.
func Path() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, configFileName), nil
}

// Load reads config, applying environment overrides. A missing file is not an
// error: local-only commands (repos, watch --local) work with no config at all,
// and commands that need credentials check Complete() and say so with an
// actionable message.
func Load() (Config, error) {
	var c Config

	path, err := Path()
	if err != nil {
		return c, err
	}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &c); err != nil {
			return c, fmt.Errorf("%s is not valid JSON: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return c, fmt.Errorf("cannot read %s: %w", path, err)
	}

	if v := os.Getenv("SHALE_URL"); v != "" {
		c.URL = v
	}
	if v := os.Getenv("SHALE_API_KEY"); v != "" {
		c.APIKey = v
	}

	c.URL = strings.TrimRight(strings.TrimSpace(c.URL), "/")
	c.APIKey = strings.TrimSpace(c.APIKey)
	return c, nil
}

// Complete reports whether we have everything needed to talk to a hub.
func (c Config) Complete() bool { return c.URL != "" && c.APIKey != "" }

// SaveCredentials merges url/apiKey into the config file, preserving every other
// key. Nothing else writes this file today, but a wholesale rewrite would make
// adding one later a silent data-loss bug — merge is the behavior that stays
// correct as the file grows.
func SaveCredentials(url, apiKey string) error {
	path, err := Path()
	if err != nil {
		return err
	}

	existing := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		// A corrupt file is left alone rather than overwritten — better to fail
		// than to destroy settings we cannot parse.
		if err := json.Unmarshal(raw, &existing); err != nil {
			return fmt.Errorf("%s is not valid JSON — fix or remove it before running setup: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("cannot read %s: %w", path, err)
	}

	existing["url"] = strings.TrimRight(url, "/")
	existing["apiKey"] = apiKey

	return WriteJSONFile(path, existing, 0o600)
}

// LoadOrCreateMachine returns this machine's identity, generating it on first
// run. Callers can rely on the ID being stable for the life of the install.
func LoadOrCreateMachine() (Machine, error) {
	dir, err := StateDir()
	if err != nil {
		return Machine{}, err
	}
	path := filepath.Join(dir, machineFile)

	if raw, err := os.ReadFile(path); err == nil {
		var m Machine
		if err := json.Unmarshal(raw, &m); err == nil && m.ID != "" {
			return m, nil
		}
		// Unparseable or ID-less — fall through and mint a new identity rather
		// than failing. A new machine row is recoverable; a hard stop is not.
	} else if !os.IsNotExist(err) {
		return Machine{}, fmt.Errorf("cannot read %s: %w", path, err)
	}

	m := Machine{
		ID:       RandomID(),
		Label:    hostnameLabel(),
		Platform: buildinfo.PlatformLabel(),
		Created:  nowRFC3339(),
	}
	if err := WriteJSONFile(path, m, 0o600); err != nil {
		return Machine{}, err
	}
	return m, nil
}

// RandomID is 128 bits of randomness, hex-encoded — 32 hex characters, which is
// also the shape the hub's machineId schema validates against.
//
// It is exported because the authorization flow uses it for the OAuth-style
// `state` parameter as well. crypto/rand failure is fatal: a predictable or
// empty machine ID would silently merge two machines' data server-side.
func RandomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("cannot generate random id: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// ShortID is the first 8 hex characters — enough to recognize a machine in the
// UI without printing a full identifier at every prompt.
func ShortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func hostnameLabel() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// WriteJSONFile writes atomically — a crash mid-write would otherwise leave a
// truncated config or, worse, a machine file with no ID.
func WriteJSONFile(path string, v any, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("cannot create %s: %w", filepath.Dir(path), err)
	}

	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode %s: %w", path, err)
	}
	data = append(data, '\n')

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return fmt.Errorf("cannot write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("cannot replace %s: %w", path, err)
	}
	return nil
}
