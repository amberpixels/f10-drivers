package notion

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zalando/go-keyring"
)

// keyringService names the logins in the OS keychain; the account is the
// server url.
const keyringService = "f10-drivers"

// TokenStoreEnv set to "file" skips the keychain, so tests never touch the
// developer's.
const TokenStoreEnv = "F10_DRIVERS_TOKEN_STORE"

var errNotStored = errors.New("no stored login")

// tokenStore keeps logins per user, never under a project.
type tokenStore interface {
	load(server string) (*storedLogin, error)
	save(l *storedLogin) (string, error) // where it went, for the user
	remove(server string) error
}

func defaultStore() tokenStore {
	file := fileStore{}
	if os.Getenv(TokenStoreEnv) == "file" {
		return file
	}

	return keychainStore{fallback: file}
}

// keychainStore keeps logins in the OS keychain, and in the file where the
// machine has no keychain to use.
type keychainStore struct{ fallback fileStore }

func (k keychainStore) load(server string) (*storedLogin, error) {
	secret, err := keyring.Get(keyringService, server)
	if err != nil {
		return k.fallback.load(server)
	}

	var l storedLogin
	if err := json.Unmarshal([]byte(secret), &l); err != nil {
		return nil, fmt.Errorf("the keychain's %s login is unreadable: %w", keyringService, err)
	}

	return &l, nil
}

func (k keychainStore) save(l *storedLogin) (string, error) {
	secret, err := json.Marshal(l)
	if err != nil {
		return "", err
	}

	if keyring.Set(keyringService, l.Server, string(secret)) != nil {
		where, err := k.fallback.save(l)

		return where + " (no usable keychain)", err
	}

	return "the OS keychain", nil
}

// remove forgets the login in both places: a save may have fallen back.
func (k keychainStore) remove(server string) error {
	_ = keyring.Delete(keyringService, server)

	return k.fallback.remove(server)
}

// fileStore keeps logins in a 0600 file under the user config dir.
type fileStore struct{}

func (fileStore) path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, "f10-drivers", "notion-logins.json"), nil
}

func (f fileStore) all() (map[string]*storedLogin, string, error) {
	path, err := f.path()
	if err != nil {
		return nil, "", err
	}

	logins := map[string]*storedLogin{}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return logins, path, nil
	}

	if err != nil {
		return nil, "", err
	}

	if err := json.Unmarshal(data, &logins); err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}

	return logins, path, nil
}

func (f fileStore) load(server string) (*storedLogin, error) {
	logins, _, err := f.all()
	if err != nil {
		return nil, err
	}

	if l, ok := logins[server]; ok {
		return l, nil
	}

	return nil, errNotStored
}

func (f fileStore) save(l *storedLogin) (string, error) {
	logins, path, err := f.all()
	if err != nil {
		return "", err
	}

	logins[l.Server] = l

	return path, f.write(path, logins)
}

func (f fileStore) remove(server string) error {
	logins, path, err := f.all()
	if err != nil {
		return err
	}

	if _, ok := logins[server]; !ok {
		return nil
	}

	delete(logins, server)

	return f.write(path, logins)
}

// write replaces the file through a temp file, so a failed write never
// leaves half a login behind.
func (fileStore) write(path string, logins map[string]*storedLogin) error {
	data, err := json.MarshalIndent(logins, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".notion-logins-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), path)
}
