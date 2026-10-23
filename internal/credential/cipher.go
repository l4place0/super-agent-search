// Package credential owns the on-disk AES-GCM key format shared by the server
// and read-only diagnostic utility. It performs no I/O and never logs material.
package credential

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
)

type Cipher struct{ aead cipher.AEAD }

func New(master []byte) (*Cipher, error) {
	block, err := aes.NewCipher(master)
	if err != nil {
		return nil, errors.New("invalid master key")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}
func (c *Cipher) Encrypt(secret string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", errors.New("key encryption failed")
	}
	return base64.StdEncoding.EncodeToString(c.aead.Seal(nonce, nonce, []byte(secret), nil)), nil
}
func (c *Cipher) Decrypt(encrypted string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil || len(b) < c.aead.NonceSize() {
		return "", errors.New("invalid encrypted key")
	}
	n := c.aead.NonceSize()
	plain, err := c.aead.Open(nil, b[:n], b[n:], nil)
	if err != nil {
		return "", errors.New("invalid encrypted key")
	}
	return string(plain), nil
}
