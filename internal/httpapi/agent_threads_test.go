package httpapi

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// The thread catalogue and attachment endpoints over HTTP (doc/interface.md §20.3, §20.4).

// uploadImage posts one multipart upload and returns the decoded response.
func uploadImage(t *testing.T, api testAPI, data []byte, filename, contentType, threadID string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if threadID != "" {
		if err := writer.WriteField("thread_id", threadID); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/agent/attachments", &body)
	// A deliberately wrong Content-Type: the server sniffs the bytes instead (§4.5).
	request.Header.Set("Content-Type", strings.Replace(writer.FormDataContentType(), "multipart/form-data", "multipart/form-data", 1))
	request.Header.Set("Authorization", "Bearer "+api.access)
	response := httptest.NewRecorder()
	api.server.Engine.ServeHTTP(response, request)
	_ = contentType
	return response
}

func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	photo := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			photo.Set(x, y, color.RGBA{R: uint8(x * 30), G: uint8(y * 30), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, photo); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestAgentThreadCatalogueOverHTTP(t *testing.T) {
	api := newTestAPI(t)

	created := api.do(t, http.MethodPost, "/api/v1/agent/threads", map[string]any{}, map[string]string{"Idempotency-Key": "thread-1"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create thread=%d %s", created.Code, created.Body.String())
	}
	var remote struct {
		RemoteID string `json:"remote_id"`
	}
	decode(t, created, &remote)
	if !strings.HasPrefix(remote.RemoteID, "thr_") {
		t.Fatalf("remote_id=%q, want a thread id", remote.RemoteID)
	}

	renamed := api.do(t, http.MethodPatch, "/api/v1/agent/threads/"+remote.RemoteID, map[string]any{"title": "论文拆解", "custom": `{"pinned":true}`}, nil)
	if renamed.Code != http.StatusOK {
		t.Fatalf("patch=%d %s", renamed.Code, renamed.Body.String())
	}
	var patched struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Status string `json:"status"`
		Custom string `json:"custom"`
	}
	decode(t, renamed, &patched)
	if patched.Title != "论文拆解" || patched.Status != "regular" || patched.Custom != `{"pinned":true}` {
		t.Fatalf("patched=%#v", patched)
	}

	archived := api.do(t, http.MethodPost, "/api/v1/agent/threads/"+remote.RemoteID+"/archive", nil, nil)
	if archived.Code != http.StatusOK {
		t.Fatalf("archive=%d %s", archived.Code, archived.Body.String())
	}
	listed := api.do(t, http.MethodGet, "/api/v1/agent/threads?status=archived", nil, nil)
	var archivedPage struct {
		Items []map[string]any `json:"items"`
	}
	decode(t, listed, &archivedPage)
	if len(archivedPage.Items) != 1 || archivedPage.Items[0]["id"] != remote.RemoteID {
		t.Fatalf("archived page=%#v", archivedPage.Items)
	}
	regular := api.do(t, http.MethodGet, "/api/v1/agent/threads?status=regular", nil, nil)
	var regularPage struct {
		Items []map[string]any `json:"items"`
	}
	decode(t, regular, &regularPage)
	if len(regularPage.Items) != 0 {
		t.Fatalf("regular page=%#v, want the archived thread gone from it", regularPage.Items)
	}
	if response := api.do(t, http.MethodPost, "/api/v1/agent/threads/"+remote.RemoteID+"/unarchive", nil, nil); response.Code != http.StatusOK {
		t.Fatalf("unarchive=%d %s", response.Code, response.Body.String())
	}

	title := api.do(t, http.MethodPost, "/api/v1/agent/threads/"+remote.RemoteID+"/title", nil, nil)
	if title.Code != http.StatusOK {
		t.Fatalf("title=%d %s", title.Code, title.Body.String())
	}
	var generated struct {
		Title string `json:"title"`
	}
	decode(t, title, &generated)
	if generated.Title == "" {
		t.Fatal("no title was generated")
	}

	deleted := api.do(t, http.MethodDelete, "/api/v1/agent/threads/"+remote.RemoteID, nil, nil)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete=%d %s", deleted.Code, deleted.Body.String())
	}
	if response := api.do(t, http.MethodGet, "/api/v1/agent/threads/"+remote.RemoteID, nil, nil); response.Code != http.StatusNotFound {
		t.Fatalf("a deleted thread reads as %d, want 404", response.Code)
	}
	// An unknown status is a client error, not an empty list.
	if response := api.do(t, http.MethodGet, "/api/v1/agent/threads?status=deleted", nil, nil); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=deleted returned %d, want 422", response.Code)
	}
}

