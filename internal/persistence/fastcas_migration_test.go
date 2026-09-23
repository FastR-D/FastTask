package persistence

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyFastCASVersionSixUpgradesThroughAgentMigrations(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "legacy-fastcas.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, name := range []string{"000001_init.up.sql", "000002_external_imports.up.sql", "000003_admin_platform.up.sql", "000004_task_coords.up.sql", "000005_agent_runtime.up.sql"} {
		contents, err := embeddedMigrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DB.Exec(string(contents)).Error; err != nil {
			t.Fatal(err)
		}
	}
	legacy, err := embeddedMigrations.ReadFile("migrations/000009_fastcas.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DB.Exec(strings.Replace(string(legacy), "VALUES(9,'fastcas'", "VALUES(6,'fastcas'", 1)).Error; err != nil {
		t.Fatal(err)
	}
	now := Now()
	user := User{ID: NewID("user"), Identifier: "legacy-cas", PasswordHash: "hash", DisplayName: "Legacy", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB.Exec(`INSERT INTO sessions(id,family_id,user_id,refresh_hash,status,expires_at,created_at,updated_at,auth_source,cas_issuer,cas_sid,cas_link_id,cas_link_version)
 VALUES('legacy-session','legacy-family',?,'legacy-refresh','active',?,?,?,'fastcas','https://cas.example.test','legacy-sid','legacy-link',1)`, user.ID, now.AddDate(0, 1, 0), now, now).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB.Exec(`INSERT INTO fastcas_links(id,issuer,client_id,user_id,subject,state,version,verified_at)
 VALUES('legacy-link','https://cas.example.test','fasttask',?,'subject-1','active',1,?)`, user.ID, now).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var nameSix, nameNine string
	if err := store.DB.Raw("SELECT name FROM schema_migrations WHERE version=6").Scan(&nameSix).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB.Raw("SELECT name FROM schema_migrations WHERE version=9").Scan(&nameNine).Error; err != nil {
		t.Fatal(err)
	}
	if nameSix != "agent_threads" || nameNine != "fastcas" {
		t.Fatalf("migration sequence: v6=%q v9=%q", nameSix, nameNine)
	}
	var subject, source string
	if err := store.DB.Raw("SELECT subject FROM fastcas_links WHERE id='legacy-link'").Scan(&subject).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB.Raw("SELECT auth_source FROM sessions WHERE id='legacy-session'").Scan(&source).Error; err != nil {
		t.Fatal(err)
	}
	if subject != "subject-1" || source != "fastcas" {
		t.Fatalf("legacy account mapping changed: subject=%q source=%q", subject, source)
	}
}
