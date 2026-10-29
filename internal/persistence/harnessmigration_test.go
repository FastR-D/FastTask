package persistence

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestHarnessMigrationUpgradeAndMirrors(t *testing.T) {
	entries, err := embeddedMigrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".up.sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	store, err := Open(filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	// Replay a real pre-harness version 21 schema before invoking the production
	// migrator, rather than manufacturing new columns in a fresh current schema.
	for _, name := range names {
		embedded, err := embeddedMigrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		mirrored, err := os.ReadFile(filepath.Join("../../migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		// Version 1 has a pre-existing compact embedded representation.
		// New harness migrations must be byte-for-byte mirrored.
		if name >= "000022" && string(embedded) != string(mirrored) {
			t.Fatalf("migration mirror mismatch: %s", name)
		}
		if name >= "000022" {
			continue
		}
		if strings.Contains(string(embedded), noTransactionMarker) {
			err = execSchemaRewrite(ctx, store.DB, string(embedded))
		} else {
			err = store.DB.Exec(string(embedded)).Error
		}
		if err != nil {
			t.Fatalf("baseline %s: %v", name, err)
		}
	}
	now := Now()
	user := User{ID: "legacy-user", Identifier: "legacy-user", PasswordHash: "fixture", DisplayName: "Legacy", Timezone: "Asia/Singapore", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err = store.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	// Store an opaque checkpoint; its bytes must survive the upgrade even though
	// runtime recovery now uses canonical rows instead of restoring it.
	thread := AgentThread{ID: "legacy-thread", UserID: user.ID, Title: "Legacy", Status: "regular", CreatedAt: now, UpdatedAt: now}
	if err = store.DB.Omit("HarnessMode").Create(&thread).Error; err != nil {
		t.Fatal(err)
	}
	if err = store.DB.Model(&AgentThread{}).Where("id = ?", thread.ID).Update("checkpoint", []byte("legacy-checkpoint")).Error; err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	var version int
	store.DB.Raw("SELECT MAX(version) FROM schema_migrations").Scan(&version)
	if version != ExpectedSchemaVersion {
		t.Fatalf("schema %d != %d", version, ExpectedSchemaVersion)
	}
	var got AgentThread
	if err = store.DB.First(&got, "id = ?", thread.ID).Error; err != nil {
		t.Fatal(err)
	}
	if string(got.Checkpoint) != "legacy-checkpoint" {
		t.Fatal("upgrade lost checkpoint")
	}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal("migration not idempotent", err)
	}
}
