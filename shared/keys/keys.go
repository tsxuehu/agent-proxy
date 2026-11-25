package keys

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
)

func New(prefix string) string {
	return prefix + rand.Text()
}

func Digest(secret string) [32]byte { return sha256.Sum256([]byte(secret)) }

func Matches(hash []byte, secret string) bool {
	digest := Digest(secret)
	return secret != "" && subtle.ConstantTimeCompare(hash, digest[:]) == 1
}

type Vault struct{ cipher.AEAD }

func NewVault(value string) (Vault, error) {
	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(key) != 32 {
		return Vault{}, errors.New("AGENT_CREDENTIAL_KEY must be a base64-encoded 32-byte key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return Vault{}, err
	}
	gcm, err := cipher.NewGCMWithRandomNonce(block)
	return Vault{gcm}, err
}

func (v Vault) Encrypt(id string, plaintext []byte) []byte {
	return v.Seal(nil, nil, plaintext, []byte(id))
}

func (v Vault) Decrypt(id string, ciphertext []byte) ([]byte, error) {
	return v.Open(nil, nil, ciphertext, []byte(id))
}
