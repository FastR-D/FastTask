package application

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

func deriveSecretKey(secret string) []byte {
	key := sha256.Sum256([]byte("fasttask:model-provider:v1:" + secret))
	return key[:]
}

func (a *ProviderService) encryptSecret(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if a.secretKey == nil {
		return "", errors.New("model provider secret key is not configured")
	}
	gcm, err := a.secretGCM()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(value), nil))
	return "aesgcm.v1:" + encoded, nil
}

func (a *ProviderService) decryptSecret(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if a.secretKey == nil {
		return "", errors.New("model provider secret key is not configured")
	}
	encoded, ok := strings.CutPrefix(value, "aesgcm.v1:")
	if !ok {
		return "", errors.New("model provider secret has an unsupported format")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	gcm, err := a.secretGCM()
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("model provider secret is truncated")
	}
	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt model provider secret: %w", err)
	}
	return string(plain), nil
}

func (a *ProviderService) secretGCM() (cipher.AEAD, error) {
	block, err := aes.NewCipher(a.secretKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
