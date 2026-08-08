package main

import (
	cryptorand "crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strings"

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
		jwtSecret, err := randomSecret(48)
		if err != nil {
			return err
		}
		panelSecret, err := randomSecret(48)
		if err != nil {
			return err
		}
		adminPassword, err := randomSecret(24)
		if err != nil {
			return err
		}
		values["FASTTASK_ENV"] = "production"
		values["FASTTASK_JWT_SECRET"] = jwtSecret
		values["FASTTASK_PANEL_JWT_SECRET"] = panelSecret
		values["FASTTASK_ADMIN_PASSWORD"] = adminPassword
		if err := writeEnvFile(envPath, values); err != nil {
			return err
		}
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
