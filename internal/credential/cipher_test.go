package credential

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"testing"
)

func TestLegacyCiphertextCompatibility(t *testing.T) {
	master := bytes.Repeat([]byte{7}, 32)
	c, err := New(master)
	if err != nil {
		t.Fatal(err)
	}
	// The pre-extraction format is base64(nonce || GCM ciphertext || tag).
	block, err := aes.NewCipher(master)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := bytes.Repeat([]byte{3}, legacy.NonceSize())
	encoded := base64.StdEncoding.EncodeToString(legacy.Seal(nonce, nonce, []byte("fake-secret"), nil))
	if plain, err := c.Decrypt(encoded); err != nil || plain != "fake-secret" {
		t.Fatal("legacy format changed")
	}
	first, err := c.Encrypt("fake-secret")
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Encrypt("fake-secret")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("nonce reused")
	}
	decoded, err := base64.StdEncoding.DecodeString(first)
	if err != nil {
		t.Fatal(err)
	}
	n := legacy.NonceSize()
	if plain, err := legacy.Open(nil, decoded[:n], decoded[n:], nil); err != nil || string(plain) != "fake-secret" {
		t.Fatal("new ciphertext not compatible with legacy reader")
	}
	decoded[len(decoded)-1] ^= 1
	if _, err := c.Decrypt(base64.StdEncoding.EncodeToString(decoded)); err == nil {
		t.Fatal("tampering accepted")
	}
	for _, value := range []string{"not-base64", "", "AA==", base64.StdEncoding.EncodeToString(make([]byte, 12))} {
		if _, err := c.Decrypt(value); err == nil {
			t.Fatal("invalid ciphertext accepted")
		}
	}
	other, _ := New(bytes.Repeat([]byte{8}, 32))
	if _, err := other.Decrypt(first); err == nil {
		t.Fatal("wrong key accepted")
	}
	if _, err := New([]byte("bad")); err == nil {
		t.Fatal("bad master accepted")
	}
}
