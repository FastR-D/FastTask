package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/agent/protocol"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

// The model proxy (doc/harness.md §4.3, §4.4).
//
// One endpoint, OpenAI-compatible, identical for both hosts. It does four things and nothing
// else: inject the credentials the host never sees, replace the system prompt and the tool
// list with the server's own, write the authoritative transcript as the bytes go past, and
// forward those bytes unchanged. There is no second network hop and no client-supplied
// transcript: the request body is read for the messages it carries and then discarded, which
// is also what makes a forged history impossible (§4.4.1 item 3).

// harnessProxy is the proxy itself. It is its own unit so the harness stays inside the
// wiring.md §9 method budget per struct.
type harnessProxy struct{ harnessBase }

// proxyHTTPClient has no total timeout: a streamed model call is bounded by the run's wall
// clock and by the caller's context, not by a client deadline.
var proxyHTTPClient = &http.Client{}

// cancelCheckInterval is how often a streaming proxy call re-reads the run row to notice a
// cancellation. It is the third of §5.2's three channels: the upstream stream is closed under
// the host, whose fetch then aborts.
const cancelCheckInterval = 500 * time.Millisecond

// ProxyError is a refused or failed proxy call. Code is one of the documented error codes
// (doc/interface.md §20); Status is the HTTP status the handler must answer with.
type ProxyError struct {
	Code   string
	Status int
	Detail string
	// Fatal marks an error that already ended the run, so the handler does not have to guess
	// whether the host may retry.
	Fatal bool
}

func (e *ProxyError) Error() string { return e.Code + ": " + e.Detail }

// ModelProxyStream is one proxied model call. The handler copies Body to the client; reading
// it is what writes the transcript, so a handler that discards the body would lose the run's
// authoritative record.
type ModelProxyStream struct {
	StatusCode  int
	ContentType string
	Body        io.ReadCloser
}

// errRunCancelled ends a proxied stream because the user cancelled (§5.2).
var errRunCancelled = errors.New("run cancelled while the model was streaming")

