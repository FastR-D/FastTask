package persistence

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"gorm.io/gorm"
)

func TestMigrationsWALAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fasttask.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var journal string
	if err := store.DB.Raw("PRAGMA journal_mode").Scan(&journal).Error; err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Fatalf("journal mode = %q, want wal", journal)
	}
	now := Now()
	user := User{ID: NewID("user"), Identifier: "tester", PasswordHash: "hash", DisplayName: "Tester", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var found User
	if err := reopened.DB.First(&found, "id = ?", user.ID).Error; err != nil {
		t.Fatal(err)
	}
	if found.Identifier != "tester" {
		t.Fatalf("unexpected user: %#v", found)
	}
	var version int
	if err := reopened.DB.Raw("SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version).Error; err != nil {
		t.Fatal(err)
	}
	if version != ExpectedSchemaVersion {
		t.Fatalf("schema version=%d, want %d", version, ExpectedSchemaVersion)
	}
}

func TestMigrationUpgradeFromVersionOne(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	contents, err := embeddedMigrations.ReadFile("migrations/000001_init.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DB.Exec(string(contents)).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := store.DB.Raw("SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version).Error; err != nil {
		t.Fatal(err)
	}
	if version != ExpectedSchemaVersion {
		t.Fatalf("schema version=%d, want %d", version, ExpectedSchemaVersion)
	}
	if !store.DB.Migrator().HasTable(&ExternalImport{}) {
		t.Fatal("external_imports table was not created")
	}
	if !store.DB.Migrator().HasTable(&TaskCoord{}) {
		t.Fatal("task_coords table was not created")
	}
}

