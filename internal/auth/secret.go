package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
)

func LoadOrCreateSecret(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, "secret.key")
	b, err := os.ReadFile(path)
	if err == nil {
		return hex.DecodeString(string(b))
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	hexed := []byte(hex.EncodeToString(raw))
	if err := os.WriteFile(path, hexed, 0o600); err != nil {
		return nil, err
	}
	return raw, nil
}

func aesGCM(key []byte) (cipher.AEAD, error) {
	k := sha256.Sum256(key) // 规整到 32 字节
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func Encrypt(key, plain []byte) ([]byte, error) {
	gcm, err := aesGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

func Decrypt(key, cipherText []byte) ([]byte, error) {
	gcm, err := aesGCM(key)
	if err != nil {
		return nil, err
	}
	if len(cipherText) < gcm.NonceSize() {
		return nil, errors.New("密文过短")
	}
	return gcm.Open(nil, cipherText[:gcm.NonceSize()], cipherText[gcm.NonceSize():], nil)
}