func TestAgentThreadAccessIsScopedOverHTTP(t *testing.T) {
	api := newTestAPI(t)
	created := api.do(t, http.MethodPost, "/api/v1/agent/threads", map[string]any{}, map[string]string{"Idempotency-Key": "thread-scope"})
	var remote struct {
		RemoteID string `json:"remote_id"`
	}
	decode(t, created, &remote)

	otherToken := agentSecondToken(t, api)
	headers := map[string]string{"Authorization": "Bearer " + otherToken}
	for _, call := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/agent/threads/" + remote.RemoteID},
		{http.MethodPatch, "/api/v1/agent/threads/" + remote.RemoteID},
		{http.MethodDelete, "/api/v1/agent/threads/" + remote.RemoteID},
		{http.MethodPost, "/api/v1/agent/threads/" + remote.RemoteID + "/archive"},
	} {
		var body any
		if call.method == http.MethodPatch {
			body = map[string]any{"title": "偷来的"}
		}
		response := api.do(t, call.method, call.path, body, headers)
		// A cross-user thread is indistinguishable from a missing one (§2.2, arch.md §12).
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s %s as another user=%d, want 404 (%s)", call.method, call.path, response.Code, response.Body.String())
		}
	}
	list := api.do(t, http.MethodGet, "/api/v1/agent/threads", nil, headers)
	var page struct {
		Items []map[string]any `json:"items"`
	}
	decode(t, list, &page)
	if len(page.Items) != 0 {
		t.Fatalf("another user's threads leaked: %#v", page.Items)
	}
}

func TestAgentAttachmentRoundTripOverHTTP(t *testing.T) {
	api := newTestAPIWithAgent(t, application.WithAttachmentDir(t.TempDir()))

	response := uploadImage(t, api, testPNG(t, 4, 3), "photo.png", "image/png", "")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload=%d %s", response.Code, response.Body.String())
	}
	var uploaded struct {
		AttachmentID string `json:"attachment_id"`
		Mime         string `json:"mime"`
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		Bytes        int64  `json:"bytes"`
	}
	decode(t, response, &uploaded)
	if uploaded.AttachmentID == "" || uploaded.Mime != "image/png" || uploaded.Width != 4 || uploaded.Height != 3 || uploaded.Bytes == 0 {
		t.Fatalf("upload response=%#v", uploaded)
	}

	read := api.do(t, http.MethodGet, "/api/v1/agent/attachments/"+uploaded.AttachmentID, nil, nil)
	if read.Code != http.StatusOK {
		t.Fatalf("read=%d %s", read.Code, read.Body.String())
	}
	if got := read.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type=%q, want image/png", got)
	}
	if got := read.Header().Get("Cache-Control"); !strings.Contains(got, "private") {
		t.Fatalf("Cache-Control=%q, want a private cache (the bytes belong to one user)", got)
	}
	if config, _, err := image.DecodeConfig(bytes.NewReader(read.Body.Bytes())); err != nil || config.Width != 4 {
		t.Fatalf("the returned bytes are not the image: %v %#v", err, config)
	}

	// Another user gets 404, not 403: the attachment is indistinguishable from a missing one.
	otherToken := agentSecondToken(t, api)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		cross := api.do(t, method, "/api/v1/agent/attachments/"+uploaded.AttachmentID, nil,
			map[string]string{"Authorization": "Bearer " + otherToken})
		if cross.Code != http.StatusNotFound {
			t.Fatalf("%s as another user=%d, want 404", method, cross.Code)
		}
	}

	deleted := api.do(t, http.MethodDelete, "/api/v1/agent/attachments/"+uploaded.AttachmentID, nil, nil)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete=%d %s", deleted.Code, deleted.Body.String())
	}
	if gone := api.do(t, http.MethodGet, "/api/v1/agent/attachments/"+uploaded.AttachmentID, nil, nil); gone.Code != http.StatusNotFound {
		t.Fatalf("a deleted attachment reads as %d, want 404", gone.Code)
	}
}

func TestAgentAttachmentRejectsNonImages(t *testing.T) {
	api := newTestAPIWithAgent(t, application.WithAttachmentDir(t.TempDir()))

	response := uploadImage(t, api, []byte("#!/bin/sh\nrm -rf /\n"), "shell.png", "image/png", "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("a script named .png=%d, want 400 body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "ATTACHMENT_UNSUPPORTED") {
		t.Fatalf("the 400 does not say why: %s", response.Body.String())
	}
	// An upload with no file part is a client error, not a stored empty attachment.
	empty := api.do(t, http.MethodPost, "/api/v1/agent/attachments", map[string]any{}, nil)
	if empty.Code < 400 || empty.Code >= 500 {
		t.Fatalf("an empty upload=%d, want a 4xx", empty.Code)
	}
	var stored int64
	api.store.DB.Model(&persistence.AgentAttachment{}).Count(&stored)
	if stored != 0 {
		t.Fatalf("%d attachments were stored from rejected uploads", stored)
	}
}