// OpenModelProxy performs one proxied chat completion for a harness run.
func (s *harnessProxy) OpenModelProxy(ctx context.Context, p HarnessPrincipal, body []byte) (*ModelProxyStream, error) {
	run, err := s.lifecycle().activeRun(ctx, p)
	if err != nil {
		return nil, s.proxyError(err)
	}
	if run.CancelRequested || run.Status == persistence.RunCancelling {
		return nil, &ProxyError{Code: "CANCELLED", Status: http.StatusConflict, Detail: "the run was cancelled", Fatal: true}
	}
	now := s.clock()
	if s.lifecycle().wallClockExceeded(run, now) {
		// The wall clock is a server-side limit even though the host drives the loop (§1.1).
		_ = s.lifecycle().finishRun(ctx, run, persistence.RunFailed, "RUN_TIMEOUT", "run wall clock exceeded", protocol.ReasonRunTimeout)
		return nil, &ProxyError{Code: "RUN_TIMEOUT", Status: http.StatusConflict, Detail: "run wall clock exceeded", Fatal: true}
	}
	if limit := s.svc.limits.MaxTurns; limit > 0 && run.ModelCalls >= limit {
		// Turn budget spent. The user is told in the transcript and the run ends cleanly, as
		// the in-process loop used to (agent-impl.md §6): a budget is not an error.
		if err := s.emitTurnLimitNotice(ctx, run); err != nil {
			return nil, &ProxyError{Code: "STATE_ERROR", Status: http.StatusInternalServerError, Detail: err.Error(), Fatal: true}
		}
		return nil, &ProxyError{Code: "MAX_TURNS", Status: http.StatusConflict, Detail: "turn budget exhausted", Fatal: true}
	}

	creds, err := s.svc.credentials(ctx)
	if err != nil {
		return nil, &ProxyError{Code: "PROVIDER_ERROR", Status: http.StatusBadGateway, Detail: "the model provider could not be resolved"}
	}
	if creds == nil || creds.BaseURL == "" || creds.APIKey == "" || creds.Model == "" {
		return nil, &ProxyError{Code: "HARNESS_UNAVAILABLE", Status: http.StatusServiceUnavailable, Detail: "no model is configured"}
	}

	payload, err := s.rewriteRequest(ctx, body, creds, run)
	if err != nil {
		return nil, err
	}
	upstream := strings.TrimRight(creds.BaseURL, "/") + "/chat/completions"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream, bytes.NewReader(payload))
	if err != nil {
		return nil, &ProxyError{Code: "PROVIDER_ERROR", Status: http.StatusBadGateway, Detail: "the upstream request could not be built"}
	}
	request.Header.Set("Authorization", "Bearer "+creds.APIKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")

	response, err := proxyHTTPClient.Do(request)
	if err != nil {
		// The host may retry (libfx does, once). A transport failure is not the run's fault, so
		// the run stays open and the host decides what to report (§3.6).
		return nil, &ProxyError{Code: "PROVIDER_ERROR", Status: http.StatusBadGateway, Detail: "the model provider could not be reached"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		detail := s.sanitize(agent.Truncate(string(raw), 300), creds)
		if agent.LooksLikeNoToolSupport(response.StatusCode, detail) {
			// Permanent: a model that cannot take tools can never run this agent, and
			// degrading to a single-turn reply would mislead the user (§5.1.1).
			_ = s.lifecycle().finishRun(ctx, run, persistence.RunFailed, "PROVIDER_NO_TOOL_SUPPORT", detail, protocol.ReasonError)
			return nil, &ProxyError{Code: "PROVIDER_NO_TOOL_SUPPORT", Status: http.StatusBadGateway, Detail: detail, Fatal: true}
		}
		return nil, &ProxyError{Code: "PROVIDER_ERROR", Status: response.StatusCode, Detail: detail}
	}

	sess, err := s.lifecycle().loadRunSession(ctx, run)
	if err != nil {
		response.Body.Close()
		return nil, &ProxyError{Code: "STATE_ERROR", Status: http.StatusInternalServerError, Detail: err.Error()}
	}
	if sess == nil {
		response.Body.Close()
		return nil, &ProxyError{Code: "STATE_ERROR", Status: http.StatusInternalServerError, Detail: "the run has no assistant message to write into"}
	}
	transcript := s.newTranscript(ctx, sess, run)
	contentType := response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "text/event-stream"
	}
	return &ModelProxyStream{
		StatusCode:  response.StatusCode,
		ContentType: contentType,
		Body:        &proxyReader{transcript: transcript, upstream: response.Body, parser: agent.NewStreamParser(transcript)},
	}, nil
}

// proxyError maps a lifecycle refusal onto the documented proxy error codes.
func (s *harnessProxy) proxyError(err error) error {
	switch {
	case errors.Is(err, ErrHarnessToken):
		return &ProxyError{Code: "HARNESS_TOKEN_INVALID", Status: http.StatusUnauthorized, Detail: "the run capability is not valid"}
	case errors.Is(err, ErrRunNotActive):
		return &ProxyError{Code: "RUN_NOT_ACTIVE", Status: http.StatusConflict, Detail: "the run is no longer active", Fatal: true}
	case persistence.IsNotFound(err):
		return &ProxyError{Code: "RUN_NOT_FOUND", Status: http.StatusNotFound, Detail: "the run does not exist"}
	default:
		return &ProxyError{Code: "INTERNAL", Status: http.StatusInternalServerError, Detail: "the model call could not be prepared"}
	}
}

// sanitize removes any trace of the injected credentials from a message that is about to be
// returned to a host (§4.5, §14.17). A provider error body should not contain them, and this
// makes sure a misconfigured proxy cannot leak one anyway.
func (s *harnessProxy) sanitize(detail string, creds *ModelCredentials) string {
	for _, secret := range []string{creds.APIKey, creds.BaseURL, creds.Model} {
		if secret == "" {
			continue
		}
		detail = strings.ReplaceAll(detail, secret, "[redacted]")
	}
	return detail
}

