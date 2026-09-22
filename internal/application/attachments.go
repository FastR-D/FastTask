package application

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/persistence"
)

// Attachment storage (doc/chat-features.md §4.3, §4.5).
//
// Attachments live on the server's own disk: one binary plus a data directory is the deployment shape
// (arch.md §14), and an object store would add a dependency to hold bytes that never leave the process
// except as a data URI on the way to the model.
//
// Every limit in §4.5 is enforced here, not in the client, because a client-side limit is a suggestion.

const (
	// AttachmentMaxBytes is the per-image cap (§4.5).
	AttachmentMaxBytes = 8 << 20
	// AttachmentMaxPerMessage is how many images one message may carry.
	AttachmentMaxPerMessage = 4
	// AttachmentMaxPerThread and AttachmentMaxThreadBytes are the per-thread quota.
	AttachmentMaxPerThread   = 100
	AttachmentMaxThreadBytes = 500 << 20
	// AttachmentOrphanTTL is how long an attachment that was never sent is kept (§4.5).
	AttachmentOrphanTTL = 24 * time.Hour
)

// Attachment errors, mapped to 400 by the HTTP layer with a message the user can act on (§4.5).
var (
	ErrAttachmentTooLarge = errors.New("the image is larger than the 8 MB limit")
	ErrAttachmentQuota    = errors.New("this conversation has reached its attachment quota")
	ErrAttachmentEmpty    = errors.New("the upload carried no bytes")
)

