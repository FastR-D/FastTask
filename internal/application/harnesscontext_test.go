package application

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FastR-D/FastTask/internal/persistence"
)

func TestCanonicalContextSurvivesStaleCheckpointAndLateDecisions(t *testing.T) {
	store, owner, _ := panelTestStore(t)
	svc := NewAgentService(New(store))
	ctx := context.Background()
	thread, err := svc.repo.CreateThread(ctx, owner, nil, "history")
	if err != nil {
		t.Fatal(err)
	}
	run := &persistence.AgentRun{UserID: owner, ThreadID: thread.ID, Status: persistence.RunRunning, HarnessMode: HarnessModeWASM}
	if err = svc.repo.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	add := func(role string, parts ...persistence.AgentMessagePart) *persistence.AgentMessage {
		t.Helper()
		m := &persistence.AgentMessage{UserID: owner, ThreadID: thread.ID, RunID: run.ID, Role: role}
		if err := svc.repo.CreateMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		for i, p := range parts {
			p.UserID = owner
			p.MessageID = m.ID
			p.Idx = i
			if err := svc.repo.CreatePart(ctx, &p); err != nil {
				t.Fatal(err)
			}
		}
		return m
	}
	add("user", persistence.AgentMessagePart{Type: "text", Text: "保留原方向，不增加字数指标"})
	call := "history-call"
	add("assistant", persistence.AgentMessagePart{Type: "text", Text: "读取前"}, persistence.AgentMessagePart{Type: "tool-call", ToolCallID: &call, ToolName: "get_task_tree", ArgsJSON: `{"goal_id":"g"}`, ResultJSON: `{"revision":7}`}, persistence.AgentMessagePart{Type: "text", Text: "读取后"})
	current := add("user", persistence.AgentMessagePart{Type: "text", Text: "继续"})
	run.ParentMessageID = &current.ID
	if err = svc.repo.SaveThreadCheckpoint(ctx, owner, thread.ID, []byte("damaged-old-runtime"), "old"); err != nil {
		t.Fatal(err)
	}
	first, err := svc.modelContext(ctx, run, "system")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(first)
	s := string(encoded)
	if !strings.Contains(s, "保留原方向") || !strings.Contains(s, "读取后") {
		t.Fatal(s)
	}
	if !(strings.Index(s, "读取前") < strings.Index(s, "history-call") && strings.Index(s, "history-call") < strings.Index(s, "读取后")) {
		t.Fatalf("step order lost: %s", s)
	}
	part, err := svc.repo.GetPartByToolCallID(ctx, owner, call)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.repo.UpdatePartResult(ctx, owner, part.ID, `{"applied":true,"revision":8}`, false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		next, err := svc.modelContext(ctx, run, "system")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(next)
		if !strings.Contains(string(b), `\"applied\":true`) {
			t.Fatalf("late result lost: %s", b)
		}
	}
	cp, err := svc.GetThreadCheckpoint(ctx, HarnessPrincipal{UserID: owner, ThreadID: thread.ID, RunID: run.ID})
	if err != nil || len(cp.Checkpoint) != 0 {
		t.Fatalf("opaque checkpoint restored: %+v %v", cp, err)
	}
	instructions, _, err := svc.instructionsFor(ctx, run)
	if err != nil || strings.Contains(instructions, "保留原方向") {
		t.Fatal("history elevated to system")
	}
}

func TestInstructionLimitIncludesMarker(t *testing.T) {
	for _, value := range []string{strings.Repeat("a", InstructionsLimit+1), strings.Repeat("中", InstructionsLimit), strings.Repeat("a", InstructionsLimit)} {
		got := truncateInstructions(value)
		if len(got) > InstructionsLimit || !json.Valid([]byte(`"`+strings.ReplaceAll(got, "\n", `\n`)+`"`)) {
			t.Fatal("invalid or oversized instructions")
		}
	}
}