// rewriteRequest turns a host's OpenAI-compatible request into the one the provider should
// see (§4.3). Everything that guards an invariant is replaced rather than merged: the model,
// the system prompt and the tool list. Everything else — temperature, the reasoning level,
// provider options — is passed through, because none of it can weaken a guarantee.
// rewriteRequest returns the request the provider should see, or a *ProxyError. The error is
// returned as a plain error interface on purpose: a typed nil *ProxyError assigned to an error
// variable is a non-nil interface, which is the classic way a Go proxy ends up refusing a call
// it just accepted.
func (s *harnessProxy) rewriteRequest(ctx context.Context, body []byte, creds *ModelCredentials, run *persistence.AgentRun) ([]byte, error) {
	payload := map[string]any{}
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 {
		if err := json.Unmarshal(trimmed, &payload); err != nil {
			return nil, &ProxyError{Code: "INVALID_REQUEST", Status: http.StatusBadRequest, Detail: "the request body is not valid JSON"}
		}
	}
	instructions, _, err := s.svc.harnessService.harnessTokens.instructionsFor(ctx, run)
	if err != nil {
		return nil, &ProxyError{Code: "STATE_ERROR", Status: http.StatusInternalServerError, Detail: "the system prompt could not be built"}
	}

	// 1. Credentials: the host's copies are discarded, the provider's are injected here and
	//    never leave this process (§4.5).
	payload["model"] = creds.Model
	for _, key := range []string{"base_url", "baseURL", "api_key", "apiKey", "authorization"} {
		delete(payload, key)
	}
	// The transcript is written by parsing the stream, so a non-streaming call would be
	// invisible to it. libfx always streams (§4.2).
	payload["stream"] = true

	// 2. System prompt and tools are the server's, not the host's (§4.3, agent-impl.md §6). Images are
	//    expanded here rather than in the host, so every one of them passes an ownership check
	//    (doc/chat-features.md §4.4).
	payload["messages"] = s.sanitizeMessages(ctx, run.UserID, payload["messages"], instructions)
	payload["tools"] = agent.WireTools(s.svc.tools.Definitions(ToolReadonly, ToolProposal))
	payload["tool_choice"] = "auto"

	// 3. The reasoning level is the host's choice but must stay inside the enum
	//    (doc/chat-features.md §3.2).
	if raw, ok := payload["reasoning"]; ok {
		payload["reasoning"] = normalizeReasoningLevel(fmt.Sprint(raw))
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, &ProxyError{Code: "INVALID_REQUEST", Status: http.StatusBadRequest, Detail: "the request could not be re-encoded"}
	}
	return encoded, nil
}

// sanitizeMessages rebuilds the message list: the server's system prompt first and only, no
// other system or developer message, and no inline image data.
//
// Dropping host-supplied system messages is what makes a prompt injection through the request
// body impossible; the history itself is still forwarded, because that is what lets a run
// continue across turns. It is never written to the database (§4.4.1 item 3).
func (s *harnessProxy) sanitizeMessages(ctx context.Context, userID string, raw any, instructions string) []any {
	list, _ := raw.([]any)
	out := make([]any, 0, len(list)+1)
	out = append(out, map[string]any{"role": "system", "content": instructions})
	for _, item := range list {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		if role == "system" || role == "developer" {
			continue
		}
		cleaned := make(map[string]any, len(message))
		for key, value := range message {
			if key == "content" {
				cleaned[key] = s.sanitizeContent(ctx, userID, value)
				continue
			}
			cleaned[key] = value
		}
		out = append(out, cleaned)
	}
	return out
}

// sanitizeContent strips inline image data from a message's content parts. Images reach a
// model only as attachment references, expanded server-side after an ownership check
// (doc/chat-features.md §4.4); base64 a host sent directly is dropped, so there is no path
// into the provider that skips that check.
func (s *harnessProxy) sanitizeContent(ctx context.Context, userID string, content any) any {
	parts, ok := content.([]any)
	if !ok {
		return content
	}
	out := make([]any, 0, len(parts))
	for _, item := range parts {
		part, ok := item.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := part["type"].(string)
		if kind != "image_url" && kind != "image" && kind != "input_image" {
			out = append(out, part)
			continue
		}
		reference := imageReference(part)
		attachmentID, ok := AttachmentIDFromRef(reference)
		if !ok {
			// Inline data, a foreign URL, or nothing at all: dropped. An image reaches the model only as
			// a reference this server issued and can ownership-check (§4.4).
			continue
		}
		dataURI, ok := s.svc.attachments.DataURI(ctx, userID, attachmentID)
		if !ok {
			continue
		}
		expanded := map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": dataURI},
		}
		out = append(out, expanded)
	}
	return out
}