// AttachmentView is the API shape (§4.3). The path is never exposed: a client that could guess a path
// could try to read somebody else's file, and the id it does get only works through an ownership check.
type AttachmentView struct {
	AttachmentID string    `json:"attachment_id"`
	Mime         string    `json:"mime"`
	Width        int       `json:"width"`
	Height       int       `json:"height"`
	Bytes        int64     `json:"bytes"`
	ThreadID     *string   `json:"thread_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// AttachmentStore owns the bytes and the rows that describe them.
type AttachmentStore struct {
	dir  string
	repo *persistence.AgentRepository
}

// NewAttachmentStore builds a store rooted at dir. An empty dir disables uploads, which is what a
// read-only or test deployment wants.
func NewAttachmentStore(dir string, repo *persistence.AgentRepository) *AttachmentStore {
	return &AttachmentStore{dir: dir, repo: repo}
}

func viewOfAttachment(attachment persistence.AgentAttachment) AttachmentView {
	return AttachmentView{
		AttachmentID: attachment.ID, Mime: attachment.Mime, Width: attachment.Width,
		Height: attachment.Height, Bytes: attachment.Bytes, ThreadID: attachment.ThreadID,
		CreatedAt: attachment.CreatedAt,
	}
}

// Upload sniffs, strips, measures and stores one image (§4.5). The declared content type and the file name
// are read for nothing: the type comes from the magic number, and the stored name is generated.
func (s *AttachmentStore) Upload(ctx context.Context, userID string, threadID *string, data []byte) (AttachmentView, error) {
	if s == nil || s.dir == "" {
		return AttachmentView{}, errors.New("attachment storage is not configured")
	}
	if len(data) == 0 {
		return AttachmentView{}, ErrAttachmentEmpty
	}
	if len(data) > AttachmentMaxBytes {
		return AttachmentView{}, ErrAttachmentTooLarge
	}
	mime, ok := sniffImageMime(data)
	if !ok || allowedImageMimes[mime] == "" {
		return AttachmentView{}, ErrUnsupportedImage
	}
	cleaned, err := stripImageMetadata(mime, data)
	if err != nil {
		return AttachmentView{}, fmt.Errorf("%w: the image carries metadata that could not be stripped", ErrUnsupportedImage)
	}
	width, height := imageDimensions(mime, cleaned)

	if threadID != nil && strings.TrimSpace(*threadID) != "" {
		count, bytes, err := s.repo.AttachmentTotals(ctx, userID, *threadID)
		if err != nil {
			return AttachmentView{}, err
		}
		if count+1 > AttachmentMaxPerThread || bytes+int64(len(cleaned)) > AttachmentMaxThreadBytes {
			return AttachmentView{}, ErrAttachmentQuota
		}
	}

	attachment := &persistence.AgentAttachment{
		ID: persistence.NewID("att"), UserID: userID, ThreadID: nil,
		Mime: mime, Bytes: int64(len(cleaned)), Width: width, Height: height,
	}
	if threadID != nil && strings.TrimSpace(*threadID) != "" {
		scope := strings.TrimSpace(*threadID)
		attachment.ThreadID = &scope
	}
	attachment.Path = s.pathFor(attachment.ID, mime)
	if err := os.MkdirAll(filepath.Dir(attachment.Path), 0o750); err != nil {
		return AttachmentView{}, err
	}
	if err := os.WriteFile(attachment.Path, cleaned, 0o600); err != nil {
		return AttachmentView{}, err
	}
	if err := s.repo.CreateAttachment(ctx, attachment); err != nil {
		// A row that failed to be written leaves bytes nobody can reach; remove them.
		_ = os.Remove(attachment.Path)
		return AttachmentView{}, err
	}
	return viewOfAttachment(*attachment), nil
}

// pathFor is where an attachment's bytes live. Both path segments are generated, so no user input reaches
// the filesystem layout — and the join is still cleaned, because a bug in an id generator should not become
// a traversal.
func (s *AttachmentStore) pathFor(id, mime string) string {
	return filepath.Join(s.dir, filepath.Base(id)+imageExtension(mime))
}

// Get resolves an attachment and returns its view with the on-disk path. A cross-user id is not-found.
func (s *AttachmentStore) Get(ctx context.Context, userID, attachmentID string) (AttachmentView, string, error) {
	attachment, err := s.repo.GetAttachment(ctx, userID, attachmentID)
	if err != nil {
		return AttachmentView{}, "", err
	}
	if _, err := os.Stat(attachment.Path); err != nil {
		return AttachmentView{}, "", err
	}
	return viewOfAttachment(*attachment), attachment.Path, nil
}

// Delete removes the row and then the file, in that order: a crash between the two loses bytes that were
// already unreachable, which is the safe direction.
func (s *AttachmentStore) Delete(ctx context.Context, userID, attachmentID string) error {
	attachment, err := s.repo.DeleteAttachment(ctx, userID, attachmentID)
	if err != nil {
		return err
	}
	s.DeleteFile(*attachment)
	return nil
}

// DeleteFile removes one attachment's bytes. It is called after the rows that referenced them are gone.
func (s *AttachmentStore) DeleteFile(attachment persistence.AgentAttachment) {
	if s == nil || attachment.Path == "" {
		return
	}
	_ = os.Remove(attachment.Path)
}

// Bind attaches an uploaded file to the thread and message that carried it (§4.5), so a thread deletion
// takes its attachments with it and the orphan sweep does not.
func (s *AttachmentStore) Bind(ctx context.Context, userID, attachmentID, threadID, messageID string) error {
	return s.repo.AttachAttachment(ctx, userID, attachmentID, threadID, messageID)
}

// DataURI renders an attachment as an inline data URI for the model proxy (§4.4). Expansion happens here
// and nowhere else: this is the one place that has both the bytes and the proof of ownership.
func (s *AttachmentStore) DataURI(ctx context.Context, userID, attachmentID string) (string, bool) {
	attachment, err := s.repo.GetAttachment(ctx, userID, attachmentID)
	if err != nil {
		return "", false
	}
	if attachment.Bytes > AttachmentMaxBytes {
		return "", false
	}
	data, err := os.ReadFile(attachment.Path)
	if err != nil || int64(len(data)) != attachment.Bytes {
		return "", false
	}
	return "data:" + attachment.Mime + ";base64," + base64.StdEncoding.EncodeToString(data), true
}

// AttachmentIDFromRef pulls an attachment id out of a reference the host sent (§4.4). The reference is a
// URL on our own API, because that is what the attachment adapter puts in the message; anything else is
// not ours to expand.
func AttachmentIDFromRef(reference string) (string, bool) {
	const marker = "/agent/attachments/"
	index := strings.LastIndex(reference, marker)
	if index < 0 {
		return "", false
	}
	id := strings.TrimSuffix(reference[index+len(marker):], "/")
	if id = strings.SplitN(id, "?", 2)[0]; id == "" {
		return "", false
	}
	if !strings.HasPrefix(id, "att_") || strings.ContainsAny(id, "/.\\") {
		return "", false
	}
	return id, true
}

// ReapOrphanAttachments deletes uploads that were never sent (§4.5). It runs on the scheduler's existing
// sweep, so no goroutine is added for it.
func (s *AttachmentStore) ReapOrphans(ctx context.Context, now time.Time) (int, error) {
	if s == nil {
		return 0, nil
	}
	orphans, err := s.repo.OrphanAttachments(ctx, now.Add(-AttachmentOrphanTTL))
	if err != nil {
		return 0, err
	}
	if len(orphans) == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(orphans))
	for _, orphan := range orphans {
		ids = append(ids, orphan.ID)
	}
	removed, err := s.repo.DeleteAttachments(ctx, ids)
	if err != nil {
		return 0, err
	}
	for _, orphan := range orphans {
		s.DeleteFile(orphan)
	}
	return int(removed), nil
}
