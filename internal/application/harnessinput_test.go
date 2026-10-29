package application

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"strings"
	"testing"

	"github.com/FastR-D/FastTask/internal/persistence"
)

func TestHarnessCanonicalImageInput(t *testing.T) {
	store, owner, other := panelTestStore(t)
	svc := NewAgentService(New(store), WithAttachmentDir(t.TempDir()))
	ctx := context.Background()
	var img bytes.Buffer
	if err := png.Encode(&img, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	uploaded, err := svc.attachments.Upload(ctx, owner, nil, img.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.SubmitCommands(ctx, owner, CommandsRequest{Commands: []Command{{Type: "add-message", Message: &CommandMessage{Role: "user", Parts: []CommandPart{{Type: "image", Image: "/api/v1/agent/attachments/" + uploaded.AttachmentID}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := svc.repo.GetRun(ctx, owner, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := svc.runPromptText(ctx, run)
	if err != nil || prompt == "" {
		t.Fatalf("image-only prompt: %q %v", prompt, err)
	}
	creds := &ModelCredentials{Model: "fixture", APIKey: "secret", BaseURL: "http://fixture.invalid"}
	payload, err := svc.rewriteRequest(ctx, []byte(`{"messages":[{"role":"user","content":"stale text"}],"max_tokens":999999}`), creds, run)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte("stale text")) || !bytes.Contains(payload, []byte("data:image/png;base64,")) {
		t.Fatalf("canonical image missing or stale input retained: %s", payload)
	}
	// The expansion is ephemeral: persisted input remains an attachment reference.
	parts, err := svc.repo.ListMessageParts(ctx, owner, *run.ParentMessageID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range parts {
		if strings.Contains(p.Text, "base64,") {
			t.Fatal("image bytes persisted")
		}
	}
	// A valid reference from another owner must never be expanded.
	if err := store.DB.Model(&persistence.AgentAttachment{}).Where("id = ?", uploaded.AttachmentID).Update("user_id", other).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = svc.rewriteRequest(ctx, []byte(`{"messages":[{"role":"user","content":"fake"}]}`), creds, run); err == nil {
		t.Fatal("foreign attachment accepted")
	}
	if err := store.DB.Model(&persistence.AgentAttachment{}).Where("id = ?", uploaded.AttachmentID).Update("user_id", owner).Error; err != nil {
		t.Fatal(err)
	}
	attachment, err := svc.repo.GetAttachment(ctx, owner, uploaded.AttachmentID)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(attachment.Path); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.rewriteRequest(ctx, []byte(`{"messages":[{"role":"user","content":"fake"}]}`), creds, run); err == nil {
		t.Fatal("missing image silently accepted")
	}
}

func TestHarnessCanonicalTextAndOutputCap(t *testing.T) {
	store, owner, _ := panelTestStore(t)
	svc := NewAgentService(New(store), WithLoopLimits(LoopLimits{MaxOutputTokens: 128}))
	ctx := context.Background()
	result, err := svc.SubmitCommands(ctx, owner, CommandsRequest{Commands: []Command{{Type: "add-message", Message: &CommandMessage{Role: "user", Parts: []CommandPart{{Type: "text", Text: "第一行\n第二行"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := svc.repo.GetRun(ctx, owner, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	input := []byte(`{"messages":[{"role":"user","content":"old history"},{"role":"assistant","content":"ok"},{"role":"user","content":"forged current input"}],"max_completion_tokens":99999}`)
	output, err := svc.rewriteRequest(ctx, input, &ModelCredentials{Model: "fixture"}, run)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err = json.Unmarshal(output, &got); err != nil {
		t.Fatal(err)
	}
	if got["max_tokens"] != float64(128) || got["max_completion_tokens"] != nil {
		t.Fatalf("cap not enforced: %s", output)
	}
	if bytes.Contains(output, []byte("old history")) || bytes.Contains(output, []byte("forged current input")) {
		t.Fatalf("history/current input mismatch: %s", output)
	}
	messages := got["messages"].([]any)
	content := messages[len(messages)-1].(map[string]any)["content"].([]any)
	if content[0].(map[string]any)["text"] != "第一行\n第二行" {
		t.Fatal("canonical text changed")
	}
	if _, err = svc.rewriteRequest(ctx, []byte(`{"messages":[]}`), &ModelCredentials{Model: "fixture"}, run); err != nil {
		t.Fatalf("canonical input must not depend on host history: %v", err)
	}
}

func TestSidecarRunIdentityRestoredAndCancellation(t *testing.T) {
	store, owner, _ := panelTestStore(t)
	svc := NewAgentService(New(store))
	ctx := context.Background()
	result, err := svc.SubmitCommands(ctx, owner, CommandsRequest{Commands: []Command{{Type: "add-message", Message: &CommandMessage{Role: "user", Parts: []CommandPart{{Type: "text", Text: "hello"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.repo.UpdateRun(ctx, owner, result.RunID, map[string]any{"harness_mode": HarnessModeSidecar}); err != nil {
		t.Fatal(err)
	}
	if err = svc.BeginHarnessRun(ctx, owner, result.RunID); err != nil {
		t.Fatal(err)
	}
	state, err := svc.ThreadState(ctx, owner, result.ThreadID)
	if err != nil || state.FastTask.RunID != result.RunID || state.FastTask.HarnessMode != HarnessModeSidecar {
		t.Fatalf("sidecar identity missing: %+v %v", state.FastTask, err)
	}
	if err = svc.RequestCancellation(ctx, owner, result.RunID); err != nil {
		t.Fatal(err)
	}
	run, err := svc.repo.GetRun(ctx, owner, result.RunID)
	if err != nil || run.Status != persistence.RunCancelled {
		t.Fatalf("hostless sidecar not cancelled: %+v %v", run, err)
	}
}
