package persistence

import (
	"context"
	"encoding/json"
	"github.com/FastR-D/FastTask/internal/agent/protocol"
	"gorm.io/gorm"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func executionFixture(t *testing.T) (*AgentRepository, string, *AgentRun) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	owner := "agent-owner"
	u := User{ID: owner, Identifier: owner, DisplayName: owner, PasswordHash: "fixture", Timezone: "Asia/Singapore", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: Now(), UpdatedAt: Now()}
	if err = s.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	r := NewAgentRepository(s)
	thread, err := r.CreateThread(ctx, owner, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{UserID: owner, ThreadID: thread.ID, Status: RunRunning}
	if err = r.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	return r, owner, run
}
func TestConcurrentEventsFoldSnapshotAtomically(t *testing.T) {
	repo, owner, run := executionFixture(t)
	ctx := context.Background()
	set, _ := protocol.Set([]string{"text"}, "")
	b, _ := json.Marshal(protocol.UpdateState(set))
	if _, err := repo.AppendChunk(ctx, owner, run.ID, string(b)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			op, _ := protocol.AppendText([]string{"text"}, "中")
			b, _ := json.Marshal(protocol.UpdateState(op))
			_, err := repo.AppendChunk(ctx, owner, run.ID, string(b))
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	current, err := repo.GetRun(ctx, owner, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err = json.Unmarshal([]byte(current.StateJSON), &got); err != nil {
		t.Fatal(err)
	}
	if len([]rune(got["text"].(string))) != 20 {
		t.Fatal("concurrent text lost")
	}
	chunks, err := repo.ListChunksFrom(ctx, owner, run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := "{}"
	for i, c := range chunks {
		if c.Seq != i {
			t.Fatal("noncontiguous log")
		}
		snapshot, err = protocol.ApplyStateJSON(snapshot, c.ChunkJSON)
		if err != nil {
			t.Fatal(err)
		}
	}
	if snapshot != current.StateJSON || current.CheckpointSeq != len(chunks) {
		t.Fatal("snapshot does not equal replay")
	}
	if err = repo.SaveRunState(ctx, owner, run.ID, `{"stale":true}`, 1); err != nil {
		t.Fatal(err)
	}
	latest, _ := repo.GetRun(ctx, owner, run.ID)
	if latest.StateJSON != snapshot {
		t.Fatal("stale session rolled snapshot back")
	}
}
func TestApprovalIntervalsAreUnionNotPollSum(t *testing.T) {
	repo, owner, run := executionFixture(t)
	ctx := context.Background()
	now := Now()
	transition := func(status string, at time.Time) {
		t.Helper()
		if err := repo.store.Transaction(ctx, func(tx *gorm.DB) error { return repo.WithTx(tx).SetApprovalStatus(ctx, owner, run.ID, status, at) }); err != nil {
			t.Fatal(err)
		}
	}
	transition(RunAwaitingApproval, now)
	transition(RunAwaitingApproval, now.Add(30*time.Second))
	transition(RunRunning, now.Add(time.Minute))
	got, _ := repo.GetRun(ctx, owner, run.ID)
	if got.ApprovalWaitMs != 60000 || got.ApprovalStartedAt != nil {
		t.Fatalf("wait counted twice: %+v", got)
	}
}
func TestAttemptClaimAndTerminalFencing(t *testing.T) {
	repo, owner, run := executionFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	wins := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := repo.AcquireModelAttempt(ctx, owner, run.ID, 10, 8)
			if err == nil {
				wins <- lease
			}
		}()
	}
	wg.Wait()
	close(wins)
	if len(wins) != 1 {
		t.Fatalf("claims: %d", len(wins))
	}
	if err := repo.SetRunStatus(ctx, owner, run.ID, RunCancelled, "CANCELLED", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetRunStatus(ctx, owner, run.ID, RunSucceeded, "", ""); err == nil {
		t.Fatal("terminal resurrected")
	}
}
