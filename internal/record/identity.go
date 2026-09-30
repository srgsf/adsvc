package record

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Identity is this node's signing key. The private half never enters the catalogue: it
// lives in its own file, readable by the owner only.
type Identity struct {
	Origin Origin
	key    ed25519.PrivateKey
}

// NewIdentity is a fresh random identity.
func NewIdentity() *Identity {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err) // crypto/rand does not fail
	}
	return identityOf(key)
}

func identityOf(key ed25519.PrivateKey) *Identity {
	id := &Identity{key: key}
	copy(id.Origin[:], key.Public().(ed25519.PublicKey))
	return id
}

const identityHeader = "adsvc ed25519 seed "

// LoadIdentity reads the identity at path, creating it (mode 0600) when there is none.
func LoadIdentity(path string) (*Identity, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return createIdentity(path)
	}
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	seed, err := hex.DecodeString(string(bytes.TrimSpace(bytes.TrimPrefix(b, []byte(identityHeader)))))
	if !bytes.HasPrefix(b, []byte(identityHeader)) || err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("identity %s: not an adsvc identity file", path)
	}
	return identityOf(ed25519.NewKeyFromSeed(seed)), nil
}

func createIdentity(path string) (*Identity, error) {
	id := NewIdentity()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	_, err = fmt.Fprintf(f, "%s%s\n", identityHeader, hex.EncodeToString(id.key.Seed()))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("identity: %w", err)
	}
	return id, nil
}

// SignBytes signs b (requests between peers, see package replica).
func (id *Identity) SignBytes(b []byte) []byte { return ed25519.Sign(id.key, b) }

// VerifyBytes reports whether sig is o's signature of b.
func VerifyBytes(o Origin, b, sig []byte) bool { return ed25519.Verify(o[:], b, sig) }
