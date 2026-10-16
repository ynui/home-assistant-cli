package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Config holds the connection details for a single Home Assistant instance.
// It contains a bearer token that grants full control of the house, so the
// file is written 0600 inside a 0700 directory.
type Config struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// configDir is ~/.config/has on every platform, or the directory holding an
// overridden HAS_CONFIG. Deliberately not os.UserConfigDir(): that would land
// in ~/Library/Application Support on macOS.
func configDir() (string, error) {
	if p := os.Getenv("HAS_CONFIG"); p != "" {
		return filepath.Dir(p), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "has"), nil
}

// configPath is ~/.config/has/config.json, overridable with HAS_CONFIG.
func configPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// loadConfig reads the saved config. A missing file is not an error — it just
// means `has login` has not run yet.
func loadConfig() (Config, error) {
	var c Config
	p, err := configPath()
	if err != nil {
		return c, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return c, nil
	}
	return c, json.Unmarshal(b, &c)
}

func saveConfig(c Config) error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// Write atomically so a crash or a full disk never leaves a half-written
	// bearer-token file behind. WriteFile only applies the mode on creation,
	// so the temp file is chmod'd explicitly.
	if err := atomicWriteFile(p, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Chmod(p, 0o600)
}

// configured builds a Client from, in increasing order of precedence:
// the config file, HAS_URL/HAS_TOKEN, and the --url/--token flags.
func configured() (*Client, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	if v := os.Getenv("HAS_URL"); v != "" {
		cfg.URL = v
	}
	if v := os.Getenv("HAS_TOKEN"); v != "" {
		cfg.Token = v
	}
	if *flagURL != "" {
		cfg.URL = *flagURL
	}
	if *flagToken != "" {
		cfg.Token = *flagToken
	}
	return newClient(cfg, *flagTimeout)
}

// atomicWriteFile writes data to path via a temp file in the same directory
// followed by a rename, so readers never see a partially written file. The
// mode is applied to the temp file before the rename and again afterwards, since
// rename preserves the temp file's mode but an existing destination is replaced.
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".has-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() {
		f.Close()
		os.Remove(tmp)
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Chmod(path, mode)
}