// TestUpgradeFromVersionThreePreservesData 覆盖 v3 旧库升级到 v4 的发布路径。
func TestUpgradeFromVersionThreePreservesData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"000001_init.up.sql", "000002_external_imports.up.sql", "000003_admin_platform.up.sql"} {
		contents, err := embeddedMigrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DB.Exec(string(contents)).Error; err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	var version int
	if err := store.DB.Raw("SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version).Error; err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatalf("prepared database is at version %d, want 3", version)
	}
	if err := store.Ready(context.Background()); err == nil {
		t.Fatal("version 3 database reported ready under the new binary")
	}
	now := Now()
	user := User{ID: NewID("user"), Identifier: "upgrade", PasswordHash: "hash", DisplayName: "Upgrade", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	goal := Goal{ID: NewID("goal"), UserID: user.ID, Title: "升级前的目标", SuccessCriteria: "数据必须保留", Status: "active", Revision: 3, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&goal).Error; err != nil {
		t.Fatal(err)
	}
	task := Task{ID: NewID("task"), UserID: user.ID, GoalID: goal.ID, Type: "task", Title: "升级前的任务", Status: "in_progress", Priority: 80, EstimateMinutes: 50, SuccessCriteria: "保留", MinimumAction: "保留", Revision: 7, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate v3 database: %v", err)
	}
	if err := reopened.Ready(context.Background()); err != nil {
		t.Fatalf("readiness after upgrade: %v", err)
	}
	if !reopened.DB.Migrator().HasTable(&TaskCoord{}) {
		t.Fatal("task_coords table missing after upgrade")
	}
	var migratedTask Task
	if err := reopened.DB.First(&migratedTask, "id = ?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if migratedTask.Title != task.Title || migratedTask.Status != "in_progress" || migratedTask.Revision != 7 {
		t.Fatalf("existing task changed during upgrade: %#v", migratedTask)
	}
	var migratedGoal Goal
	if err := reopened.DB.First(&migratedGoal, "id = ?", goal.ID).Error; err != nil {
		t.Fatal(err)
	}
	if migratedGoal.Revision != 3 || migratedGoal.Title != goal.Title {
		t.Fatalf("existing goal changed during upgrade: %#v", migratedGoal)
	}
	coord := TaskCoord{ID: NewID("coord"), UserID: user.ID, TaskID: task.ID, Lens: "research_risk", X: 70, Y: 85, Source: "agent", Rationale: "升级后写入", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := reopened.DB.Create(&coord).Error; err != nil {
		t.Fatalf("write coord after upgrade: %v", err)
	}
	var duplicate TaskCoord
	duplicate = coord
	duplicate.ID = NewID("coord")
	if err := reopened.DB.Create(&duplicate).Error; err == nil {
		t.Fatal("duplicate (task_id, lens) coord accepted")
	}
	if err := reopened.Migrate(context.Background()); err != nil {
		t.Fatalf("second migrate must be a no-op: %v", err)
	}
	if err := reopened.DB.Raw("SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version).Error; err != nil {
		t.Fatal(err)
	}
	if version != ExpectedSchemaVersion {
		t.Fatalf("schema version=%d, want %d", version, ExpectedSchemaVersion)
	}
}

func TestExternalImportSourceUniquenessIsUserScoped(t *testing.T) {
	store := newTestStore(t)
	defer store.Close()
	now := Now()
	users := []User{
		{ID: NewID("user"), Identifier: "import-a", PasswordHash: "hash", DisplayName: "A", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now},
		{ID: NewID("user"), Identifier: "import-b", PasswordHash: "hash", DisplayName: "B", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now},
	}
	if err := store.DB.Create(&users).Error; err != nil {
		t.Fatal(err)
	}
	first := ExternalImport{ID: NewID("import"), UserID: users[0].ID, SchemaVersion: "1.0", SourceSystem: "fastread", SourceExternalID: "paper-1", Kind: "research_material", Title: "Paper", ArtifactsJSON: "[]", MetadataJSON: "{}", Status: "candidate", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&first).Error; err != nil {
		t.Fatal(err)
	}
	duplicate := first
	duplicate.ID = NewID("import")
	if err := store.DB.Create(&duplicate).Error; err == nil {
		t.Fatal("duplicate source key for one user was accepted")
	}
	otherUser := first
	otherUser.ID, otherUser.UserID = NewID("import"), users[1].ID
	if err := store.DB.Create(&otherUser).Error; err != nil {
		t.Fatalf("same source key for another user rejected: %v", err)
	}
}

func TestTransactionRollbackAndActiveSessionConstraint(t *testing.T) {
	store := newTestStore(t)
	defer store.Close()
	ctx := context.Background()
	now := Now()
	user := User{ID: NewID("user"), Identifier: "rollback", PasswordHash: "hash", DisplayName: "Rollback", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	rolledBack := User{ID: NewID("user"), Identifier: "rolled-back", PasswordHash: "hash", DisplayName: "Nope", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	err := store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Create(&rolledBack).Error; err != nil {
			return err
		}
		return errors.New("rollback")
	})
	if err == nil {
		t.Fatal("transaction did not return rollback error")
	}
	var rolledBackCount int64
	if err := store.DB.Model(&User{}).Where("id = ?", rolledBack.ID).Count(&rolledBackCount).Error; err != nil {
		t.Fatal(err)
	}
	if rolledBackCount != 0 {
		t.Fatal("transaction did not roll back")
	}
	goal := Goal{ID: NewID("goal"), UserID: user.ID, Title: "Goal", SuccessCriteria: "Done", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&goal).Error; err != nil {
		t.Fatal(err)
	}
	task := Task{ID: NewID("task"), UserID: user.ID, GoalID: goal.ID, Type: "task", Title: "Task", Status: "ready", Priority: 50, EstimateMinutes: 25, SuccessCriteria: "Done", MinimumAction: "Start", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	first := WorkSession{ID: NewID("work"), UserID: user.ID, TaskID: task.ID, SessionType: "pomodoro", TargetMinutes: 25, Status: "running", StartedAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&first).Error; err != nil {
		t.Fatal(err)
	}
	second := first
	second.ID = NewID("work")
	if err := store.DB.Create(&second).Error; err == nil {
		t.Fatal("second active session was accepted")
	}
}

func TestBackupCreatesReadableDatabase(t *testing.T) {
	store := newTestStore(t)
	defer store.Close()
	destination := filepath.Join(t.TempDir(), "backup.db")
	if err := store.Backup(context.Background(), destination); err != nil {
		t.Fatal(err)
	}
	backup, err := Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	if err := backup.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	var integrity string
	if err := backup.DB.Raw("PRAGMA integrity_check").Scan(&integrity).Error; err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity = %s", integrity)
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}
