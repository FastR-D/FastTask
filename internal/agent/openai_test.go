package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/config"
)

func TestOpenAIProviderParsesStructuredProposal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("missing authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": `[{"type":"task","title":"跑通基线","success_criteria":"保存可复现日志","minimum_action":"打开配置并确认数据路径","priority":90,"estimate_minutes":50}]`}}}})
	}))
	defer server.Close()
	provider := NewOpenAI(config.Config{OpenAIBaseURL: server.URL, OpenAIModel: "test-model", OpenAIAPIKey: "test-key"})
	proposal, err := provider.TaskProposal(context.Background(), "完成论文", "通过评审", "先完成实验")
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal) != 1 || proposal[0]["title"] != "跑通基线" {
		t.Fatalf("unexpected proposal: %#v", proposal)
	}
}

func TestOpenAIProviderRejectsInvalidProposal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": `[{"type":"task","title":"missing fields"}]`}}}})
	}))
	defer server.Close()
	provider := NewOpenAI(config.Config{OpenAIBaseURL: server.URL, OpenAIModel: "test-model", OpenAIAPIKey: "test-key"})
	if _, err := provider.TaskProposal(context.Background(), "goal", "criteria", "instruction"); err == nil {
		t.Fatal("invalid proposal accepted")
	}
}

func TestOpenAITranscriberSendsActualAudio(t *testing.T) {
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		received, err = io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		if r.FormValue("model") != "test-stt" {
			t.Fatalf("model=%s", r.FormValue("model"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"text": "真实音频转写"})
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "voice.webm")
	want := []byte("actual audio payload")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	provider := NewOpenAI(config.Config{OpenAIBaseURL: server.URL, OpenAIModel: "chat", OpenAIAPIKey: "key", TranscriptionModel: "test-stt"})
	text, err := provider.Transcribe(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if text != "真实音频转写" || !bytes.Equal(received, want) {
		t.Fatalf("text=%q audio=%q", text, received)
	}
}

func TestRealConfiguredModel(t *testing.T) {
	if os.Getenv("FASTTASK_REAL_LLM_TEST") != "1" {
		t.Skip("set FASTTASK_REAL_LLM_TEST=1 to call the configured model")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasLLM() {
		t.Skip("OPENAI-compatible model is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	proposal, err := NewOpenAI(cfg).TaskProposal(ctx, "完成一个可复现的基线实验", "保存配置、日志和结果摘要", "拆成少量今天能开始的任务")
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal) < 2 {
		t.Fatalf("real model returned only %d nodes", len(proposal))
	}
	for index, node := range proposal {
		for _, field := range []string{"type", "title", "success_criteria", "minimum_action", "priority", "estimate_minutes"} {
			if _, ok := node[field]; !ok {
				t.Fatalf("node %d missing %s: %#v", index, field, node)
			}
		}
	}
}
