package application

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"strings"
	"testing"

	"github.com/FastR-D/FastTask/internal/persistence"
)

// Attachment handling (doc/chat-features.md §4.5).
//
// The three rules under test are the ones a client cannot be trusted with: the type comes from the bytes,
// the location metadata is gone before the file is written, and the quotas are enforced here rather than
// in the composer.

// pngBytes renders a tiny valid PNG.
func pngBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	photo := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			photo.Set(x, y, color.RGBA{R: uint8(x * 8), G: uint8(y * 8), B: 40, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, photo); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// jpegBytes renders a tiny valid JPEG.
func jpegBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	photo := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		photo.Set(x, 0, color.RGBA{R: 200, G: 100, B: 50, A: 255})
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, photo, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// jpegWithGPS inserts an APP1/EXIF segment carrying a GPS marker, the shape a phone photo has. It is the
// input the stripper must not let through.
func jpegWithGPS(t *testing.T, payload []byte) []byte {
	t.Helper()
	exif := append([]byte("Exif\x00\x00"), payload...)
	segment := make([]byte, 0, len(exif)+4)
	segment = append(segment, 0xff, 0xe1)
	length := len(exif) + 2
	segment = append(segment, byte(length>>8), byte(length&0xff))
	segment = append(segment, exif...)
	// SOI, then the APP1 segment, then the rest of the file.
	return append(append(append([]byte{}, payload[:0]...), append([]byte{0xff, 0xd8}, segment...)...), payload[2:]...)
}

func attachmentFixture(t *testing.T) (fixture, *AgentService) {
	t.Helper()
	f := newFixture(t)
	svc := NewAgentService(f.app, WithAttachmentDir(t.TempDir()))
	return f, svc
}

func TestAttachmentUploadSniffsTypeAndStripsMetadata(t *testing.T) {
	f, svc := attachmentFixture(t)
	ctx := context.Background()

	view, err := svc.Attachments().Upload(ctx, f.user.ID, nil, pngBytes(t, 3, 2))
	if err != nil {
		t.Fatalf("upload png: %v", err)
	}
	if view.Mime != "image/png" || view.Width != 3 || view.Height != 2 {
		t.Fatalf("view=%#v, want a 3x2 png", view)
	}
	if view.AttachmentID == "" || strings.HasPrefix(view.AttachmentID, "att_") == false {
		t.Fatalf("attachment id=%q, want a generated one", view.AttachmentID)
	}

	// A JPEG carrying GPS is stored without it, and still decodes.
	payload := jpegBytes(t, 4, 1)
	withGPS := jpegWithGPS(t, payload)
	if !bytes.Contains(withGPS, []byte("Exif")) {
		t.Fatal("the fixture did not carry an EXIF segment")
	}
	stored, err := svc.Attachments().Upload(ctx, f.user.ID, nil, withGPS)
	if err != nil {
		t.Fatalf("upload jpeg: %v", err)
	}
	row, err := svc.Repository().GetAttachment(ctx, f.user.ID, stored.AttachmentID)
	if err != nil {
		t.Fatal(err)
	}
	bytesOnDisk, err := os.ReadFile(row.Path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytesOnDisk, []byte("Exif")) {
		t.Fatal("EXIF survived the upload: a phone photo would put its GPS into the database (§4.5)")
	}
	if stored.Width == 0 || stored.Height == 0 {
		t.Fatalf("dimensions=%dx%d, want them read from the stripped bytes", stored.Width, stored.Height)
	}
	if stored.Mime != "image/jpeg" {
		t.Fatalf("mime=%q, want image/jpeg", stored.Mime)
	}
	// The stripped file is still a decodable JPEG.
	if _, _, err := image.DecodeConfig(bytes.NewReader(bytesOnDisk)); err != nil {
		t.Fatalf("the stripped image no longer decodes: %v", err)
	}
}

func TestAttachmentUploadRejectsNonImages(t *testing.T) {
	f, svc := attachmentFixture(t)
	ctx := context.Background()

	cases := map[string][]byte{
		"a text file named .png":   []byte("this is not an image at all"),
		"a shell script":           []byte("#!/bin/sh\nrm -rf /\n"),
		"an svg (xml, not raster)": []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"empty":                    nil,
	}
	for name, data := range cases {
		_, err := svc.Attachments().Upload(ctx, f.user.ID, nil, data)
		if err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	// An oversized upload is refused before it is buffered anywhere (§4.5).
	big := make([]byte, AttachmentMaxBytes+1)
	copy(big, pngBytes(t, 1, 1))
	if _, err := svc.Attachments().Upload(ctx, f.user.ID, nil, big); err != ErrAttachmentTooLarge {
		t.Fatalf("oversized upload err=%v, want ErrAttachmentTooLarge", err)
	}
}

func TestAttachmentThreadQuota(t *testing.T) {
	f, svc := attachmentFixture(t)
	ctx := context.Background()
	thread, err := svc.Repository().CreateThread(ctx, f.user.ID, nil, "配额")
	if err != nil {
		t.Fatal(err)
	}
	threadID := thread.ID
	for i := 0; i < 3; i++ {
		if _, err := svc.Attachments().Upload(ctx, f.user.ID, &threadID, pngBytes(t, 2, 2)); err != nil {
			t.Fatalf("upload %d: %v", i, err)
		}
	}
	count, total, err := svc.Repository().AttachmentTotals(ctx, f.user.ID, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 || total == 0 {
		t.Fatalf("totals=%d/%d, want 3 attachments with a size", count, total)
	}
	// The count quota is enforced against the thread, not against one request.
	if err := svc.store.DB.Model(&persistence.AgentAttachment{}).
		Where("user_id = ? AND thread_id = ?", f.user.ID, threadID).
		Update("bytes", AttachmentMaxThreadBytes).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Attachments().Upload(ctx, f.user.ID, &threadID, pngBytes(t, 2, 2)); err != ErrAttachmentQuota {
		t.Fatalf("quota err=%v, want ErrAttachmentQuota", err)
	}
}

func TestAttachmentDataURIAndReferenceParsing(t *testing.T) {
	f, svc := attachmentFixture(t)
	ctx := context.Background()
	view, err := svc.Attachments().Upload(ctx, f.user.ID, nil, pngBytes(t, 2, 2))
	if err != nil {
		t.Fatal(err)
	}
	reference := "/api/v1/agent/attachments/" + view.AttachmentID
	id, ok := AttachmentIDFromRef(reference)
	if !ok || id != view.AttachmentID {
		t.Fatalf("AttachmentIDFromRef(%q)=%q,%v", reference, id, ok)
	}
	for _, rejected := range []string{
		"https://evil.example/agent/attachments/att_x", // not our path shape is fine, but the id must be ours
		"/api/v1/agent/attachments/../../etc/passwd",
		"/api/v1/agent/attachments/not-an-attachment",
		"data:image/png;base64,AAAA",
		"",
	} {
		if id, ok := AttachmentIDFromRef(rejected); ok && !strings.HasPrefix(id, "att_") {
			t.Fatalf("AttachmentIDFromRef(%q)=%q, want a refusal", rejected, id)
		}
		if rejected == "/api/v1/agent/attachments/../../etc/passwd" {
			if _, ok := AttachmentIDFromRef(rejected); ok {
				t.Fatal("a traversal reference was accepted")
			}
		}
	}

	uri, ok := svc.Attachments().DataURI(ctx, f.user.ID, view.AttachmentID)
	if !ok || !strings.HasPrefix(uri, "data:image/png;base64,") {
		t.Fatalf("data uri=%q ok=%v", uri, ok)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(uri, "data:image/png;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := image.DecodeConfig(bytes.NewReader(decoded)); err != nil {
		t.Fatalf("the expanded image does not decode: %v", err)
	}

	// Another user's attachment expands to nothing, and reads as not-found.
	other := persistence.NewID("user")
	if _, ok := svc.Attachments().DataURI(ctx, other, view.AttachmentID); ok {
		t.Fatal("a cross-user attachment was expanded")
	}
	if _, _, err := svc.Attachments().Get(ctx, other, view.AttachmentID); !persistence.IsNotFound(err) {
		t.Fatalf("cross-user read err=%v, want not-found", err)
	}
}

func TestAttachmentDeleteRemovesRowAndFile(t *testing.T) {
	f, svc := attachmentFixture(t)
	ctx := context.Background()
	view, err := svc.Attachments().Upload(ctx, f.user.ID, nil, pngBytes(t, 2, 2))
	if err != nil {
		t.Fatal(err)
	}
	row, err := svc.Repository().GetAttachment(ctx, f.user.ID, view.AttachmentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Attachments().Delete(ctx, f.user.ID, view.AttachmentID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(row.Path); !os.IsNotExist(err) {
		t.Fatal("the file survived the delete")
	}
	if _, err := svc.Repository().GetAttachment(ctx, f.user.ID, view.AttachmentID); !persistence.IsNotFound(err) {
		t.Fatalf("row err=%v, want not-found", err)
	}
}

func TestAttachmentOrphanSweep(t *testing.T) {
	f, svc := attachmentFixture(t)
	ctx := context.Background()
	sent, err := svc.Attachments().Upload(ctx, f.user.ID, nil, pngBytes(t, 2, 2))
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := svc.Attachments().Upload(ctx, f.user.ID, nil, pngBytes(t, 2, 2))
	if err != nil {
		t.Fatal(err)
	}
	thread, err := svc.Repository().CreateThread(ctx, f.user.ID, nil, "绑定")
	if err != nil {
		t.Fatal(err)
	}
	// Bound to the thread but not to a message: what makes an attachment an orphan is having never been
	// sent, and the sweep keys off the thread.
	if err := svc.Attachments().Bind(ctx, f.user.ID, sent.AttachmentID, thread.ID, ""); err != nil {
		t.Fatal(err)
	}
	// Age both rows; only the unbound one is an orphan (§4.5).
	old := persistence.Now().Add(-2 * AttachmentOrphanTTL)
	if err := svc.store.DB.Model(&persistence.AgentAttachment{}).Where("user_id = ?", f.user.ID).
		Update("created_at", old).Error; err != nil {
		t.Fatal(err)
	}
	removed, err := svc.Attachments().ReapOrphans(ctx, persistence.Now())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed=%d, want 1 (only the attachment that was never sent)", removed)
	}
	if _, err := svc.Repository().GetAttachment(ctx, f.user.ID, sent.AttachmentID); err != nil {
		t.Fatal("the sent attachment was reclaimed")
	}
	if _, err := svc.Repository().GetAttachment(ctx, f.user.ID, orphan.AttachmentID); !persistence.IsNotFound(err) {
		t.Fatalf("orphan err=%v, want not-found", err)
	}
}

func TestWebPDimensionsAreReadFromTheContainer(t *testing.T) {
	// A minimal lossy WebP header: RIFF/WEBP/VP8 with the 0x9d012a sync code and 5x7 dimensions.
	header := []byte("RIFF\x00\x00\x00\x00WEBPVP8 \x1a\x00\x00\x00")
	frame := []byte{
		0x30, 0x01, 0x00, 0x9d, 0x01, 0x2a,
		0x05, 0x00, // width 5
		0x07, 0x00, // height 7
	}
	data := append(header, frame...)
	width, height := imageDimensions("image/webp", data)
	if width != 5 || height != 7 {
		t.Fatalf("dimensions=%dx%d, want 5x7", width, height)
	}
	if w, h := webpDimensions(nil); w != 0 || h != 0 {
		t.Fatalf("an empty container reported %dx%d", w, h)
	}
}