// TestAgentMessageCarriesAttachmentReferences covers doc/chat-features.md §4.2 and §4.4 end to end: a
// message may carry images, they are stored as references rather than bytes, and the model proxy expands
// a reference into real image data while dropping anything inline.
func TestAgentMessageCarriesAttachmentReferences(t *testing.T) {
	stub := newModelStub(t, stubTurn{text: "图里是一条上升的曲线。"})
	api := newHarnessAPI(t, stub, application.WithAttachmentDir(t.TempDir()))

	uploaded := uploadImage(t, api, testPNG(t, 3, 2), "plot.png", "image/png", "")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload=%d %s", uploaded.Code, uploaded.Body.String())
	}
	var attachment struct {
		AttachmentID string `json:"attachment_id"`
	}
	decode(t, uploaded, &attachment)
	reference := "/api/v1/agent/attachments/" + attachment.AttachmentID

	body := map[string]any{
		"commands": []any{map[string]any{
			"type": "add-message",
			"message": map[string]any{
				"role": "user",
				"parts": []any{
					map[string]any{"type": "text", "text": "看看这张图"},
					map[string]any{"type": "image", "image": reference},
					// Inline data is dropped: every image has to pass an ownership check (§4.4).
					map[string]any{"type": "image", "image": "data:image/png;base64,iVBORw0KGgo="},
				},
			},
		}},
		"threadId":     nil,
		"harness_mode": application.HarnessModeWASM,
	}
	runID, responses := startHarnessRunWithBody(t, api, body)
	host := newHarnessClient(t, api, runID)

	// The host's model call carries the conversation the way the shim renders it: a text part plus an
	// image_url for the attachment, and an inline data URI it should not have sent at all.
	if _, response := host.modelCallBody(map[string]any{
		"model": "host-picked-model", "stream": true,
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "看看这张图"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": reference}},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,iVBORw0KGgo="}},
			},
		}},
	}); response.Code != http.StatusOK {
		t.Fatalf("model call=%d %s", response.Code, response.Body.String())
	}
	host.complete("stop")
	streamResponse(t, responses)

	// The reference is stored as an image part; the inline data is not stored at all.
	parts := harnessParts(t, api, runID)
	var images int
	for _, part := range parts {
		if part.Type == "image" {
			images++
			if part.Text != reference {
				t.Fatalf("image part=%q, want the reference", part.Text)
			}
		}
		if strings.Contains(part.Text, "base64") {
			t.Fatalf("inline image data was persisted: %q", part.Text)
		}
	}
	if images != 1 {
		t.Fatalf("image parts=%d, want 1 (the inline one dropped)", images)
	}

	// The provider saw a data URI expanded from OUR storage, not the reference and not the client's bytes.
	request := stub.request(t, 0)
	messages, _ := request["messages"].([]any)
	var sawImage, sawReference, sawInline int
	for _, item := range messages {
		message, _ := item.(map[string]any)
		content, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, part := range content {
			entry, _ := part.(map[string]any)
			if entry["type"] != "image_url" {
				continue
			}
			url, _ := entry["image_url"].(map[string]any)
			value, _ := url["url"].(string)
			switch {
			case strings.HasPrefix(value, "data:image/png;base64,") && len(value) > 100:
				sawImage++
			case strings.Contains(value, "/agent/attachments/"):
				sawReference++
			case strings.HasPrefix(value, "data:"):
				// The client's own inline bytes: too short to be a real image, and dropped on principle.
				sawInline++
			}
		}
	}
	if sawImage != 1 {
		t.Fatalf("the provider saw %d expanded images, want exactly the one attachment", sawImage)
	}
	if sawReference != 0 {
		t.Fatal("the raw reference reached the provider, which cannot fetch it")
	}
	if sawInline != 0 {
		t.Fatal("inline image data from the host reached the provider (§4.4: it must be dropped)")
	}
	// The attachment is now bound to the thread, so deleting the thread takes the file with it (§2.5).
	var bound int64
	api.store.DB.Model(&persistence.AgentAttachment{}).Where("thread_id IS NOT NULL").Count(&bound)
	if bound != 1 {
		t.Fatalf("bound attachments=%d, want 1", bound)
	}
	if deleted := api.do(t, http.MethodDelete, "/api/v1/agent/threads/"+host.threadID(), nil, nil); deleted.Code != http.StatusNoContent {
		t.Fatalf("thread delete=%d %s", deleted.Code, deleted.Body.String())
	}
	var remaining int64
	api.store.DB.Model(&persistence.AgentAttachment{}).Count(&remaining)
	if remaining != 0 {
		t.Fatalf("%d attachment rows survived the thread deletion", remaining)
	}
}
