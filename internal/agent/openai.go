package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/config"
)

type Provider interface {
	Name() string
	TaskProposal(context.Context, string, string, string) ([]map[string]any, error)
	ConversationReply(context.Context, string) (string, error)
}

type Transcriber interface {
	Name() string
	Transcribe(context.Context, string) (string, error)
}

type OpenAI struct {
	baseURL            string
	model              string
	apiKey             string
	transcriptionModel string
	client             *http.Client
}

func NewOpenAI(cfg config.Config) *OpenAI {
	return &OpenAI{baseURL: cfg.OpenAIBaseURL, model: cfg.OpenAIModel, apiKey: cfg.OpenAIAPIKey, transcriptionModel: cfg.TranscriptionModel, client: &http.Client{Timeout: 45 * time.Second}}
}

func (o *OpenAI) Name() string { return "openai-compatible/" + o.model }

func (o *OpenAI) TaskProposal(ctx context.Context, title, criteria, instruction string) ([]map[string]any, error) {
	prompt := fmt.Sprintf(`你是 FastTask 的科研任务规划器。请为目标生成 2 到 6 个可执行节点。
目标：%s
验收标准：%s
用户指令：%s
只返回 JSON 数组，不要 Markdown。
创建节点使用 op=create，并包含 client_ref、可选 parent_ref、type（milestone/task/action）、title、success_criteria、minimum_action、priority（0-100）、estimate_minutes（5-1440）。parent_ref 必须引用数组中更早的 client_ref。
修订现有节点可以使用 op=update、op=move 或 op=supersede，并提供 target_id；move 提供 parent_id 或 null。最小行动必须是 5-15 分钟内可以开始且与目标直接相关的动作。`, title, criteria, instruction)
	content, err := o.complete(ctx, "你只输出合法 JSON，绝不输出代码块或额外解释。", prompt, 1800)
	if err != nil {
		return nil, err
	}
	content = stripFence(content)
	var proposal []map[string]any
	if err := json.Unmarshal([]byte(content), &proposal); err != nil {
		return nil, fmt.Errorf("decode model proposal: %w", err)
	}
	if len(proposal) < 1 || len(proposal) > 12 {
		return nil, errors.New("model proposal has invalid node count")
	}
	for _, node := range proposal {
		op := strings.TrimSpace(fmt.Sprint(node["op"]))
		if op == "" || op == "<nil>" {
			op = "create"
		}
		switch op {
		case "create":
			if !presentText(node, "title") || !presentText(node, "success_criteria") || !presentText(node, "minimum_action") {
				return nil, errors.New("model proposal is missing required fields")
			}
			typeName := fmt.Sprint(node["type"])
			if typeName != "milestone" && typeName != "task" && typeName != "action" {
				return nil, errors.New("model proposal has invalid node type")
			}
		case "update", "move", "supersede":
			if !presentText(node, "target_id") {
				return nil, errors.New("model revision is missing target_id")
			}
		default:
			return nil, errors.New("model proposal has invalid operation")
		}
	}
	return proposal, nil
}

func presentText(node map[string]any, key string) bool {
	value, exists := node[key]
	if !exists || value == nil {
		return false
	}
	return strings.TrimSpace(fmt.Sprint(value)) != ""
}

func (o *OpenAI) ConversationReply(ctx context.Context, content string) (string, error) {
	system := "你是 FastTask 科研推进助手。用中文简洁回复。先识别进展或阻碍，再给一个 5-15 分钟的最小行动。不要声称已经修改任务树；结构变更必须明确说需要用户确认提案。"
	return o.complete(ctx, system, content, 700)
}

func (o *OpenAI) Transcribe(ctx context.Context, path string) (string, error) {
	if o.transcriptionModel == "" {
		return "", errors.New("OPENAI_TRANSCRIPTION_MODEL is not configured")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, io.LimitReader(file, 32<<20)); err != nil {
		return "", err
	}
	if err := writer.WriteField("model", o.transcriptionModel); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+o.apiKey)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := o.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("transcription request: %w", err)
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("transcription returned status %d", response.StatusCode)
	}
	var result struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return "", err
	}
	if strings.TrimSpace(result.Text) == "" {
		return "", errors.New("transcription returned empty text")
	}
	return strings.TrimSpace(result.Text), nil
}

func (o *OpenAI) complete(ctx context.Context, system, user string, maxTokens int) (string, error) {
	payload := map[string]any{"model": o.model, "messages": []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": user}}, "max_tokens": maxTokens}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+o.apiKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := o.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("model request: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("model returned status %d", response.StatusCode)
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode model response: %w", err)
	}
	if len(result.Choices) == 0 || strings.TrimSpace(result.Choices[0].Message.Content) == "" {
		return "", errors.New("model returned empty response")
	}
	return strings.TrimSpace(result.Choices[0].Message.Content), nil
}

func stripFence(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "```") {
		value = strings.TrimPrefix(value, "```json")
		value = strings.TrimPrefix(value, "```")
		value = strings.TrimSuffix(value, "```")
	}
	return strings.TrimSpace(value)
}