// imageReference pulls the URL out of whichever shape an SDK used for an image part.
func imageReference(part map[string]any) string {
	switch value := part["image_url"].(type) {
	case string:
		return value
	case map[string]any:
		if url, _ := value["url"].(string); url != "" {
			return url
		}
	}
	if url, _ := part["image"].(string); url != "" {
		return url
	}
	url, _ := part["url"].(string)
	return url
}

// emitTurnLimitNotice tells the user the run stopped because the turn budget ran out, then
// ends the run cleanly (§6). It replaces the in-process loop's final notice.
func (s *harnessProxy) emitTurnLimitNotice(ctx context.Context, run *persistence.AgentRun) error {
	sess, err := s.lifecycle().loadRunSession(ctx, run)
	if err != nil {
		return err
	}
	if sess == nil {
		return s.lifecycle().finishRun(ctx, run, persistence.RunSucceeded, "", "", protocol.ReasonMaxTurns)
	}
	notice := "（已达到单次运行的工具调用轮次上限，先在此收尾。如需继续，请再发一条消息。）"
	idx, err := sess.addTextPart()
	if err != nil {
		return err
	}
	if err := sess.appendTextToPart(idx, notice); err != nil {
		return err
	}
	if err := sess.persistTextPart(idx, notice); err != nil {
		return err
	}
	sess.turns = run.ModelCalls
	if _, err := sess.succeed(); err != nil {
		return err
	}
	_, _ = s.repo().RevokeHarnessTokensForRun(ctx, run.ID)
	return nil
}

// --- the transcribing reader (§4.4) ---

// proxyReader forwards upstream bytes to the client while feeding them to the transcript.
// One read, two consumers: that is why there is no extra round trip and no chance for the
// forwarded stream and the stored transcript to disagree.
type proxyReader struct {
	transcript *harnessTranscript
	upstream   io.ReadCloser
	parser     *agent.StreamParser
	lastCheck  time.Time
	closed     bool
}

func (r *proxyReader) Read(p []byte) (int, error) {
	n, err := r.upstream.Read(p)
	if n > 0 {
		if feedErr := r.parser.Feed(r.transcript.ctx, p[:n]); feedErr != nil {
			r.transcript.fail(feedErr)
			return n, feedErr
		}
		if r.cancelled() {
			r.transcript.fail(errRunCancelled)
			return n, errRunCancelled
		}
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			r.transcript.finish(r.parser)
		} else {
			r.transcript.fail(err)
		}
	}
	return n, err
}

// Close ends the transcript exactly once, whatever happened to the copy loop.
func (r *proxyReader) Close() error {
	if !r.closed {
		r.closed = true
		r.transcript.finish(r.parser)
	}
	return r.upstream.Close()
}

// cancelled re-reads the run row at most every cancelCheckInterval, so a cancellation closes
// the upstream stream under the host without a query per byte (§5.2 channel 3).
func (r *proxyReader) cancelled() bool {
	now := time.Now()
	if now.Sub(r.lastCheck) < cancelCheckInterval {
		return false
	}
	r.lastCheck = now
	run, err := r.transcript.svc.repo().GetRun(r.transcript.ctx, r.transcript.run.UserID, r.transcript.run.ID)
	if err != nil {
		return false
	}
	return run.CancelRequested || run.Status == persistence.RunCancelling
}

// harnessTranscript writes the authoritative transcript of one proxied model call
// (doc/harness.md §4.4). It implements agent.StreamSink, so the same bytes that are forwarded
// produce the persisted parts and the chunks the UI renders from.
//
// Fragments are tracked per part rather than assumed nested (§4.4.1): text, reasoning and
// several tool calls may interleave in any order.
type harnessTranscript struct {
	svc  *harnessProxy
	ctx  context.Context
	sess *runSession
	run  *persistence.AgentRun

	textID  string
	textIdx int
	text    strings.Builder

	reasoningID  string
	reasoningIdx int
	reasoning    strings.Builder
	reasoningSeq int

	calls     map[int]*pendingToolPart
	callOrder []int

	done     bool
	failed   bool
	finishRS string
}

type pendingToolPart struct {
	id     string
	name   string
	args   strings.Builder
	partID string
	idx    int
}

