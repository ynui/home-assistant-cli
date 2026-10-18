package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Config holds the connection details. The token grants full control of the
// house, so the file is written 0600 inside a 0700 directory.
type Config struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// configDir is ~/.config/has, or the directory of an overridden HAS_CONFIG.
// Deliberately not os.UserConfigDir(): that lands in ~/Library/Application
// Support on macOS.
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
	// Write atomically, so a crash never leaves a half-written bearer-token
	// file. WriteFile only applies the mode on creation, hence the temp-file
	// Chmod plus the one after rename.
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

// atomicWriteFile writes data to path via a temp file in the same directory,
// followed by a rename, so readers never see a partially written file. The
// mode is applied before and after the rename.
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
