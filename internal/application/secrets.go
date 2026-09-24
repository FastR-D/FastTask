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

// deriveSecretKey derives the model-provider AES-GCM key from the configured secret.
func deriveSecretKey(secret string) []byte {
	return deriveLabeledKey("fasttask:model-provider:v1:", secret)
}

// deriveNotificationKey derives the notification AES-GCM key from the same configured
// secret under its own label. Two labels, one operator secret: a ciphertext written for a
// model provider is then not decryptable as a notification credential, so the two stores
// cannot be mixed up by a bug or by copying a column between tables.
func deriveNotificationKey(secret string) []byte {
	return deriveLabeledKey("fasttask:notification:v1:", secret)
}

func deriveLabeledKey(label, secret string) []byte {
	key := sha256.Sum256([]byte(label + secret))
	return key[:]
}

func (a *ProviderService) encryptSecret(value string) (string, error) {
	return encryptWithKey(a.secretKey, value)
}

func (a *ProviderService) decryptSecret(value string) (string, error) {
	return decryptWithKey(a.secretKey, value)
}

// encryptWithKey seals value under key. An empty value is not an error and produces an
// empty ciphertext, which is how a channel that needs no credential (Bark) is stored.
// A nil key fails closed: a deployment without an encryption secret stores no plaintext
// credential by accident.
func encryptWithKey(key []byte, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	gcm, err := secretGCM(key)
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

func decryptWithKey(key []byte, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	encoded, ok := strings.CutPrefix(value, "aesgcm.v1:")
	if !ok {
		return "", errors.New("the stored secret has an unsupported format")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	gcm, err := secretGCM(key)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("the stored secret is truncated")
	}
	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt secret: %w", err)
	}
	return string(plain), nil
}

func secretGCM(key []byte) (cipher.AEAD, error) {
	if key == nil {
		return nil, errors.New("the secret encryption key is not configured")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// secretHint masks a credential for display: enough to recognise which key is
// configured, never enough to use it.
func secretHint(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 8 {
		return value[:1] + "..."
	}
	return value[:4] + "..." + value[len(value)-4:]
}
