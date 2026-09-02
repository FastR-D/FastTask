package main

import (
	cryptorand "crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/persistence"
	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
	"github.com/spf13/cobra"
	"gorm.io/gorm"
)

func hardenCommand() *cobra.Command {
	var envPath string
	cmd := &cobra.Command{Use: "harden", Short: "Rotate production secrets and administrator password", RunE: func(cmd *cobra.Command, args []string) error {
		values, err := readEnvFile(envPath)
		if err != nil {
			return err
		}
		previousJWTSecret := values["FASTTASK_JWT_SECRET"]
		previousProviderKey := values["FASTTASK_PROVIDER_ENCRYPTION_KEY"]
		jwtSecret, err := randomSecret(48)
		if err != nil {
			return err
		}
		panelSecret, err := randomSecret(48)
		if err != nil {
			return err
		}
		providerKey := previousProviderKey
		if providerKey == "" {
			providerKey, err = randomSecret(48)
			if err != nil {
				return err
			}
		}
		adminPassword, err := randomSecret(24)
		if err != nil {
			return err
		}
		values["FASTTASK_ENV"] = "production"
		values["FASTTASK_JWT_SECRET"] = jwtSecret
		values["FASTTASK_PANEL_JWT_SECRET"] = panelSecret
		values["FASTTASK_PROVIDER_ENCRYPTION_KEY"] = providerKey
		values["FASTTASK_ADMIN_PASSWORD"] = adminPassword
		databasePath := values["FASTTASK_DATABASE"]
		if databasePath == "" {
			databasePath = "data/fasttask.db"
		}
		identifier := values["FASTTASK_ADMIN_USER"]
		if identifier == "" {
			identifier = "admin"
		}
		store, err := persistence.Open(databasePath)
		if err != nil {
			return err
		}
		defer store.Close()
		if err := store.Migrate(cmd.Context()); err != nil {
			return err
		}
		var providerCount int64
		if err := store.DB.WithContext(cmd.Context()).Model(&persistence.ModelProvider{}).Count(&providerCount).Error; err != nil {
			return err
		}
		if providerCount > 0 {
			if previousProviderKey == "" && previousJWTSecret == "" {
				return fmt.Errorf("provider keys exist but no previous encryption key is available")
			}
			oldKey := previousProviderKey
			if oldKey == "" {
				oldKey = previousJWTSecret
			}
			oldApp := application.NewWithSecret(store, oldKey)
			newApp := application.NewWithSecret(store, providerKey)
			var providers []persistence.ModelProvider
			if err := store.DB.WithContext(cmd.Context()).Find(&providers).Error; err != nil {
				return err
			}
			if err := store.Transaction(cmd.Context(), func(tx *gorm.DB) error {
				for _, provider := range providers {
					plainKey, err := oldApp.DecryptProviderKey(provider.APIKeyCiphertext)
					if err != nil {
						return fmt.Errorf("decrypt provider %s: %w", provider.ID, err)
					}
					cipherKey, hint, err := newApp.EncryptProviderKey(plainKey)
					if err != nil {
						return err
					}
					if err := tx.Model(&persistence.ModelProvider{}).Where("id = ?", provider.ID).Updates(map[string]any{"api_key_ciphertext": cipherKey, "api_key_hint": hint, "revision": provider.Revision + 1, "updated_at": persistence.Now()}).Error; err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return err
			}
		}
		hash, err := platformauth.HashPassword(adminPassword)
		if err != nil {
			return err
		}
		result := store.DB.WithContext(cmd.Context()).Model(&persistence.User{}).Where("identifier = ?", identifier).Updates(map[string]any{"password_hash": hash, "updated_at": persistence.Now(), "revision": gorm.Expr("revision + 1")})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("administrator %q not found", identifier)
		}
		if err := store.DB.WithContext(cmd.Context()).Model(&persistence.Session{}).Where("user_id IN (SELECT id FROM users WHERE identifier = ?)", identifier).Updates(map[string]any{"status": "revoked", "updated_at": persistence.Now()}).Error; err != nil {
			return err
		}
		if err := writeEnvFile(envPath, values); err != nil {
			return err
		}
		fmt.Printf("production secrets rotated; credentials are stored in %s\n", envPath)
		return nil
	}}
	cmd.Flags().StringVar(&envPath, "env-file", ".env", "environment file to harden")
	return cmd
}

func randomSecret(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := cryptorand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func readEnvFile(path string) (map[string]string, error) {
	values := map[string]string{}
	contents, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), "\"'")
		}
	}
	return values, nil
}

func writeEnvFile(path string, values map[string]string) error {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var content strings.Builder
	for _, key := range keys {
		content.WriteString(key)
		content.WriteByte('=')
		content.WriteString(values[key])
		content.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
