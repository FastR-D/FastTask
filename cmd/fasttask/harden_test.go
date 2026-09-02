package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/persistence"
	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
)

func TestHardenRotatesProviderEncryptionKeyWithoutBreakingProviders(t *testing.T) {
	dir := t.TempDir()
	databasePath := filepath.Join(dir, "fasttask.db")
	envPath := filepath.Join(dir, "fasttask.env")
	previousJWT := "previous-jwt-secret-with-enough-characters"
	plaintextKey := "provider-plain-key-for-tests"

	store, err := persistence.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	authConfig := config.Config{JWTSecret: previousJWT, AccessTTL: 3600000000000, RefreshTTL: 3600000000000, AdminIdentifier: "admin", AdminPassword: "initial-admin-password", AdminName: "Admin"}
	if err := platformauth.New(store, authConfig).EnsureAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}
	oldApp := application.NewWithSecret(store, previousJWT)
	ciphertext, hint, err := oldApp.EncryptProviderKey(plaintextKey)
	if err != nil {
		t.Fatal(err)
	}
	now := persistence.Now()
	provider := persistence.ModelProvider{ID: persistence.NewID("provider"), Name: "Test", ProviderType: "openai_compatible", BaseURL: "https://llm.example/v1", ModelName: "test-model", APIKeyCiphertext: ciphertext, APIKeyHint: hint, Status: "active", IsDefault: true, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&provider).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	envContent := "FASTTASK_ENV=production\nFASTTASK_DATABASE=" + databasePath + "\nFASTTASK_ADMIN_USER=admin\nFASTTASK_JWT_SECRET=" + previousJWT + "\nFASTTASK_ADMIN_PASSWORD=initial-admin-password\n"
	if err := os.WriteFile(envPath, []byte(envContent), 0o600); err != nil {
		t.Fatal(err)
	}
	harden := hardenCommand()
	harden.SetArgs([]string{"--env-file", envPath})
	if err := harden.Execute(); err != nil {
		t.Fatal(err)
	}

	values, err := readEnvFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	providerKey := values["FASTTASK_PROVIDER_ENCRYPTION_KEY"]
	if len(providerKey) < 32 || providerKey == previousJWT {
		t.Fatalf("provider encryption key was not hardened: %d bytes", len(providerKey))
	}
	if values["FASTTASK_JWT_SECRET"] == previousJWT || len(values["FASTTASK_JWT_SECRET"]) < 32 {
		t.Fatal("JWT secret was not rotated")
	}

	reopened, err := persistence.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var stored persistence.ModelProvider
	if err := reopened.DB.First(&stored, "id = ?", provider.ID).Error; err != nil {
		t.Fatal(err)
	}
	newApp := application.NewWithSecret(reopened, providerKey)
	decrypted, err := newApp.DecryptProviderKey(stored.APIKeyCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	if decrypted != plaintextKey {
		t.Fatal("provider key was not re-encrypted")
	}
}
