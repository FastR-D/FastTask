package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestChunkEncoderEmitsValidJSONForEachType covers agent-impl.md §10 "chunk
// 编码器对每种类型产出合法 JSON，path 规则正确": every emittable chunk type
// serializes to valid JSON, and the path-required types always carry a path
// while optional ones may omit it.
func TestChunkEncoderEmitsValidJSONForEachType(t *testing.T) {
	cases := []struct {
		name       string
		chunk      Chunk
		wantType   ChunkType
		pathExists bool
	}{
		{"update-state", UpdateState(mustSet(t, []string{"isRunning"}, true)), ChunkUpdateState, false},
		{"step-start", StepStart(), ChunkStepStart, false},
		{"step-finish", StepFinish(ReasonStop), ChunkStepFinish, false},
		{"message-finish", MessageFinish(ReasonStop), ChunkMessageFinish, false},
		{"error", ErrorChunk(), ChunkError, false},
		{"part-start", PartStart(TextPart("hi")), ChunkPartStart, false},
		{"annotations", Annotations([]any{}), ChunkAnnotations, false},
		{"data", Data([]any{}), ChunkData, false},
		{"text-delta", TextDelta([]int{0, 1}, "x"), ChunkTextDelta, true},
		{"part-finish", PartFinish([]int{0}), ChunkPartFinish, true},
		{"tool-call-args-text-finish", ToolCallArgsTextFinish([]int{2}), ChunkToolCallArgsTextFinish, true},
		{"result", Result([]int{3}, false), ChunkResult, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.chunk.Validate(); err != nil {
				t.Fatalf("valid chunk rejected: %v", err)
			}
			encoded, err := json.Marshal(tc.chunk)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("chunk is not valid JSON (%s): %v", encoded, err)
			}
			if decoded["type"] != string(tc.wantType) {
				t.Fatalf("type=%v, want %v", decoded["type"], tc.wantType)
			}
			_, hasPath := decoded["path"]
			if hasPath != tc.pathExists {
				t.Fatalf("path presence=%v, want %v (%s)", hasPath, tc.pathExists, encoded)
			}
		})
	}
}

