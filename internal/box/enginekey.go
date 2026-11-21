package box

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The engine's key, when the engine needs one, reaches the gateway as a file
// in the box's state folder, mounted read-only into the gateway and nowhere
// else. An environment variable would show in plain text in docker inspect,
// which people paste into bug reports. ezbx up writes the file from the
// variable named by key_env, and ezbx down removes it, so the key is on disk
// only while the box's containers exist.

const (
	keyInside = "/run/ezbx/engine-key"
	// MaxKeyLen is the longest key accepted, in bytes.
	MaxKeyLen = 4096
)

// KeyFile is where the engine's key is kept on the host while the box is up.
func (p Plan) KeyFile() string { return KeyFileIn(p.StateDir) }

// KeyFileIn gives the key file of a box's state folder.
func KeyFileIn(stateDir string) string { return filepath.Join(stateDir, "engine-key") }

// CleanKey checks a key and returns it without one trailing line break
// (an editor or echo adds one). It refuses an empty key, one longer than
// MaxKeyLen, and one holding a line break or another control character,
// which could never be a working key and could split the request header.
// Its errors never quote the key.
func CleanKey(s string) (string, error) {
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	if s == "" {
		return "", errors.New("the key is empty")
	}
	if len(s) > MaxKeyLen {
		return "", fmt.Errorf("the key is longer than %d bytes", MaxKeyLen)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("the key holds a line break or another control character")
		}
	}
	return s, nil
}

// WriteKeyFile checks the key and writes it to file, readable only by this
// user. It writes a temporary file in the same folder and moves it into
// place, so the file is never seen half written.
func WriteKeyFile(file, key string) error {
	key, err := CleanKey(key)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".engine-key-*")
	if err != nil {
		return fmt.Errorf("writing the engine key: %w", err)
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("writing the engine key: %w", err)
	}
	if _, err := tmp.WriteString(key); err != nil {
		return fmt.Errorf("writing the engine key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing the engine key: %w", err)
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("writing the engine key: %w", err)
	}
	done = true
	return nil
}

// ReadKeyFile reads and checks a key file, as the gateway does at start.
func ReadKeyFile(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", fmt.Errorf("cannot read the engine key file: %w", err)
	}
	defer f.Close()
	// Two bytes more than the limit leave room for a trailing line break,
	// and one more shows the file is too long.
	data, err := io.ReadAll(io.LimitReader(f, MaxKeyLen+3))
	if err != nil {
		return "", fmt.Errorf("cannot read the engine key file: %w", err)
	}
	key, err := CleanKey(string(data))
	if err != nil {
		return "", fmt.Errorf("the engine key file %s: %w", file, err)
	}
	return key, nil
}

// RemoveKeyFile removes a box's key file, if there is one, and reports
// whether there was.
func RemoveKeyFile(stateDir string) (bool, error) {
	err := os.Remove(KeyFileIn(stateDir))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("removing the engine key: %w", err)
	}
	return true, nil
}
