package application

import (
	"context"
	"strings"
	"testing"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// The thread catalogue (doc/chat-features.md §2).
//
// A thread is the unit a user reasons about, so the operations on it have to behave like file operations:
// listing is paginated and stable, archiving keeps everything, deleting removes everything, and a title is
// derived from what was actually said.

// seedThread runs one harness conversation on a new thread and returns its id.
func seedThread(t *testing.T, svc *AgentService, upstream *fakeUpstream, store *persistence.Store, userID, text string) string {
	t.Helper()
	runID := submitHarnessRun(t, svc, store, userID, text)
	host := newTestHost(t, svc, upstream, userID, runID)
	host.drive(4)
	var run persistence.AgentRun
	if err := store.DB.Where("id = ?", runID).First(&run).Error; err != nil {
		t.Fatal(err)
	}
	return run.ThreadID
}

func TestThreadCatalogueListsAndPages(t *testing.T) {
	upstream := newFakeUpstream(t, scriptedTurn{text: "回答一"}, scriptedTurn{text: "回答二"}, scriptedTurn{text: "回答三"})
	f, svc := harnessFixture(t, upstream)
	ctx := context.Background()

	first := seedThread(t, svc, upstream, f.store, f.user.ID, "第一个问题")
	second := seedThread(t, svc, upstream, f.store, f.user.ID, "第二个问题")
	third := seedThread(t, svc, upstream, f.store, f.user.ID, "第三个问题")

	page, err := svc.ListAgentThreads(ctx, f.user.ID, "", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("page has %d threads, want 2", len(page.Items))
	}
	if !page.HasMore || page.NextCursor == "" {
		t.Fatalf("page=%#v, want a next cursor", page)
	}
	// Newest activity first, so the third conversation leads.
	if page.Items[0].ID != third || page.Items[1].ID != second {
		t.Fatalf("order=%q,%q want %q,%q", page.Items[0].ID, page.Items[1].ID, third, second)
	}

	next, err := svc.ListAgentThreads(ctx, f.user.ID, "", page.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Items) != 1 || next.Items[0].ID != first {
		t.Fatalf("second page=%#v, want the remaining thread", next.Items)
	}
	if next.HasMore {
		t.Fatal("the last page claims more rows")
	}

	// A status filter that is not a status is refused rather than silently widened.
	if _, err := svc.ListAgentThreads(ctx, f.user.ID, "deleted", "", 20); err == nil {
		t.Fatal("an unknown status filter was accepted")
	}
}

func TestThreadArchiveKeepsEverythingAndDeleteRemovesIt(t *testing.T) {
	upstream := newFakeUpstream(t, scriptedTurn{
		text:      "让我看看目标",
		toolCalls: []agent.ToolCall{{ID: "call_1", Name: "list_goals", Arguments: `{}`}},
	}, scriptedTurn{text: "你有一个目标"})
	f, svc := harnessFixture(t, upstream, WithAttachmentDir(t.TempDir()))
	ctx := context.Background()

	threadID := seedThread(t, svc, upstream, f.store, f.user.ID, "我有哪些目标")
	attachment, err := svc.Attachments().Upload(ctx, f.user.ID, &threadID, pngBytes(t, 2, 2))
	if err != nil {
		t.Fatal(err)
	}

	archived, err := svc.SetAgentThreadStatus(ctx, f.user.ID, threadID, persistence.ThreadArchived)
	if err != nil {
		t.Fatal(err)
	}
	if archived.Status != persistence.ThreadArchived {
		t.Fatalf("status=%q, want archived", archived.Status)
	}
	// Archiving is not deleting: the transcript is still there (§2.5).
	if _, err := svc.GetAgentThread(ctx, f.user.ID, threadID); err != nil {
		t.Fatalf("an archived thread is unreadable: %v", err)
	}
	regular, err := svc.ListAgentThreads(ctx, f.user.ID, persistence.ThreadRegular, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range regular.Items {
		if item.ID == threadID {
			t.Fatal("an archived thread is still listed as regular")
		}
	}
	if _, err := svc.SetAgentThreadStatus(ctx, f.user.ID, threadID, "deleted"); err == nil {
		t.Fatal("an unknown status was accepted")
	}

	// Deleting takes the runs, messages, parts, chunk log and attachments with it (§2.5).
	if err := svc.DeleteAgentThread(ctx, f.user.ID, threadID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	counts := map[string]int64{}
	for table, model := range map[string]any{
		"agent_threads":        &persistence.AgentThread{},
		"agent_runs":           &persistence.AgentRun{},
		"agent_messages":       &persistence.AgentMessage{},
		"agent_message_parts":  &persistence.AgentMessagePart{},
		"agent_run_chunks":     &persistence.AgentRunChunk{},
		"agent_harness_tokens": &persistence.AgentHarnessToken{},
		"agent_attachments":    &persistence.AgentAttachment{},
	} {
		var n int64
		if err := f.store.DB.Model(model).Where("user_id = ?", f.user.ID).Count(&n).Error; err != nil {
			t.Fatal(err)
		}
		counts[table] = n
	}
	for table, n := range counts {
		if n != 0 {
			t.Fatalf("%s still has %d rows after the thread was deleted", table, n)
		}
	}
	if _, err := svc.Repository().GetAttachment(ctx, f.user.ID, attachment.AttachmentID); !persistence.IsNotFound(err) {
		t.Fatalf("attachment err=%v, want not-found", err)
	}
}

func TestThreadDeleteCancelsAnInFlightRun(t *testing.T) {
	upstream := newFakeUpstream(t, scriptedTurn{text: "回答"})
	f, svc := harnessFixture(t, upstream)
	ctx := context.Background()

	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "进行中")
	host := newTestHost(t, svc, upstream, f.user.ID, runID)
	if status := host.runStatus().Status; status != persistence.RunRunning {
		t.Fatalf("run status=%q, want running", status)
	}
	if err := svc.DeleteAgentThread(ctx, f.user.ID, host.threadID()); err != nil {
		t.Fatalf("delete with an in-flight run: %v", err)
	}
	// No orphan run may survive its thread (§2.5).
	var runs int64
	f.store.DB.Model(&persistence.AgentRun{}).Where("user_id = ?", f.user.ID).Count(&runs)
	if runs != 0 {
		t.Fatalf("%d runs survived the thread deletion", runs)
	}
}

func TestThreadRenameAndCustomAreIndependent(t *testing.T) {
	upstream := newFakeUpstream(t, scriptedTurn{text: "回答"})
	f, svc := harnessFixture(t, upstream)
	ctx := context.Background()

	created, err := svc.CreateAgentThread(ctx, f.user.ID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := svc.UpdateAgentThread(ctx, f.user.ID, created.ID, strPtr("论文拆解"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Title != "论文拆解" {
		t.Fatalf("title=%q", renamed.Title)
	}
	if renamed.Custom != "" {
		t.Fatalf("custom=%q, want it untouched by a rename", renamed.Custom)
	}
	// Custom is display data the client owns; a rename must not clear it (§2.3).
	withCustom, err := svc.UpdateAgentThread(ctx, f.user.ID, created.ID, nil, strPtr(`{"pinned":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if withCustom.Custom != `{"pinned":true}` || withCustom.Title != "论文拆解" {
		t.Fatalf("thread=%#v", withCustom)
	}
	// An over-long title is truncated rather than rejected, and an over-long custom blob is refused.
	long, err := svc.UpdateAgentThread(ctx, f.user.ID, created.ID, strPtr(strings.Repeat("长", 400)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := len([]rune(long.Title)); got != 120 {
		t.Fatalf("title runes=%d, want 120", got)
	}
	if _, err := svc.UpdateAgentThread(ctx, f.user.ID, created.ID, nil, strPtr(strings.Repeat("x", 5000))); err == nil {
		t.Fatal("an oversized custom blob was accepted")
	}
}

func TestThreadTitleIsDeterministicAndNeedsNoModel(t *testing.T) {
	upstream := newFakeUpstream(t, scriptedTurn{text: "回答"})
	f, svc := harnessFixture(t, upstream)
	ctx := context.Background()

	threadID := seedThread(t, svc, upstream, f.store, f.user.ID, "帮我把这周的实验排一下顺序，顺便看看有没有卡住的任务，再给一个今天就能开始的最小行动")
	callsBefore := upstream.callCount()
	want := "帮我把这周的实验排一下顺序，顺便看看有没有卡住的任务，再给一"
	title, err := svc.GenerateThreadTitle(ctx, f.user.ID, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if title != want {
		t.Fatalf("title=%q, want the first %d runes of the first user message %q", title, threadTitleRunes, want)
	}
	if upstream.callCount() != callsBefore {
		t.Fatal("generating a title called the model; §2.4 wants a deterministic rule")
	}
	// The title is stored, so a list response carries it.
	stored, err := svc.GetAgentThread(ctx, f.user.ID, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Title != want {
		t.Fatalf("stored title=%q", stored.Title)
	}

	// A thread with nothing said yet still gets a usable title.
	empty, err := svc.CreateAgentThread(ctx, f.user.ID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	generated, err := svc.GenerateThreadTitle(ctx, f.user.ID, empty.ID)
	if err != nil {
		t.Fatal(err)
	}
	if generated == "" {
		t.Fatal("an empty thread produced no title")
	}
}

func TestThreadAccessIsScopedToItsOwner(t *testing.T) {
	upstream := newFakeUpstream(t, scriptedTurn{text: "回答"})
	f, svc := harnessFixture(t, upstream)
	ctx := context.Background()

	threadID := seedThread(t, svc, upstream, f.store, f.user.ID, "私人对话")
	other := persistence.User{ID: persistence.NewID("user"), Identifier: "thread-other", PasswordHash: "h", DisplayName: "O", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: persistence.Now(), UpdatedAt: persistence.Now()}
	if err := f.store.DB.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	// A cross-user read is indistinguishable from a missing thread (§2.2, arch.md §12).
	if _, err := svc.GetAgentThread(ctx, other.ID, threadID); !persistence.IsNotFound(err) {
		t.Fatalf("cross-user read err=%v, want not-found", err)
	}
	if _, err := svc.UpdateAgentThread(ctx, other.ID, threadID, strPtr("偷来的标题"), nil); !persistence.IsNotFound(err) {
		t.Fatalf("cross-user rename err=%v, want not-found", err)
	}
	if err := svc.DeleteAgentThread(ctx, other.ID, threadID); !persistence.IsNotFound(err) {
		t.Fatalf("cross-user delete err=%v, want not-found", err)
	}
	page, err := svc.ListAgentThreads(ctx, other.ID, "", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("another user's threads leaked into the list: %#v", page.Items)
	}
	// The owner's thread is untouched.
	if _, err := svc.GetAgentThread(ctx, f.user.ID, threadID); err != nil {
		t.Fatalf("the owner lost their thread: %v", err)
	}
}

func strPtr(value string) *string { return &value }
