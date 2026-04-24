// Package hostkey loads the proxy's persistent SSH host key, generating a new
// ed25519 key when the file is absent. Keys must be stable across restarts
// so returning users do not see host-key-changed warnings.
package hostkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	"golang.org/x/crypto/ssh"
)

// LoadOrGenerate reads a PEM-encoded PKCS#8 ed25519 private key from path.
// If the file is absent, a fresh key is generated, written with 0600 perms,
// and returned. Any existing file that cannot be parsed is an error — we
// never silently overwrite a user key.
func LoadOrGenerate(path string) (ssh.Signer, error) {
	if raw, err := os.ReadFile(path); err == nil {
		return parsePEM(raw)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read host key %s: %w", path, err)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519: %w", err)
	}
	pemBytes, err := marshalPEM(priv)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write host key %s: %w", path, err)
	}
	return ssh.NewSignerFromKey(priv)
}

func parsePEM(raw []byte) (ssh.Signer, error) {
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("parse host key: %w", err)
	}
	return signer, nil
}

func marshalPEM(priv ed25519.PrivateKey) ([]byte, error) {
	// ssh.MarshalPrivateKey returns a *pem.Block; we need the bytes.
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}
	return pem.EncodeToMemory(blk), nil
}