func (s *harnessProxy) newTranscript(ctx context.Context, sess *runSession, run *persistence.AgentRun) *harnessTranscript {
	return &harnessTranscript{
		svc: s, ctx: ctx, sess: sess, run: run,
		textIdx: -1, reasoningIdx: -1, calls: map[int]*pendingToolPart{},
	}
}

// TextDelta streams model text into a text part, creating the part on the first delta so its
// index is claimed before anything can reference it (§2.7.1).
func (t *harnessTranscript) TextDelta(ctx context.Context, delta string) error {
	if delta == "" {
		return nil
	}
	if t.textID == "" {
		part, idx, err := t.createPart("text", "", "", "")
		if err != nil {
			return err
		}
		t.textID, t.textIdx = part.ID, idx
	}
	t.text.WriteString(delta)
	t.sess.state.Messages[t.sess.assistantIdx].Parts[t.textIdx].Text += delta
	op, err := protocol.AppendText(protocol.PartTextPath(t.sess.assistantIdx, t.textIdx), delta)
	if err != nil {
		return err
	}
	return t.sess.sink.Emit(ctx, protocol.UpdateState(op))
}

// ReasoningDelta streams a chain of thought into a reasoning part. When persistence is off the
// deltas are dropped entirely — not stored, not streamed — because the point of the switch is
// that the text never lands anywhere (doc/chat-features.md §3.4).
func (t *harnessTranscript) ReasoningDelta(ctx context.Context, delta string) error {
	if delta == "" || !t.svc.svc.reasoningPersisted {
		return nil
	}
	if t.reasoningID == "" {
		t.reasoningSeq++
		id := fmt.Sprintf("reasoning_%s_%d", t.run.ID, t.reasoningSeq)
		part, idx, err := t.createPart("reasoning", id, "", "")
		if err != nil {
			return err
		}
		t.reasoningID, t.reasoningIdx = part.ID, idx
	}
	t.reasoning.WriteString(delta)
	part := &t.sess.state.Messages[t.sess.assistantIdx].Parts[t.reasoningIdx]
	part.Text += delta
	op, err := protocol.AppendText(protocol.PartTextPath(t.sess.assistantIdx, t.reasoningIdx), delta)
	if err != nil {
		return err
	}
	return t.sess.sink.Emit(ctx, protocol.UpdateState(op))
}

// ToolCallDelta records a call the model is making. The part is created as soon as the call is
// named, with empty arguments, because the host may reach the tool endpoint before this stream
// ends and the result has to land on an existing row (§4.4.1).
func (t *harnessTranscript) ToolCallDelta(ctx context.Context, delta agent.ToolCallDelta) error {
	slot := t.calls[delta.Index]
	if slot == nil {
		slot = &pendingToolPart{}
		t.calls[delta.Index] = slot
		t.callOrder = append(t.callOrder, delta.Index)
	}
	if delta.ID != "" {
		slot.id = delta.ID
	}
	if delta.Name != "" {
		slot.name = delta.Name
	}
	slot.args.WriteString(delta.Arguments)
	if slot.partID == "" && slot.name != "" {
		part, idx, err := t.createPart("tool-call", slot.id, slot.name, "{}")
		if err != nil {
			return err
		}
		slot.partID, slot.idx = part.ID, idx
		if slot.id == "" {
			slot.id = part.ID
		}
	}
	return nil
}

// Finish records the provider's finish reason. The run does not end here: a turn may continue
// with tool calls, and only the host knows when it is over (§5.1).
func (t *harnessTranscript) Finish(_ context.Context, reason string) error {
	t.finishRS = reason
	return nil
}