func mustSet(t *testing.T, path []string, value any) Operation {
	t.Helper()
	op, err := Set(path, value)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

// TestValidateRejectsMalformedChunks asserts the encoder refuses frames the
// strict decoder would reject: unknown types, missing required path, missing
// required fields, and negative path segments (§2.5).
func TestValidateRejectsMalformedChunks(t *testing.T) {
	empty := []int{}
	cases := []struct {
		name  string
		chunk Chunk
	}{
		{"unknown type", Chunk{Type: ChunkType("bogus")}},
		{"text-delta missing path", Chunk{Type: ChunkTextDelta, TextDelta: "x"}},
		{"result missing path", Chunk{Type: ChunkResult}},
		{"update-state missing operations", Chunk{Type: ChunkUpdateState}},
		{"step-finish missing reason", Chunk{Type: ChunkStepFinish}},
		{"message-finish missing reason", Chunk{Type: ChunkMessageFinish}},
		{"part-start missing part", Chunk{Type: ChunkPartStart}},
		{"negative path segment", Chunk{Type: ChunkPartFinish, Path: &[]int{-1}}},
		{"operation empty path", Chunk{Type: ChunkUpdateState, Operations: []Operation{{Type: OpSet}}}},
		{"operation unknown type", Chunk{Type: ChunkUpdateState, Operations: []Operation{{Type: "nope", Path: []string{"a"}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.chunk.Validate(); err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
		_ = empty
	}
}

// TestEmptyTextPartSerializesWithTextKey is the §2.6/§2.7.1 trap: an empty text
// part MUST serialize as {"type":"text","text":""}. If "text" were omitted, the
// subsequent append-text would have no string target and the client would throw.
func TestEmptyTextPartSerializesWithTextKey(t *testing.T) {
	encoded, err := json.Marshal(TextPart(""))
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"type":"text","text":""}` {
		t.Fatalf("empty text part = %s, want {\"type\":\"text\",\"text\":\"\"}", encoded)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["text"]; !ok {
		t.Fatal("text key missing from empty text part")
	}
}

// TestToolCallPartShape asserts a tool-call part carries the fields the
// converter and approval UI read, and that args/result are objects not null
// (§2.7).
func TestToolCallPartShape(t *testing.T) {
	part := ToolCallPart("call_1", "propose_task_tree_patch", nil)
	part.Approval = &Approval{Status: ApprovalPending}
	encoded, err := json.Marshal(part)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["type"] != "tool-call" || decoded["toolCallId"] != "call_1" || decoded["toolName"] != "propose_task_tree_patch" {
		t.Fatalf("tool-call identity wrong: %s", encoded)
	}
	if args, ok := decoded["args"].(map[string]any); !ok || args == nil {
		t.Fatalf("args must be an object: %s", encoded)
	}
	approval, ok := decoded["approval"].(map[string]any)
	if !ok || approval["status"] != "pending" {
		t.Fatalf("approval not serialized: %s", encoded)
	}
	// A text part must not leak tool-call fields.
	textEncoded, _ := json.Marshal(TextPart("hello"))
	if strings.Contains(string(textEncoded), "toolCallId") {
		t.Fatalf("text part leaked tool-call fields: %s", textEncoded)
	}
}

// TestPartRoundTripsThroughJSON asserts persisted state_json can be read back:
// both part shapes survive marshal -> unmarshal (doc/agent-impl.md §3.1).
func TestPartRoundTripsThroughJSON(t *testing.T) {
	original := []Part{
		TextPart("streamed text"),
		func() Part {
			p := ToolCallPart("call_9", "propose_daily_plan", map[string]any{"candidates": []any{"a"}})
			p.Result = map[string]any{"decision": "approve"}
			p.Approval = &Approval{Status: ApprovalApproved}
			return p
		}(),
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []Part
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 2 {
		t.Fatalf("decoded %d parts, want 2", len(decoded))
	}
	if decoded[0].Type != PartText || decoded[0].Text != "streamed text" {
		t.Fatalf("text part round-trip wrong: %#v", decoded[0])
	}
	if decoded[1].Type != PartToolCall || decoded[1].ToolCallID != "call_9" || decoded[1].ToolName != "propose_daily_plan" {
		t.Fatalf("tool-call part round-trip wrong: %#v", decoded[1])
	}
	if decoded[1].Approval == nil || decoded[1].Approval.Status != ApprovalApproved {
		t.Fatalf("approval round-trip wrong: %#v", decoded[1].Approval)
	}
}

// TestStateSerializesArraysNotNull asserts the client always sees arrays for
// messages/parts/pendingProposals even when empty (§2.7). A null where an array
// is expected breaks the converter.
func TestStateSerializesArraysNotNull(t *testing.T) {
	encoded, err := json.Marshal(NewState())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "null") {
		t.Fatalf("empty state serialized a null: %s", encoded)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["messages"].([]any); !ok {
		t.Fatalf("messages not an array: %s", encoded)
	}
	fasttask, ok := decoded["fasttask"].(map[string]any)
	if !ok {
		t.Fatalf("fasttask missing: %s", encoded)
	}
	if _, ok := fasttask["pendingProposals"].([]any); !ok {
		t.Fatalf("pendingProposals not an array: %s", encoded)
	}
	// A message with nil parts still serializes parts as [].
	msgEncoded, _ := json.Marshal(Message{ID: "m1", Role: RoleAssistant, Status: RunningStatus(), CreatedAt: RFC3339(time.Now())})
	if !strings.Contains(string(msgEncoded), `"parts":[]`) {
		t.Fatalf("message parts not defaulted to []: %s", msgEncoded)
	}
}

// TestStatusShapesMatchMessageStatus asserts the four status shapes the
// converter passes through serialize exactly as assistant-ui expects (§2.7).
func TestStatusShapesMatchMessageStatus(t *testing.T) {
	running, _ := json.Marshal(RunningStatus())
	if string(running) != `{"type":"running"}` {
		t.Fatalf("running status = %s", running)
	}
	complete, _ := json.Marshal(CompleteStatus(ReasonStop))
	if string(complete) != `{"type":"complete","reason":"stop"}` {
		t.Fatalf("complete status = %s", complete)
	}
	requires, _ := json.Marshal(RequiresActionStatus())
	if string(requires) != `{"type":"requires-action","reason":"tool-calls"}` {
		t.Fatalf("requires-action status = %s", requires)
	}
}

// TestStreamWriterFraming covers agent-impl.md §10 "流以 [DONE] 结束；响应不含
// event: 行" and §2.4: each chunk is one data frame, no event: line is ever
// emitted, and the stream terminates with data: [DONE].
func TestStreamWriterFraming(t *testing.T) {
	var buf bytes.Buffer
	w := NewStreamWriter(&buf, nil)

	if err := w.WriteState(mustSet(t, IsRunningPath(), true)); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteChunk(StepStart()); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteComment("ping"); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteDone(); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if strings.Contains(out, "event:") {
		t.Fatalf("stream wrote an event: line, which breaks the strict decoder:\n%s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("stream did not end with data: [DONE]:\n%q", out)
	}
	// Heartbeat is a comment line, not a data frame.
	if !strings.Contains(out, ": ping\n\n") {
		t.Fatalf("heartbeat comment missing:\n%q", out)
	}
	// Each data frame is exactly one line ending in a blank line.
	dataFrames := strings.Count(out, "data: ")
	if dataFrames != 3 { // update-state, step-start, [DONE]
		t.Fatalf("data frame count=%d, want 3:\n%q", dataFrames, out)
	}
	// Every frame is terminated by a blank line.
	for _, frame := range strings.Split(strings.TrimRight(out, "\n"), "\n\n") {
		if !strings.HasPrefix(frame, "data: ") && !strings.HasPrefix(frame, ": ") {
			t.Fatalf("unexpected frame shape: %q", frame)
		}
	}
}

// TestStreamWriterRejectsInvalidChunk asserts the writer never emits a frame the
// decoder would drop: an invalid chunk is refused before any bytes are written.
func TestStreamWriterRejectsInvalidChunk(t *testing.T) {
	var buf bytes.Buffer
	w := NewStreamWriter(&buf, nil)
	if err := w.WriteChunk(Chunk{Type: ChunkType("bogus")}); err == nil {
		t.Fatal("expected error writing an invalid chunk")
	}
	if buf.Len() != 0 {
		t.Fatalf("invalid chunk wrote bytes: %q", buf.String())
	}
}

// TestStreamWriterFlushesPerFrame asserts flush is invoked after every frame so
// bytes reach the client immediately (§9.3).
func TestStreamWriterFlushesPerFrame(t *testing.T) {
	var buf bytes.Buffer
	flushes := 0
	w := NewStreamWriter(&buf, func() { flushes++ })
	_ = w.WriteChunk(StepStart())
	_ = w.WriteChunk(MessageFinish(ReasonStop))
	_ = w.WriteDone()
	if flushes != 3 {
		t.Fatalf("flushes=%d, want 3 (one per frame)", flushes)
	}
}

// TestAppendTextRequiresPriorSet documents the §2.6 ordering rule at the encoder
// level: the streaming sequence is set(text part) -> append-text -> set(status).
// Emitting append-text against a path with no established string is a caller
// error; this test locks the canonical ordering so a refactor cannot drop the
// initial set.
func TestAppendTextRequiresPriorSet(t *testing.T) {
	var buf bytes.Buffer
	w := NewStreamWriter(&buf, nil)

	// Canonical streaming sequence for assistant message at index 1, part 0.
	setPart := mustSet(t, PartPath(1, 0), TextPart(""))
	if err := w.WriteState(setPart); err != nil {
		t.Fatal(err)
	}
	appendOp, err := AppendText(PartTextPath(1, 0), "hello ")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteState(appendOp); err != nil {
		t.Fatal(err)
	}
	appendOp2, _ := AppendText(PartTextPath(1, 0), "world")
	if err := w.WriteState(appendOp2); err != nil {
		t.Fatal(err)
	}
	setStatus := mustSet(t, MessageStatusPath(1), CompleteStatus(ReasonStop))
	if err := w.WriteState(setStatus); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	setIdx := strings.Index(out, `"type":"set"`)
	appendIdx := strings.Index(out, `"type":"append-text"`)
	if setIdx == -1 || appendIdx == -1 || setIdx > appendIdx {
		t.Fatalf("append-text appeared before the establishing set:\n%s", out)
	}
	// The append-text value carries only the delta, not the whole string.
	if !strings.Contains(out, `"value":"hello "`) || !strings.Contains(out, `"value":"world"`) {
		t.Fatalf("append-text deltas wrong:\n%s", out)
	}
}

// TestUnsafePathSegmentsRejected asserts the client's prototype-pollution guard
// is mirrored server-side so a bad path never reaches the wire (§2.6).
func TestUnsafePathSegmentsRejected(t *testing.T) {
	for _, segment := range []string{"__proto__", "constructor", "prototype"} {
		if _, err := Set([]string{"messages", segment}, "x"); err == nil {
			t.Fatalf("Set accepted unsafe segment %q", segment)
		}
		if _, err := AppendText([]string{segment, "text"}, "x"); err == nil {
			t.Fatalf("AppendText accepted unsafe segment %q", segment)
		}
	}
}

// TestStringPathBuilders locks the exact path shapes from §2.7.1 so message
// indexing stays server-owned and stable.
func TestStringPathBuilders(t *testing.T) {
	assertPath(t, MessagePath(3), []string{"messages", "3"})
	assertPath(t, PartPath(3, 0), []string{"messages", "3", "parts", "0"})
	assertPath(t, PartTextPath(3, 0), []string{"messages", "3", "parts", "0", "text"})
	assertPath(t, MessageStatusPath(3), []string{"messages", "3", "status"})
	assertPath(t, IsRunningPath(), []string{"isRunning"})
	assertPath(t, FastTaskPath(), []string{"fasttask"})
}

func assertPath(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("path=%v, want %v", got, want)
	}
}
