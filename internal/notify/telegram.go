package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The Telegram Bot adapter (Bot API, https://core.telegram.org/bots/api).
//
// A channel holds one bot token; a target holds one chat id — the user, group or
// channel the bot may post to. The bot must already be a member of a group or
// channel: Telegram decides reachability, and this adapter only reports it.
//
// One secret and two optional settings:
//
//	secret                the bot token from @BotFather (required)
//	endpoint              API base, default https://api.telegram.org. A deployment
//	                      behind an egress proxy, or running the self-hosted Bot API
//	                      server, sets this.
//	settings["api_base"]  accepted as an alias for endpoint
//
// Messages are sent as plain text. Telegram's parse_mode would require escaping
// every task title a user typed, and a notification that fails to parse is
// delivered to nobody.

// SettingTelegramAPIBase is the alias for Endpoint in a Telegram channel's settings.
const SettingTelegramAPIBase = "api_base"

const defaultTelegramAPI = "https://api.telegram.org"

type telegramSender struct {
	client  *http.Client
	timeout time.Duration
	base    string
	token   string
}

func newTelegram(spec Spec) (Sender, error) {
	token := strings.TrimSpace(spec.Secret)
	if token == "" {
		return nil, errors.New("telegram: a bot token is required")
	}
	base := strings.TrimSpace(spec.Endpoint)
	if base == "" {
		base = spec.setting(SettingTelegramAPIBase)
	}
	if base == "" {
		base = defaultTelegramAPI
	}
	base = strings.TrimRight(base, "/")
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("telegram: %q is not an http(s) API base", base)
	}
	timeout := spec.timeout()
	return &telegramSender{client: spec.client(timeout), timeout: timeout, base: base, token: token}, nil
}

func (t *telegramSender) Kind() string { return KindTelegram }

func (t *telegramSender) Verify(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, t.base+"/bot"+t.token+"/getMe", nil)
	if err != nil {
		return err
	}
	var body telegramResponse
	response, err := exchange(t.client, request, &body)
	if err != nil {
		return err
	}
	if !body.OK {
		return classifyTelegram(response.Status, body)
	}
	return nil
}

func (t *telegramSender) Send(ctx context.Context, address string, msg Message) (Receipt, error) {
	chatID := strings.TrimSpace(address)
	if chatID == "" {
		return Receipt{}, ErrNoAddress
	}
	payload := map[string]any{"chat_id": chatID, "text": msg.text(), "disable_web_page_preview": true}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Receipt{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+"/bot"+t.token+"/sendMessage", bytes.NewReader(encoded))
	if err != nil {
		return Receipt{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	var body telegramResponse
	response, err := exchange(t.client, request, &body)
	if err != nil {
		return Receipt{}, err
	}
	if !body.OK {
		return Receipt{}, classifyTelegram(response.Status, body)
	}
	return Receipt{ProviderID: fmt.Sprintf("%d", body.Result.MessageID)}, nil
}

// telegramResponse is the envelope every Bot API method answers with.
type telegramResponse struct {
	OK          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
	Result      struct {
		MessageID int64  `json:"message_id"`
		Username  string `json:"username"`
	} `json:"result"`
	Parameters struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// classifyTelegram turns the envelope into either ErrInvalidTarget or an error
// worth retrying. Telegram reports a dead chat in the description text rather than
// in a code, so the wording is matched — but only after the code has narrowed it to
// the two statuses that can mean "this chat will never accept a message again":
// 400 (chat not found) and 403 (the bot was blocked or kicked). A 429 keeps the
// retry_after in the message so an operator sees why deliveries are delayed.
func classifyTelegram(status int, body telegramResponse) error {
	code := body.ErrorCode
	if code == 0 {
		code = status
	}
	description := body.Description
	switch {
	case code == http.StatusTooManyRequests:
		if body.Parameters.RetryAfter > 0 {
			description = fmt.Sprintf("%s (retry after %ds)", description, body.Parameters.RetryAfter)
		}
	case code == http.StatusBadRequest && containsAny(description, "chat not found", "user not found", "have no rights", "wrong file identifier"):
		return fmt.Errorf("%w: telegram refused the chat: %s", ErrInvalidTarget, description)
	case code == http.StatusForbidden && containsAny(description, "blocked", "kicked", "deactivated", "restricted", "have no rights"):
		return fmt.Errorf("%w: telegram refused the chat: %s", ErrInvalidTarget, description)
	case code == http.StatusUnauthorized:
		// The bot token itself is wrong or was revoked through @BotFather. No chat
		// will ever accept a message from it, but none of them is at fault either.
		return fmt.Errorf("%w: telegram refused the bot token", ErrUndeliverable)
	}
	return providerError(KindTelegram, code, description)
}
