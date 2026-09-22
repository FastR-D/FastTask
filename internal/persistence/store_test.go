package persistence

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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

// TestUpgradeFromVersionFourPreservesData covers the v4 -> current release path
// that adds the agent runtime tables (doc/agent-impl.md §3.2) and then moves
// threads onto their own table (doc/chat-features.md §2.3). Existing data must
// survive, /health/ready must pass after migration, and the new tables must be
// writable.
func TestUpgradeFromVersionFourPreservesData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"000001_init.up.sql", "000002_external_imports.up.sql", "000003_admin_platform.up.sql", "000004_task_coords.up.sql"} {
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
	if version != 4 {
		t.Fatalf("prepared database is at version %d, want 4", version)
	}
	if err := store.Ready(context.Background()); err == nil {
		t.Fatal("version 4 database reported ready under the v5 binary")
	}
	now := Now()
	user := User{ID: NewID("user"), Identifier: "upgrade-v4", PasswordHash: "hash", DisplayName: "Upgrade", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	conversation := Conversation{ID: NewID("conv"), UserID: user.ID, Title: "升级前的对话", Status: "active", Revision: 2, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&conversation).Error; err != nil {
		t.Fatal(err)
	}
	legacyMessage := ConversationMessage{ID: NewID("msg"), UserID: user.ID, ConversationID: conversation.ID, Role: "user", Content: "旧扁平消息必须保留", CreatedAt: now}
	if err := store.DB.Create(&legacyMessage).Error; err != nil {
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
		t.Fatalf("migrate v4 database: %v", err)
	}
	if err := reopened.Ready(context.Background()); err != nil {
		t.Fatalf("readiness after upgrade: %v", err)
	}
	for _, model := range []any{&AgentRun{}, &AgentMessage{}, &AgentMessagePart{}, &AgentRunChunk{}} {
		if !reopened.DB.Migrator().HasTable(model) {
			t.Fatalf("agent runtime table missing after upgrade: %T", model)
		}
	}
	var migratedConversation Conversation
	if err := reopened.DB.First(&migratedConversation, "id = ?", conversation.ID).Error; err != nil {
		t.Fatal(err)
	}
	if migratedConversation.Title != conversation.Title || migratedConversation.Revision != 2 {
		t.Fatalf("existing conversation changed during upgrade: %#v", migratedConversation)
	}
	var migratedMessage ConversationMessage
	if err := reopened.DB.First(&migratedMessage, "id = ?", legacyMessage.ID).Error; err != nil {
		t.Fatal(err)
	}
	if migratedMessage.Content != "旧扁平消息必须保留" {
		t.Fatalf("legacy conversation message changed during upgrade: %#v", migratedMessage)
	}

	// Agent rows hang off agent_threads (doc/chat-features.md §2.3): a run and its
	// message/part/chunk log must be writable against the upgraded schema.
	thread := AgentThread{ID: NewID("thr"), UserID: user.ID, Title: "升级后的会话", Status: ThreadRegular, CreatedAt: now, UpdatedAt: now}
	if err := reopened.DB.Create(&thread).Error; err != nil {
		t.Fatalf("write agent_thread after upgrade: %v", err)
	}
	run := AgentRun{ID: NewID("run"), UserID: user.ID, ThreadID: thread.ID, Status: "queued", StateJSON: "{}", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := reopened.DB.Create(&run).Error; err != nil {
		t.Fatalf("write agent_run after upgrade: %v", err)
	}
	message := AgentMessage{ID: NewID("amsg"), UserID: user.ID, ThreadID: thread.ID, RunID: run.ID, Role: "assistant", Seq: 1, CreatedAt: now}
	if err := reopened.DB.Create(&message).Error; err != nil {
		t.Fatalf("write agent_message after upgrade: %v", err)
	}
	part := AgentMessagePart{ID: NewID("apart"), UserID: user.ID, MessageID: message.ID, Idx: 0, Type: "text", Text: "hello", ArgsJSON: "{}", CreatedAt: now, UpdatedAt: now}
	if err := reopened.DB.Create(&part).Error; err != nil {
		t.Fatalf("write agent_message_part after upgrade: %v", err)
	}
	chunk := AgentRunChunk{RunID: run.ID, Seq: 0, UserID: user.ID, ChunkJSON: `{"type":"step-start"}`, CreatedAt: now}
	if err := reopened.DB.Create(&chunk).Error; err != nil {
		t.Fatalf("write agent_run_chunk after upgrade: %v", err)
	}
	duplicateChunk := chunk
	if err := reopened.DB.Create(&duplicateChunk).Error; err == nil {
		t.Fatal("duplicate (run_id, seq) chunk accepted")
	}
	// A second active run on the same thread must be rejected by the partial
	// unique index (agent-impl.md §3.1).
	secondRun := run
	secondRun.ID = NewID("run")
	secondRun.Status = "running"
	if err := reopened.DB.Create(&secondRun).Error; err == nil {
		t.Fatal("second queued/running run on one thread accepted")
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

// TestUpgradeFromVersionFiveBackfillsAgentThreads covers the v5 -> v6 release path
// (doc/chat-features.md §2.3): agent threads move from the conversations table onto
// their own table, existing runs and messages are repointed, and nothing is lost.
// The migration rewrites two referenced tables, so it also proves the rewrite leaves
// the schema referentially sound and can be replayed without duplicating threads.
func TestUpgradeFromVersionFiveBackfillsAgentThreads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v5.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"000001_init.up.sql", "000002_external_imports.up.sql", "000003_admin_platform.up.sql", "000004_task_coords.up.sql", "000005_agent_runtime.up.sql"} {
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
	if version != 5 {
		t.Fatalf("prepared database is at version %d, want 5", version)
	}

	now := Now()
	user := User{ID: NewID("user"), Identifier: "threads-v5", PasswordHash: "hash", DisplayName: "Threads", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	// A titled conversation and an untitled one whose thread title must fall back to
	// the first user message (§2.3 backfill step 1).
	titled := Conversation{ID: NewID("conv"), UserID: user.ID, Title: "论文拆解", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	untitled := Conversation{ID: NewID("conv"), UserID: user.ID, Title: "   ", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	// A conversation with no agent messages must NOT become a thread.
	unused := Conversation{ID: NewID("conv"), UserID: user.ID, Title: "旧对话", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	for i := range []Conversation{titled, untitled, unused} {
		conversation := []Conversation{titled, untitled, unused}[i]
		if err := store.DB.Create(&conversation).Error; err != nil {
			t.Fatal(err)
		}
	}

	// The fixtures are written as raw SQL on purpose: the v5 schema has no
	// harness_mode column, so the current GORM models cannot insert into it.
	insertRun := func(threadID, status string) string {
		id := NewID("run")
		err := store.DB.Exec(`INSERT INTO agent_runs
			(id, user_id, thread_id, job_id, status, state_json, checkpoint_seq, error_code, error_message, revision, created_at, updated_at)
			VALUES (?, ?, ?, '', ?, '{}', 0, '', '', 1, ?, ?)`,
			id, user.ID, threadID, status, now, now).Error
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	insertMessage := func(threadID, runID, role string, seq int) string {
		id := NewID("amsg")
		err := store.DB.Exec(`INSERT INTO agent_messages (id, user_id, thread_id, run_id, role, seq, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, id, user.ID, threadID, runID, role, seq, now).Error
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	titledRunID := insertRun(titled.ID, "succeeded")
	untitledRunID := insertRun(untitled.ID, "succeeded")
	insertMessage(titled.ID, titledRunID, "user", 1)
	untitledMessageID := insertMessage(untitled.ID, untitledRunID, "user", 1)
	if err := store.DB.Exec(`INSERT INTO agent_message_parts (id, user_id, message_id, idx, type, text, args_json, result_json, artifact_json, approval_status, is_error, created_at, updated_at)
		VALUES (?, ?, ?, 0, 'text', ?, '{}', '', '', '', 0, ?, ?)`,
		NewID("apart"), user.ID, untitledMessageID,
		"帮我把这周的实验排一下顺序，顺便看看有没有卡住的任务，再给一个今天就能开始的最小行动", now, now).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB.Exec(`INSERT INTO agent_run_chunks (run_id, seq, user_id, chunk_json, created_at)
		VALUES (?, 0, ?, ?, ?)`, titledRunID, user.ID, `{"type":"step-start"}`, now).Error; err != nil {
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
		t.Fatalf("migrate v5 database: %v", err)
	}
	if err := reopened.Ready(context.Background()); err != nil {
		t.Fatalf("readiness after upgrade: %v", err)
	}

	var threads []AgentThread
	if err := reopened.DB.Order("created_at").Find(&threads).Error; err != nil {
		t.Fatal(err)
	}
	if len(threads) != 2 {
		t.Fatalf("backfilled %d threads, want 2 (only conversations with agent messages): %#v", len(threads), threads)
	}
	bySource := map[string]AgentThread{}
	for _, thread := range threads {
		if thread.SourceConversationID == nil {
			t.Fatalf("backfilled thread without source_conversation_id: %#v", thread)
		}
		bySource[*thread.SourceConversationID] = thread
	}
	backfilled, ok := bySource[titled.ID]
	if !ok {
		t.Fatalf("no thread backfilled for conversation %s", titled.ID)
	}
	if backfilled.Title != "论文拆解" || backfilled.Status != ThreadRegular || backfilled.UserID != user.ID {
		t.Fatalf("backfilled thread mismatch: %#v", backfilled)
	}
	if backfilled.Checkpoint != nil || backfilled.LibfxVersion != "" {
		t.Fatalf("pre-harness thread must have no checkpoint: %#v", backfilled)
	}
	if backfilled.LastMessageAt == nil {
		t.Fatal("backfilled thread has no last_message_at")
	}
	fallback, ok := bySource[untitled.ID]
	if !ok {
		t.Fatalf("no thread backfilled for conversation %s", untitled.ID)
	}
	if want := "帮我把这周的实验排一下顺序，顺便看看有没有卡住的任务，再给一"; fallback.Title != want {
		t.Fatalf("untitled thread title=%q, want the first 30 runes of the first user message %q", fallback.Title, want)
	}
	if _, ok := bySource[unused.ID]; ok {
		t.Fatal("a conversation without agent messages became a thread")
	}

	// Runs, messages, parts and chunks survive with repointed thread ids.
	var migratedRun AgentRun
	if err := reopened.DB.First(&migratedRun, "id = ?", titledRunID).Error; err != nil {
		t.Fatal(err)
	}
	if migratedRun.ThreadID != backfilled.ID {
		t.Fatalf("run thread_id=%q, want the backfilled thread %q", migratedRun.ThreadID, backfilled.ID)
	}
	var migratedMessage AgentMessage
	if err := reopened.DB.First(&migratedMessage, "id = ?", untitledMessageID).Error; err != nil {
		t.Fatal(err)
	}
	if migratedMessage.ThreadID != fallback.ID {
		t.Fatalf("message thread_id=%q, want the backfilled thread %q", migratedMessage.ThreadID, fallback.ID)
	}
	var parts, chunks int64
	if err := reopened.DB.Model(&AgentMessagePart{}).Count(&parts).Error; err != nil {
		t.Fatal(err)
	}
	if err := reopened.DB.Model(&AgentRunChunk{}).Count(&chunks).Error; err != nil {
		t.Fatal(err)
	}
	if parts != 1 || chunks != 1 {
		t.Fatalf("parts=%d chunks=%d, want 1 and 1", parts, chunks)
	}
	var legacy Conversation
	if err := reopened.DB.First(&legacy, "id = ?", titled.ID).Error; err != nil {
		t.Fatalf("the conversations table must survive the rewrite: %v", err)
	}

	// The rewritten tables reference agent_threads, not conversations.
	for table, want := range map[string]string{"agent_runs": "%REFERENCES agent_threads%", "agent_messages": "%REFERENCES agent_threads%"} {
		var ddl string
		if err := reopened.DB.Raw("SELECT sql FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&ddl).Error; err != nil {
			t.Fatal(err)
		}
		if !like(ddl, want) {
			t.Fatalf("%s ddl does not reference agent_threads: %s", table, ddl)
		}
		if like(ddl, "%thread_id TEXT NOT NULL REFERENCES conversations%") {
			t.Fatalf("%s still points thread_id at conversations: %s", table, ddl)
		}
	}

	// Replaying the migration must not duplicate threads (§2.3: 迁移必须可重入).
	if err := reopened.Migrate(context.Background()); err != nil {
		t.Fatalf("second migrate must be a no-op: %v", err)
	}
	var after int64
	if err := reopened.DB.Model(&AgentThread{}).Count(&after).Error; err != nil {
		t.Fatal(err)
	}
	if after != 2 {
		t.Fatalf("threads=%d after a second migrate, want 2", after)
	}
	// A new run may only reference a thread, never a bare conversation id.
	orphan := AgentRun{ID: NewID("run"), UserID: user.ID, ThreadID: unused.ID, Status: "queued", StateJSON: "{}", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := reopened.DB.Create(&orphan).Error; err == nil {
		t.Fatal("a run pointing at a non-thread id was accepted")
	}
}

// like is a minimal SQL LIKE for the DDL assertions above.
func like(value, pattern string) bool {
	pattern = strings.TrimSuffix(strings.TrimPrefix(pattern, "%"), "%")
	return strings.Contains(value, pattern)
}