// createPart claims the next part index and writes the row, then emits the set that establishes
// the part for the client (§2.6). Index allocation goes through the database so the tool execution
// surface cannot take the same one.
//
// A tool-call part is keyed by its call id, and the same id can arrive twice: a host may execute a
// call before the proxy finished reading the stream, so the tool surface can have created the row
// first (§4.4.1). Reusing it is what keeps (run_id, tool_call_id) unique.
func (t *harnessTranscript) createPart(kind, id, name, argsJSON string) (*persistence.AgentMessagePart, int, error) {
	if kind == "tool-call" && id != "" {
		if existing, err := t.svc.repo().GetPartByToolCallID(t.ctx, t.run.UserID, id); err == nil {
			t.mirrorPart(existing)
			return existing, existing.Idx, nil
		}
	}
	part := &persistence.AgentMessagePart{
		UserID: t.run.UserID, MessageID: t.sess.assistantID, Type: kind, ArgsJSON: "{}",
	}
	switch kind {
	case "tool-call":
		callID := id
		if callID == "" {
			callID = persistence.NewID("call")
		}
		part.ToolCallID = &callID
		part.ToolName = name
		part.ArgsJSON = argsJSON
	case "reasoning":
		part.ID = id
	}
	err := t.svc.svc.store.Transaction(t.ctx, func(tx *gorm.DB) error {
		repo := t.svc.repo().WithTx(tx)
		idx, err := repo.NextPartIdx(t.ctx, t.run.UserID, t.sess.assistantID)
		if err != nil {
			return err
		}
		part.Idx = idx
		if err := repo.CreatePartAtIdx(t.ctx, part); err != nil {
			if persistence.IsUniqueViolation(err) && part.ToolCallID != nil {
				// The tool surface won the race; adopt its row rather than duplicating the call.
				found, lookupErr := repo.GetPartByToolCallID(t.ctx, t.run.UserID, *part.ToolCallID)
				if lookupErr != nil {
					return lookupErr
				}
				*part = *found
				return nil
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	t.mirrorPart(part)
	return part, part.Idx, nil
}

// mirrorPart puts a persisted part into the session state at its own index and emits it, so the
// client sees the part the server just wrote.
func (t *harnessTranscript) mirrorPart(part *persistence.AgentMessagePart) {
	parts := &t.sess.state.Messages[t.sess.assistantIdx].Parts
	for len(*parts) <= part.Idx {
		*parts = append(*parts, protocol.TextPart(""))
	}
	(*parts)[part.Idx] = partFromRow(part)
	_ = t.sess.emitSet(protocol.PartPath(t.sess.assistantIdx, part.Idx), (*parts)[part.Idx])
}

// finish flushes the transcript at the end of a stream: the accumulated text goes onto its
// rows, the tool calls get their complete arguments, and the state snapshot is saved.
func (t *harnessTranscript) finish(parser *agent.StreamParser) {
	if t.done {
		return
	}
	t.done = true
	ctx := t.ctx
	if t.textID != "" {
		_ = t.svc.repo().UpdatePartText(ctx, t.run.UserID, t.textID, t.text.String())
	}
	if t.reasoningID != "" {
		_ = t.svc.repo().UpdatePartText(ctx, t.run.UserID, t.reasoningID, t.reasoning.String())
	}
	for _, index := range t.callOrder {
		slot := t.calls[index]
		if slot == nil || slot.partID == "" {
			continue
		}
		args := slot.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		if err := t.svc.repo().UpdatePartArgs(ctx, t.run.UserID, slot.partID, args); err != nil {
			continue
		}
		// Re-emit so the client sees the arguments the model settled on.
		if part, err := t.svc.repo().GetPart(ctx, t.run.UserID, slot.partID); err == nil {
			updated := partFromRow(part)
			if slot.idx < len(t.sess.assistantParts()) {
				t.sess.state.Messages[t.sess.assistantIdx].Parts[slot.idx] = updated
			}
			_ = t.sess.emitSet(protocol.PartPath(t.sess.assistantIdx, slot.idx), updated)
		}
	}
	if parser != nil {
		if reason := parser.Result().FinishReason; reason != "" {
			t.finishRS = reason
		}
	}
	_ = t.sess.saveState()
	// One proxied call is one turn of the budget (§6). Counting it here rather than at entry
	// means a call that never produced a stream does not consume the budget.
	if _, err := t.svc.repo().CountModelCall(ctx, t.run.UserID, t.run.ID); err == nil {
		t.run.ModelCalls++
	}
}

// fail flushes what was written before a stream broke. A partial transcript is kept rather
// than discarded: the user sees what the model said before the failure (§4.2 of
// agent-impl.md).
func (t *harnessTranscript) fail(cause error) {
	if t.failed || t.done {
		return
	}
	t.failed = true
	t.finish(nil)
	_ = cause
}
