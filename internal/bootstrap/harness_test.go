package bootstrap

import (
	"context"
	"testing"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// TestSchedulerReapsLostHarnessHosts proves the periodic half of run recovery is actually wired
// (doc/harness.md §11): the scheduler role registers the reaper, and running it interrupts a run
// whose host stopped beating. Without this, a closed tab would leave a run holding the user's single
// active slot forever.
func TestSchedulerReapsLostHarnessHosts(t *testing.T) {
	cfg := testConfig(t)
	// A model must be configured, or POST /agent/commands keeps the run server-driven (§1.2) and
	// there is no host to lose.
	cfg.OpenAIBaseURL = "http://127.0.0.1:1/v1"
	cfg.OpenAIModel = "qwen3.8-max"
	cfg.OpenAIAPIKey = "not-used-by-this-test"

	store, err := persistence.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	app := application.NewWithSecret(store, cfg.ProviderEncryptionKey)
	agent := NewAgentService(app, cfg, nil)

	now := persistence.Now()
	user := persistence.User{ID: persistence.NewID("user"), Identifier: "reaped", PasswordHash: "hash", DisplayName: "Reaped", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	submitted, err := agent.SubmitCommands(context.Background(), user.ID, application.CommandsRequest{
		HarnessMode: application.HarnessModeWASM,
		Commands: []application.Command{{Type: "add-message", Message: &application.CommandMessage{
			Role: "user", Parts: []application.CommandPart{{Type: "text", Text: "帮我推进论文"}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if submitted.HarnessMode != application.HarnessModeWASM {
		t.Fatalf("harness mode=%q, want wasm", submitted.HarnessMode)
	}
	if _, err := agent.IssueRunGrant(context.Background(), user.ID, submitted.RunID, ""); err != nil {
		t.Fatalf("IssueRunGrant: %v", err)
	}

	// The host disappears: backdate its last heartbeat past the loss threshold.
	lost := persistence.Now().Add(-2 * application.HarnessHeartbeatLoss)
	if err := store.DB.Model(&persistence.AgentHarnessToken{}).
		Where("run_id = ?", submitted.RunID).
		Updates(map[string]any{"created_at": lost, "last_heartbeat_at": lost}).Error; err != nil {
		t.Fatal(err)
	}

	maintenance, err := NewScheduler(store, app, agent)
	if err != nil {
		t.Fatal(err)
	}
	if got := maintenance.SweeperCount(); got < 2 {
		t.Fatalf("the scheduler registered %d sweepers, want the harness reaper and the token pruner", got)
	}
	maintenance.SweepOnce(context.Background())

	var run persistence.AgentRun
	if err := store.DB.Where("id = ?", submitted.RunID).First(&run).Error; err != nil {
		t.Fatal(err)
	}
	if run.Status != persistence.RunInterrupted {
		t.Fatalf("run status=%q, want interrupted after the heartbeat was lost", run.Status)
	}
	// The capability died with it, so a host that comes back cannot keep driving.
	if _, err := agent.AuthenticateHarness(context.Background(), "Bearer not-the-token"); err == nil {
		t.Fatal("an unknown token authenticated")
	}
	var live int64
	store.DB.Model(&persistence.AgentHarnessToken{}).
		Where("run_id = ? AND revoked_at IS NULL", submitted.RunID).Count(&live)
	if live != 0 {
		t.Fatalf("%d tokens are still live for an interrupted run", live)
	}
	// The user's slot is free again: a new run can start.
	if _, err := agent.SubmitCommands(context.Background(), user.ID, application.CommandsRequest{
		HarnessMode: application.HarnessModeWASM,
		Commands: []application.Command{{Type: "add-message", Message: &application.CommandMessage{
			Role: "user", Parts: []application.CommandPart{{Type: "text", Text: "再来一次"}},
		}}},
	}); err != nil {
		t.Fatalf("a new run after the reap: %v", err)
	}
}
