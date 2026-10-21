package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// BlobStore keeps immutable snapshots on the local filesystem, addressed by
// the SHA-256 of their content (ab/cd/abcd…). Identical content is stored once.
type BlobStore struct {
	Root string
}

// Hash returns the hex SHA-256 of content.
func Hash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// RelPath is the path of a blob relative to Root.
func RelPath(hash string) string {
	return filepath.Join(hash[0:2], hash[2:4], hash)
}

// Put stores content and returns its hash and relative path. Writes are
// atomic (temp file + rename), so readers never see a partial snapshot.
func (b *BlobStore) Put(content []byte) (hash, rel string, err error) {
	hash = Hash(content)
	rel = RelPath(hash)
	full := filepath.Join(b.Root, rel)
	if _, err := os.Stat(full); err == nil {
		return hash, rel, nil
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".tmp-*")
	if err != nil {
		return "", "", err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // no-op after a successful rename
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		return "", "", err
	}
	if err := os.Rename(tmp.Name(), full); err != nil {
		return "", "", err
	}
	return hash, rel, nil
}

// Get reads a blob and checks it against its content address.
func (b *BlobStore) Get(rel string) ([]byte, error) {
	content, err := os.ReadFile(filepath.Join(b.Root, rel))
	if err != nil {
		return nil, err
	}
	if want := filepath.Base(rel); Hash(content) != want {
		return nil, fmt.Errorf("blob %s: content hash mismatch", rel)
	}
	return content, nil
}
