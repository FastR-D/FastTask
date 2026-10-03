package httpapi

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestVoiceUploadRefusesOversizedAudioInsteadOfTruncating(t *testing.T) {
	api := newTestAPI(t)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("audio", "long.webm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(make([]byte, voiceMaxBytes+1)); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/voice-transcription-jobs", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", "Bearer "+api.access)
	response := httptest.NewRecorder()
	api.server.Engine.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d %s", response.Code, response.Body.String())
	}
	if entries, _ := os.ReadDir(api.server.cfg.AudioDir); len(entries) != 0 {
		t.Fatalf("a refused upload left %d files behind", len(entries))
	}
}

func TestAudioExtensionKeepsOnlyAShortAlphanumericSuffix(t *testing.T) {
	for name, want := range map[string]string{
		"voice.webm":          ".webm",
		"Recording.M4A":       ".m4a",
		"noext":               "",
		"../../etc/passwd":    "",
		"x.ph p":              "",
		"a.verylongextension": "",
		`evil.\..\..\x`:       "",
	} {
		if got := audioExtension(name); got != want {
			t.Errorf("audioExtension(%q)=%q, want %q", name, got, want)
		}
	}
}
