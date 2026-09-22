package application

import (
	"context"
	"encoding/base64"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/persistence"
)

// The thread catalogue (doc/chat-features.md §2).
//
// A thread is what a user calls "a conversation": it owns its messages, its checkpoint and its
// attachments, and it can be renamed, archived or deleted. Nothing here is agent intelligence — the
// point of keeping it deterministic is that a thread list behaves like a file list: predictable,
// paginated, and never the product of a model call.

// threadTitleRunes is how much of the first user message becomes a title (§2.4). It matches what the
// migration used when it backfilled threads from conversations, so a renamed-by-nobody thread reads the
// same before and after the upgrade.
const threadTitleRunes = 30

// threadCatalogue is the unit AgentService embeds for thread CRUD. It is separate from the harness
// units because it answers to the user's JWT, not to a run capability (§20.1).
type threadCatalogue struct {
	svc *AgentService
}

// repo is the user-scoped agent repository, reached through the owning service the way the harness units
// reach theirs.
func (s *threadCatalogue) repo() *persistence.AgentRepository { return s.svc.repo }

// ThreadView is one thread as the API reports it. The checkpoint is deliberately absent: it is opaque
// bytes a host reads through its own capability endpoint, not something a list response should carry
// (doc/harness.md §6.1).
type ThreadView struct {
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	Status        string     `json:"status"`
	GoalID        *string    `json:"goal_id,omitempty"`
	Custom        string     `json:"custom,omitempty"`
	LastMessageAt *time.Time `json:"last_message_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// ThreadPage is one page of the catalogue.
type ThreadPage struct {
	Items      []ThreadView
	NextCursor string
	HasMore    bool
}

func viewOfThread(thread persistence.AgentThread) ThreadView {
	return ThreadView{
		ID: thread.ID, Title: thread.Title, Status: thread.Status, GoalID: thread.GoalID,
		Custom: thread.Custom, LastMessageAt: thread.LastMessageAt,
		CreatedAt: thread.CreatedAt, UpdatedAt: thread.UpdatedAt,
	}
}

// ListAgentThreads returns a page of threads, newest activity first (§2.2).
func (s *threadCatalogue) ListAgentThreads(ctx context.Context, userID, status, cursor string, limit int) (ThreadPage, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var cursorTime *time.Time
	var cursorID string
	if strings.TrimSpace(cursor) != "" {
		at, id, err := decodeThreadCursor(cursor)
		if err != nil {
			return ThreadPage{}, ErrValidation
		}
		cursorTime, cursorID = &at, id
	}
	threads, err := s.repo().ListThreads(ctx, userID, status, cursorTime, cursorID, limit+1)
	if err != nil {
		return ThreadPage{}, err
	}
	page := ThreadPage{Items: make([]ThreadView, 0, limit)}
	if len(threads) > limit {
		page.HasMore = true
		threads = threads[:limit]
	}
	for _, thread := range threads {
		page.Items = append(page.Items, viewOfThread(thread))
	}
	if page.HasMore && len(threads) > 0 {
		last := threads[len(threads)-1]
		page.NextCursor = encodeThreadCursor(activityOf(last), last.ID)
	}
	return page, nil
}

// activityOf is the sort key a thread is listed by: its last message, or its creation when it has none.
func activityOf(thread persistence.AgentThread) time.Time {
	if thread.LastMessageAt != nil {
		return *thread.LastMessageAt
	}
	return thread.CreatedAt
}

func encodeThreadCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func decodeThreadCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", ErrValidation
	}
	at, id, found := strings.Cut(string(raw), "|")
	if !found {
		return time.Time{}, "", ErrValidation
	}
	parsed, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return time.Time{}, "", ErrValidation
	}
	return parsed, id, nil
}

// CreateAgentThread starts an empty thread (§2.2). assistant-ui calls this through its thread list
// adapter's initialize(), so the id returned here becomes the client's remoteId.
func (s *threadCatalogue) CreateAgentThread(ctx context.Context, userID string, goalID *string, title string) (ThreadView, error) {
	thread, err := s.repo().CreateThread(ctx, userID, goalID, title)
	if err != nil {
		return ThreadView{}, err
	}
	return viewOfThread(*thread), nil
}

// GetAgentThread reads one thread. A cross-user id is not-found, not forbidden (§2.2).
func (s *threadCatalogue) GetAgentThread(ctx context.Context, userID, threadID string) (ThreadView, error) {
	thread, err := s.repo().GetThread(ctx, userID, threadID)
	if err != nil {
		return ThreadView{}, err
	}
	return viewOfThread(*thread), nil
}

// UpdateAgentThread renames a thread or replaces its client-owned display data (§2.2). Both pointers are
// optional: nil means "leave it alone", and an empty string means "clear it" (§1.5 PATCH semantics).
//
// Custom is untrusted by construction — nothing that takes part in an authorization decision may be read
// from it (§2.3) — so it is stored verbatim and never interpreted here.
func (s *threadCatalogue) UpdateAgentThread(ctx context.Context, userID, threadID string, title, custom *string) (ThreadView, error) {
	fields := map[string]any{}
	if title != nil {
		cleaned := strings.TrimSpace(*title)
		if len([]rune(cleaned)) > 120 {
			cleaned = string([]rune(cleaned)[:120])
		}
		fields["title"] = cleaned
	}
	if custom != nil {
		value := strings.TrimSpace(*custom)
		if len(value) > 4096 {
			return ThreadView{}, ErrValidation
		}
		fields["custom"] = value
	}
	if len(fields) == 0 {
		return s.GetAgentThread(ctx, userID, threadID)
	}
	if err := s.repo().UpdateThread(ctx, userID, threadID, fields); err != nil {
		return ThreadView{}, err
	}
	return s.GetAgentThread(ctx, userID, threadID)
}

// SetAgentThreadStatus archives or unarchives a thread (§2.5). Archiving changes one column and keeps
// every row; deleting is the irreversible one, and the two must not look alike in the UI.
func (s *threadCatalogue) SetAgentThreadStatus(ctx context.Context, userID, threadID, status string) (ThreadView, error) {
	if status != persistence.ThreadRegular && status != persistence.ThreadArchived {
		return ThreadView{}, ErrValidation
	}
	if err := s.repo().UpdateThread(ctx, userID, threadID, map[string]any{"status": status}); err != nil {
		return ThreadView{}, err
	}
	return s.GetAgentThread(ctx, userID, threadID)
}

// DeleteAgentThread removes a thread and everything under it (§2.5). An in-flight run is cancelled
// first: leaving one behind would mean a host writing into a thread that no longer exists.
func (s *threadCatalogue) DeleteAgentThread(ctx context.Context, userID, threadID string) error {
	if _, err := s.repo().GetThread(ctx, userID, threadID); err != nil {
		return err
	}
	if run, err := s.repo().GetActiveRunByThread(ctx, userID, threadID); err == nil {
		_ = s.svc.RequestCancellation(ctx, userID, run.ID)
	}
	attachments, err := s.svc.attachments.repo.ThreadAttachments(ctx, userID, threadID)
	if err != nil {
		return err
	}
	if err := s.repo().DeleteThread(ctx, userID, threadID); err != nil {
		return err
	}
	// Files go last: a row that still exists must keep its bytes readable.
	for _, attachment := range attachments {
		s.svc.attachments.DeleteFile(attachment)
	}
	return nil
}

// GenerateThreadTitle derives a title from the conversation itself (§2.4): the first user message,
// newlines folded, truncated. No model call — a title is not worth a round trip or a quota, and a
// deterministic one cannot invent something the user did not say.
func (s *threadCatalogue) GenerateThreadTitle(ctx context.Context, userID, threadID string) (string, error) {
	if _, err := s.repo().GetThread(ctx, userID, threadID); err != nil {
		return "", err
	}
	text, err := s.repo().FirstThreadMessageText(ctx, userID, threadID)
	if err != nil {
		return "", err
	}
	title := deterministicTitle(text)
	if title == "" {
		// A thread with no user text yet (created by initialize() and never used).
		title = "新对话"
	}
	if err := s.repo().UpdateThread(ctx, userID, threadID, map[string]any{"title": title}); err != nil {
		return "", err
	}
	return title, nil
}

// deterministicTitle folds whitespace and truncates on a rune boundary.
func deterministicTitle(text string) string {
	folded := strings.Join(strings.Fields(text), " ")
	runes := []rune(folded)
	if len(runes) > threadTitleRunes {
		return string(runes[:threadTitleRunes])
	}
	return folded
}
