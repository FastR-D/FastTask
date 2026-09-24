package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The Bark adapter (https://github.com/Finb/bark-server).
//
// Bark is the iOS push endpoint that needs no Apple Developer account: the user
// installs Bark, copies a device key, and any server may post to it. A channel
// therefore holds only the server; the device key is the target's address, which
// is what makes Bark usable per-user rather than per-deployment.
//
//	endpoint           the Bark server base, e.g. https://api.day.app (required)
//	settings["server"] accepted as an alias for endpoint
//	settings["group"]  notification grouping, default "FastTask"
//	settings["sound"]  optional alert sound
//	settings["icon"]   optional icon URL
//	settings["level"]  active | timeSensitive | passive | critical
//	secret             unused — a Bark server authenticates by device key
//
// The push goes to {server}/push with the device key in the body rather than to
// {server}/{key}: a key in a path lands in proxy and access logs, and it is the
// only credential the user has.

// The Bark channel settings. They are the payload fields Bark defines, minus the
// per-device key and the per-message title/body.
const (
	SettingBarkServer = "server"
	SettingBarkGroup  = "group"
	SettingBarkSound  = "sound"
	SettingBarkIcon   = "icon"
	SettingBarkLevel  = "level"
)

// DefaultBarkGroup is the grouping a channel gets when it sets none, so FastTask
// notifications are one adjustable group in iOS rather than a stream of banners.
const DefaultBarkGroup = "FastTask"

var barkLevels = map[string]bool{"active": true, "timeSensitive": true, "passive": true, "critical": true}

type barkSender struct {
	client  *http.Client
	timeout time.Duration
	server  string
	group   string
	sound   string
	icon    string
	level   string
}

func newBark(spec Spec) (Sender, error) {
	server := strings.TrimSpace(spec.Endpoint)
	if server == "" {
		server = spec.setting(SettingBarkServer)
	}
	if server == "" {
		return nil, errors.New("bark: a server base URL is required")
	}
	server = strings.TrimRight(server, "/")
	parsed, err := url.Parse(server)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("bark: %q is not an http(s) server", server)
	}
	level := spec.setting(SettingBarkLevel)
	if level != "" && !barkLevels[level] {
		return nil, fmt.Errorf("bark: %q is not one of active, timeSensitive, passive, critical", level)
	}
	group := spec.setting(SettingBarkGroup)
	if group == "" {
		group = DefaultBarkGroup
	}
	timeout := spec.timeout()
	return &barkSender{
		client: spec.client(timeout), timeout: timeout, server: server, group: group,
		sound: spec.setting(SettingBarkSound), icon: spec.setting(SettingBarkIcon), level: level,
	}, nil
}

func (b *barkSender) Kind() string { return KindBark }

func (b *barkSender) Verify(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, b.server+"/ping", nil)
	if err != nil {
		return err
	}
	response, err := exchange(b.client, request, nil)
	if err != nil {
		return err
	}
	if response.Status != http.StatusOK {
		return providerError(KindBark, response.Status, string(response.Body))
	}
	// bark-server answers "success"; api.day.app answers the same. Anything else on
	// a 200 is a proxy in front, not a Bark server.
	if body := strings.ToLower(string(response.Body)); !strings.Contains(body, "success") && !strings.Contains(body, "pong") {
		return errors.New("bark: the server did not answer its ping")
	}
	return nil
}

func (b *barkSender) Send(ctx context.Context, address string, msg Message) (Receipt, error) {
	deviceKey := strings.TrimSpace(address)
	if deviceKey == "" {
		return Receipt{}, ErrNoAddress
	}
	payload := map[string]any{
		"device_key": deviceKey,
		"title":      strings.TrimSpace(msg.Title),
		"body":       strings.TrimSpace(msg.Body),
		"group":      b.group,
	}
	if msg.URL != "" {
		payload["url"] = msg.URL
	}
	if b.sound != "" {
		payload["sound"] = b.sound
	}
	if b.icon != "" {
		payload["icon"] = b.icon
	}
	if b.level != "" {
		payload["level"] = b.level
	}
	if msg.Topic != "" {
		payload["action"] = msg.Topic
	}
	if msg.CollapseID != "" {
		// Bark has no collapse id; the closest thing it offers is replacing the
		// previous notification in the group, which is what a collapse id asks for.
		payload["isArchive"] = 1
	}
	for key, value := range msg.Data {
		if _, taken := payload[key]; !taken {
			payload[key] = value
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Receipt{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.server+"/push", bytes.NewReader(encoded))
	if err != nil {
		return Receipt{}, err
	}
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	var body barkResponse
	response, err := exchange(b.client, request, &body)
	if err != nil {
		return Receipt{}, err
	}
	if code := body.code(response.Status); code != http.StatusOK {
		if code == http.StatusNotFound || (code == http.StatusBadRequest && containsAny(body.Message, "device key", "device", "invalid")) {
			return Receipt{}, fmt.Errorf("%w: bark refused the device key: %s", ErrInvalidTarget, strings.TrimSpace(body.Message))
		}
		return Receipt{}, providerError(KindBark, code, body.Message)
	}
	return Receipt{}, nil
}

// barkResponse is Bark's envelope. The code field is a JSON number on bark-server
// and a string on some deployments, so it is decoded loosely and falls back to the
// HTTP status when absent.
type barkResponse struct {
	Code    any    `json:"code"`
	Message string `json:"message"`
}

func (r barkResponse) code(httpStatus int) int {
	switch value := r.Code.(type) {
	case float64:
		return int(value)
	case json.Number:
		if parsed, err := value.Int64(); err == nil {
			return int(parsed)
		}
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			return parsed
		}
	}
	return httpStatus
}
