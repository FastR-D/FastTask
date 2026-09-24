package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/notify"
	"github.com/FastR-D/FastTask/internal/persistence"
	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
)

// The notification endpoints over HTTP (doc/notification.md §9).
//
// What these tests exist for is not that the routes answer — it is that the two rules the
// surface is built on hold through the whole stack: a credential an administrator typed
// never comes back out, and an address a user registered never appears in a response body,
// because for FCM and APNs that address is a device credential.

const telegramTestToken = "123456789:AA-bot-token-value"

// stubSender records the deliveries the API asks for, so a test never reaches a provider.
type stubSender struct {
	mu   sync.Mutex
	sent []stubDelivery
	err  error
}

type stubDelivery struct {
	Address string
	Title   string
	Body    string
}

func (s *stubSender) Kind() string { return notify.KindTelegram }

func (s *stubSender) Verify(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *stubSender) Send(_ context.Context, address string, msg notify.Message) (notify.Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, stubDelivery{Address: address, Title: msg.Title, Body: msg.Body})
	if s.err != nil {
		return notify.Receipt{}, s.err
	}
	return notify.Receipt{ProviderID: "stub-1"}, nil
}

func (s *stubSender) deliveries() []stubDelivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubDelivery(nil), s.sent...)
}

func (s *stubSender) failWith(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

// notificationAPI is a test server whose notification adapters are stubbed, plus the
// member account the self-service half of the surface exists for.
type notificationAPI struct {
	api         testAPI
	sender      *stubSender
	member      persistence.User
	memberToken string
}

func newNotificationAPI(t *testing.T) notificationAPI {
	t.Helper()
	api := newTestAPI(t)
	sender := &stubSender{}
	api.server.app.NotificationService.WithSenderFactory(func(spec notify.Spec) (notify.Sender, error) {
		// Structural validation still runs through the real adapters: it makes no network
		// call, and skipping it would let a test accept a configuration production refuses.
		if _, err := notify.NewSender(spec); err != nil {
			return nil, err
		}
		return sender, nil
	})
	hash, err := platformauth.HashPassword("member-password")
	if err != nil {
		t.Fatal(err)
	}
	now := persistence.Now()
	member := persistence.User{ID: persistence.NewID("user"), Identifier: "researcher", PasswordHash: hash, DisplayName: "研究员", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := api.store.DB.Create(&member).Error; err != nil {
		t.Fatal(err)
	}
	// The member's own login, taken with the admin token out of the way, so the test
	// proves the self-service half is reachable by a non-administrator.
	adminAccess := api.access
	api.access = ""
	login := api.do(t, http.MethodPost, "/api/v1/auth/login", map[string]any{"identifier": member.Identifier, "password": "member-password"}, nil)
	api.access = adminAccess
	if login.Code != http.StatusOK {
		t.Fatalf("member login=%d %s", login.Code, login.Body.String())
	}
	var tokens struct {
		AccessToken string `json:"access_token"`
	}
	decode(t, login, &tokens)
	return notificationAPI{api: api, sender: sender, member: member, memberToken: tokens.AccessToken}
}

// asMember runs one request with the member's bearer token.
func (n notificationAPI) asMember(t *testing.T, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	merged := map[string]string{"Authorization": "Bearer " + n.memberToken}
	for key, value := range headers {
		merged[key] = value
	}
	return n.api.do(t, method, path, body, merged)
}

// createChannel creates a telegram channel through the admin API and returns its view.
func (n notificationAPI) createChannel(t *testing.T, name string) application.ChannelView {
	t.Helper()
	response := n.api.do(t, http.MethodPost, "/api/v1/admin/notifications/channels", map[string]any{
		"name": name, "provider": "telegram", "secret": telegramTestToken,
		"settings": map[string]string{"api_base": "https://telegram.example.test"},
	}, map[string]string{"Idempotency-Key": "channel-" + name})
	if response.Code != http.StatusCreated {
		t.Fatalf("create channel=%d %s", response.Code, response.Body.String())
	}
	var view application.ChannelView
	decode(t, response, &view)
	return view
}

func strPtr(value string) *string { return &value }

func TestAdminNotificationChannelLifecycle(t *testing.T) {
	n := newNotificationAPI(t)
	view := n.createChannel(t, "研究群机器人")

	if view.Provider != "telegram" || view.Status != "active" || view.Revision != 1 {
		t.Fatalf("view=%+v", view)
	}
	if !view.SecretSet || view.SecretHint == "" || strings.Contains(view.SecretHint, "bot-token-value") {
		t.Fatalf("hint=%q set=%v", view.SecretHint, view.SecretSet)
	}
	if view.Settings["api_base"] != "https://telegram.example.test" {
		t.Fatalf("settings=%+v", view.Settings)
	}

	list := n.api.do(t, http.MethodGet, "/api/v1/admin/notifications/channels", nil, nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list=%d %s", list.Code, list.Body.String())
	}
	var channels struct {
		Items []application.ChannelView `json:"items"`
	}
	decode(t, list, &channels)
	if len(channels.Items) != 1 || channels.Items[0].ID != view.ID {
		t.Fatalf("channels=%+v", channels.Items)
	}

	// An edit needs the revision it read, and the credential survives an edit that does not
	// mention it — which is the whole point of never returning one.
	patched := n.api.do(t, http.MethodPatch, "/api/v1/admin/notifications/channels/"+view.ID, map[string]any{
		"name": "研究群机器人（主）", "status": "disabled",
	}, map[string]string{"If-Match": application.StrongETag("notify_channel", view.ID, view.Revision)})
	if patched.Code != http.StatusOK {
		t.Fatalf("patch=%d %s", patched.Code, patched.Body.String())
	}
	var updated application.ChannelView
	decode(t, patched, &updated)
	if updated.Name != "研究群机器人（主）" || updated.Status != "disabled" || updated.Revision != 2 {
		t.Fatalf("updated=%+v", updated)
	}
	if !updated.SecretSet {
		t.Fatal("the stored credential must survive an edit that does not mention it")
	}
	stale := n.api.do(t, http.MethodPatch, "/api/v1/admin/notifications/channels/"+view.ID, map[string]any{"name": "过期"},
		map[string]string{"If-Match": application.StrongETag("notify_channel", view.ID, view.Revision)})
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale patch=%d, want 412", stale.Code)
	}
	// Huma rejects a missing required header with 422; the handler's own precondition
	// check answers 428. Either way the write is refused, which is what matters.
	missing := n.api.do(t, http.MethodPatch, "/api/v1/admin/notifications/channels/"+view.ID, map[string]any{"name": "无前置条件"}, nil)
	if missing.Code != http.StatusUnprocessableEntity && missing.Code != http.StatusPreconditionRequired {
		t.Fatalf("patch without If-Match=%d, want 422 or 428", missing.Code)
	}

	// A configuration the provider cannot use is a 422 carrying the provider's own
	// diagnosis, not a 500 the operator has to guess at.
	rejected := n.api.do(t, http.MethodPost, "/api/v1/admin/notifications/channels", map[string]any{
		"name": "空令牌", "provider": "telegram",
	}, map[string]string{"Idempotency-Key": "channel-invalid"})
	if rejected.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid channel=%d %s", rejected.Code, rejected.Body.String())
	}
	if !strings.Contains(rejected.Body.String(), "bot token") {
		t.Fatalf("the diagnosis was dropped: %s", rejected.Body.String())
	}

	// A failed credential check is a 200 with verified=false: the check worked, the
	// credentials did not. Only a missing channel is a 404.
	n.sender.failWith(notify.ErrUndeliverable)
	if _, err := n.api.server.app.UpdateChannel(context.Background(), n.api.user, view.ID, updated.Revision,
		application.UpdateChannelCommand{Status: strPtr("active")}); err != nil {
		t.Fatal(err)
	}
	verification := n.api.do(t, http.MethodPost, "/api/v1/admin/notifications/channels/"+view.ID+"/verification", nil,
		map[string]string{"Idempotency-Key": "verify-1"})
	if verification.Code != http.StatusOK {
		t.Fatalf("verification=%d %s", verification.Code, verification.Body.String())
	}
	var check application.ChannelCheck
	decode(t, verification, &check)
	if check.Verified || check.Detail == "" {
		t.Fatalf("check=%+v", check)
	}
	n.sender.failWith(nil)
	verification = n.api.do(t, http.MethodPost, "/api/v1/admin/notifications/channels/"+view.ID+"/verification", nil,
		map[string]string{"Idempotency-Key": "verify-2"})
	decode(t, verification, &check)
	if !check.Verified {
		t.Fatalf("check=%+v", check)
	}
	if missing := n.api.do(t, http.MethodPost, "/api/v1/admin/notifications/channels/notify_channel_missing/verification", nil,
		map[string]string{"Idempotency-Key": "verify-3"}); missing.Code != http.StatusNotFound {
		t.Fatalf("verification of a missing channel=%d, want 404", missing.Code)
	}

	// A test send reaches the address the operator typed and stores nothing behind.
	testSend := n.api.do(t, http.MethodPost, "/api/v1/admin/notifications/channels/"+view.ID+"/test",
		map[string]any{"address": "-1001234567890"}, map[string]string{"Idempotency-Key": "test-1"})
	if testSend.Code != http.StatusOK {
		t.Fatalf("test=%d %s", testSend.Code, testSend.Body.String())
	}
	var result application.DeliveryResult
	decode(t, testSend, &result)
	if !result.Delivered || result.ProviderMessageID != "stub-1" {
		t.Fatalf("result=%+v", result)
	}
	if deliveries := n.sender.deliveries(); len(deliveries) != 1 || deliveries[0].Address != "-1001234567890" {
		t.Fatalf("deliveries=%+v", deliveries)
	}
	var targets int64
	if err := n.api.store.DB.Model(&persistence.NotificationTarget{}).Count(&targets).Error; err != nil {
		t.Fatal(err)
	}
	if targets != 0 {
		t.Fatalf("targets=%d; a test send must not subscribe anybody", targets)
	}

	deleted := n.api.do(t, http.MethodDelete, "/api/v1/admin/notifications/channels/"+view.ID, nil, nil)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete=%d %s", deleted.Code, deleted.Body.String())
	}
	if again := n.api.do(t, http.MethodDelete, "/api/v1/admin/notifications/channels/"+view.ID, nil, nil); again.Code != http.StatusNotFound {
		t.Fatalf("delete twice=%d, want 404", again.Code)
	}
}

func TestAdminNotificationEndpointsRequireAnAdministrator(t *testing.T) {
	n := newNotificationAPI(t)
	view := n.createChannel(t, "权限")

	for _, testCase := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/admin/notifications/channels", nil},
		{http.MethodPost, "/api/v1/admin/notifications/channels", map[string]any{"name": "x", "provider": "bark", "endpoint": "https://bark.example.test"}},
		{http.MethodGet, "/api/v1/admin/notifications/targets", nil},
		{http.MethodGet, "/api/v1/admin/notifications/messages", nil},
		{http.MethodPost, "/api/v1/admin/notifications/broadcast", map[string]any{"title": "全站通知"}},
		{http.MethodPost, "/api/v1/admin/notifications/dispatch", nil},
		{http.MethodPost, "/api/v1/admin/notifications/channels/" + view.ID + "/verification", nil},
		{http.MethodPost, "/api/v1/admin/notifications/channels/" + view.ID + "/test", map[string]any{"address": "-1001234567890"}},
		{http.MethodDelete, "/api/v1/admin/notifications/channels/" + view.ID, nil},
	} {
		response := n.asMember(t, testCase.method, testCase.path, testCase.body, map[string]string{"Idempotency-Key": "member-" + testCase.path})
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s %s as a member=%d %s, want 403", testCase.method, testCase.path, response.Code, response.Body.String())
		}
	}
}

func TestNotificationTargetSelfService(t *testing.T) {
	n := newNotificationAPI(t)
	view := n.createChannel(t, "自助订阅")

	// A member sees the channels they may subscribe to, and only the active ones.
	listed := n.asMember(t, http.MethodGet, "/api/v1/notifications/channels", nil, nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("channels=%d %s", listed.Code, listed.Body.String())
	}
	var available struct {
		Items []application.ChannelView `json:"items"`
	}
	decode(t, listed, &available)
	if len(available.Items) != 1 || available.Items[0].ID != view.ID {
		t.Fatalf("items=%+v", available.Items)
	}

	created := n.asMember(t, http.MethodPost, "/api/v1/notifications/targets", map[string]any{
		"channel_id": view.ID, "address": "-1001234567890", "label": "研究群",
	}, map[string]string{"Idempotency-Key": "target-1"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create target=%d %s", created.Code, created.Body.String())
	}
	var target application.TargetView
	decode(t, created, &target)
	if target.UserID != n.member.ID || target.Label != "研究群" || target.Status != "active" {
		t.Fatalf("target=%+v", target)
	}
	if target.AddressHint == "-1001234567890" || !strings.HasSuffix(target.AddressHint, "7890") {
		t.Fatalf("hint=%q; the address is a credential and must come back masked", target.AddressHint)
	}
	if strings.Contains(created.Body.String(), "-1001234567890") {
		t.Fatalf("the response carries the plaintext address: %s", created.Body.String())
	}

	// A member registers their own address; the owner is the caller, never the body. The
	// self-service contract does not even have a user_id field, so the attempt is refused
	// as an unexpected property before any handler runs.
	if hijack := n.asMember(t, http.MethodPost, "/api/v1/notifications/targets", map[string]any{
		"channel_id": view.ID, "address": "-1002222222222", "user_id": n.api.user.ID,
	}, map[string]string{"Idempotency-Key": "target-2"}); hijack.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a body choosing the owner=%d %s, want 422", hijack.Code, hijack.Body.String())
	}
	second := n.asMember(t, http.MethodPost, "/api/v1/notifications/targets", map[string]any{
		"channel_id": view.ID, "address": "-1002222222222",
	}, map[string]string{"Idempotency-Key": "target-3"})
	if second.Code != http.StatusCreated {
		t.Fatalf("second target=%d %s", second.Code, second.Body.String())
	}
	var secondTarget application.TargetView
	decode(t, second, &secondTarget)
	if secondTarget.UserID != n.member.ID {
		t.Fatalf("target=%+v; the owner must be the caller", secondTarget)
	}

	mine := n.asMember(t, http.MethodGet, "/api/v1/notifications/targets", nil, nil)
	var targets struct {
		Items []application.TargetView `json:"items"`
	}
	decode(t, mine, &targets)
	if len(targets.Items) != 2 {
		t.Fatalf("targets=%+v", targets.Items)
	}

	patched := n.asMember(t, http.MethodPatch, "/api/v1/notifications/targets/"+target.ID, map[string]any{
		"label": "旧手机", "status": "disabled",
	}, map[string]string{"If-Match": application.StrongETag("notify_target", target.ID, target.Revision)})
	if patched.Code != http.StatusOK {
		t.Fatalf("patch=%d %s", patched.Code, patched.Body.String())
	}
	var renamed application.TargetView
	decode(t, patched, &renamed)
	if renamed.Label != "旧手机" || renamed.Status != "disabled" || renamed.Revision != target.Revision+1 {
		t.Fatalf("renamed=%+v", renamed)
	}

	tested := n.asMember(t, http.MethodPost, "/api/v1/notifications/targets/"+secondTarget.ID+"/test", nil,
		map[string]string{"Idempotency-Key": "target-test"})
	if tested.Code != http.StatusOK {
		t.Fatalf("test=%d %s", tested.Code, tested.Body.String())
	}
	var result application.DeliveryResult
	decode(t, tested, &result)
	if !result.Delivered {
		t.Fatalf("result=%+v", result)
	}

	deleted := n.asMember(t, http.MethodDelete, "/api/v1/notifications/targets/"+target.ID, nil, nil)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete=%d %s", deleted.Code, deleted.Body.String())
	}
	if again := n.asMember(t, http.MethodDelete, "/api/v1/notifications/targets/"+target.ID, nil, nil); again.Code != http.StatusNotFound {
		t.Fatalf("delete twice=%d, want 404", again.Code)
	}

	// The delivery log a member reads is their own, and it names no one else.
	log := n.asMember(t, http.MethodGet, "/api/v1/notifications/messages?limit=10", nil, nil)
	if log.Code != http.StatusOK {
		t.Fatalf("log=%d %s", log.Code, log.Body.String())
	}
	var messages struct {
		Items []application.MessageView `json:"items"`
	}
	decode(t, log, &messages)
	if len(messages.Items) != 1 || messages.Items[0].Topic != application.TopicNotificationTest {
		t.Fatalf("messages=%+v", messages.Items)
	}
	for _, message := range messages.Items {
		if message.UserID != n.member.ID {
			t.Fatalf("message=%+v belongs to somebody else", message)
		}
	}
}

func TestNotificationTargetSelfServiceCannotReachAnotherUser(t *testing.T) {
	n := newNotificationAPI(t)
	view := n.createChannel(t, "越权")
	adminTarget, err := n.api.server.app.CreateTarget(context.Background(), &n.api.user, n.api.user.ID,
		application.TargetCommand{ChannelID: view.ID, Address: "-1001234567890", Label: "管理员的手机"})
	if err != nil {
		t.Fatal(err)
	}

	// A member's requests are scoped by their own token, so another user's target is a 404
	// rather than a 403: the answer must not confirm the id exists.
	if response := n.asMember(t, http.MethodPatch, "/api/v1/notifications/targets/"+adminTarget.ID,
		map[string]any{"label": "偷来的"}, map[string]string{"If-Match": application.StrongETag("notify_target", adminTarget.ID, adminTarget.Revision)}); response.Code != http.StatusNotFound {
		t.Fatalf("patch=%d, want 404", response.Code)
	}
	if response := n.asMember(t, http.MethodPost, "/api/v1/notifications/targets/"+adminTarget.ID+"/test", nil,
		map[string]string{"Idempotency-Key": "steal-test"}); response.Code != http.StatusNotFound {
		t.Fatalf("test=%d, want 404", response.Code)
	}
	if response := n.asMember(t, http.MethodDelete, "/api/v1/notifications/targets/"+adminTarget.ID, nil, nil); response.Code != http.StatusNotFound {
		t.Fatalf("delete=%d, want 404", response.Code)
	}
	listed := n.asMember(t, http.MethodGet, "/api/v1/notifications/targets", nil, nil)
	var targets struct {
		Items []application.TargetView `json:"items"`
	}
	decode(t, listed, &targets)
	if len(targets.Items) != 0 {
		t.Fatalf("targets=%+v, want none of the administrator's", targets.Items)
	}
}

func TestAdminManagesTargetsDispatchesAndReadsTheLog(t *testing.T) {
	n := newNotificationAPI(t)
	view := n.createChannel(t, "后台管理")

	// The console registers a subscription on a user's behalf, which is what makes the
	// admin page a management surface rather than a config form.
	created := n.api.do(t, http.MethodPost, "/api/v1/admin/notifications/targets", map[string]any{
		"user_id": n.member.ID, "channel_id": view.ID, "address": "-1001234567890", "label": "研究群",
	}, map[string]string{"Idempotency-Key": "admin-target-1"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create target=%d %s", created.Code, created.Body.String())
	}
	var target application.TargetView
	decode(t, created, &target)
	if target.UserID != n.member.ID || target.UserIdentifier != "researcher" || target.ChannelName != "后台管理" {
		t.Fatalf("target=%+v", target)
	}
	// An unknown user is a 422, not a foreign-key failure surfacing as a 500.
	if response := n.api.do(t, http.MethodPost, "/api/v1/admin/notifications/targets", map[string]any{
		"user_id": "user_missing", "channel_id": view.ID, "address": "-1001234567891",
	}, map[string]string{"Idempotency-Key": "admin-target-2"}); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown user=%d %s, want 422", response.Code, response.Body.String())
	}

	listed := n.api.do(t, http.MethodGet, "/api/v1/admin/notifications/targets?user_id="+n.member.ID, nil, nil)
	var targets struct {
		Items []application.TargetView `json:"items"`
	}
	decode(t, listed, &targets)
	if len(targets.Items) != 1 || targets.Items[0].ID != target.ID {
		t.Fatalf("targets=%+v", targets.Items)
	}

	// An announcement queues for everybody the channel can reach, and the operator's own
	// pass delivers it without waiting for the scheduler.
	broadcast := n.api.do(t, http.MethodPost, "/api/v1/admin/notifications/broadcast", map[string]any{
		"title": "维护通知", "body": "今晚 23:00 停机十分钟", "channel_id": view.ID,
	}, map[string]string{"Idempotency-Key": "broadcast-1"})
	if broadcast.Code != http.StatusOK {
		t.Fatalf("broadcast=%d %s", broadcast.Code, broadcast.Body.String())
	}
	var queued struct {
		Queued int `json:"queued"`
	}
	decode(t, broadcast, &queued)
	if queued.Queued != 1 {
		t.Fatalf("queued=%d", queued.Queued)
	}
	dispatch := n.api.do(t, http.MethodPost, "/api/v1/admin/notifications/dispatch", nil,
		map[string]string{"Idempotency-Key": "dispatch-1"})
	if dispatch.Code != http.StatusOK {
		t.Fatalf("dispatch=%d %s", dispatch.Code, dispatch.Body.String())
	}
	var report application.DispatchReport
	decode(t, dispatch, &report)
	if report.Claimed != 1 || report.Sent != 1 {
		t.Fatalf("report=%+v", report)
	}
	deliveries := n.sender.deliveries()
	if len(deliveries) != 1 || deliveries[0].Address != "-1001234567890" || deliveries[0].Title != "维护通知" {
		t.Fatalf("deliveries=%+v", deliveries)
	}

	log := n.api.do(t, http.MethodGet, "/api/v1/admin/notifications/messages?limit=10", nil, nil)
	var messages struct {
		Items []application.MessageView `json:"items"`
	}
	decode(t, log, &messages)
	if len(messages.Items) != 1 {
		t.Fatalf("messages=%+v", messages.Items)
	}
	got := messages.Items[0]
	if got.Status != "sent" || got.UserIdentifier != "researcher" || got.ChannelName != "后台管理" || got.Provider != "telegram" {
		t.Fatalf("message=%+v", got)
	}
	if got.Topic != application.TopicNotificationBroadcast || got.ProviderMessageID != "stub-1" {
		t.Fatalf("message=%+v", got)
	}

	// Removing a subscription takes its history with it: the log holds a masked copy of a
	// device credential, and there is nothing left to diagnose once the target is gone.
	if response := n.api.do(t, http.MethodDelete, "/api/v1/admin/notifications/targets/"+target.ID, nil, nil); response.Code != http.StatusNoContent {
		t.Fatalf("delete target=%d %s", response.Code, response.Body.String())
	}
	log = n.api.do(t, http.MethodGet, "/api/v1/admin/notifications/messages?limit=10", nil, nil)
	decode(t, log, &messages)
	if len(messages.Items) != 0 {
		t.Fatalf("messages=%+v, want the deleted target's history gone", messages.Items)
	}
}

func TestNotificationResponsesCarryNoCredential(t *testing.T) {
	n := newNotificationAPI(t)
	view := n.createChannel(t, "不回显")
	target, err := n.api.server.app.CreateTarget(context.Background(), &n.api.user, n.member.ID,
		application.TargetCommand{ChannelID: view.ID, Address: "-1001234567890"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.api.server.app.Publish(context.Background(), n.member.ID, application.Event{
		Topic: application.TopicProposalPending, Title: "有待确认的任务树变更",
	}); err != nil {
		t.Fatal(err)
	}

	for _, response := range []*httptest.ResponseRecorder{
		n.api.do(t, http.MethodGet, "/api/v1/admin/notifications/channels", nil, nil),
		n.api.do(t, http.MethodGet, "/api/v1/admin/notifications/targets", nil, nil),
		n.asMember(t, http.MethodGet, "/api/v1/notifications/targets", nil, nil),
		n.asMember(t, http.MethodGet, "/api/v1/notifications/channels", nil, nil),
		n.api.do(t, http.MethodPost, "/api/v1/admin/notifications/dispatch", nil, map[string]string{"Idempotency-Key": "dispatch-2"}),
		n.api.do(t, http.MethodGet, "/api/v1/admin/notifications/messages", nil, nil),
		n.asMember(t, http.MethodGet, "/api/v1/notifications/messages", nil, nil),
		n.api.do(t, http.MethodPost, "/api/v1/admin/notifications/channels/"+view.ID+"/test",
			map[string]any{"address": "-1001234567890"}, map[string]string{"Idempotency-Key": "test-2"}),
		n.api.do(t, http.MethodPost, "/api/v1/admin/notifications/targets/"+target.ID+"/test", nil,
			map[string]string{"Idempotency-Key": "test-3"}),
		n.api.do(t, http.MethodGet, "/api/v1/admin/audit-events?limit=100", nil, nil),
	} {
		body := response.Body.String()
		if response.Code >= 400 {
			t.Fatalf("status=%d body=%s", response.Code, body)
		}
		for _, secret := range []string{telegramTestToken, "AA-bot-token-value", "-1001234567890"} {
			if strings.Contains(body, secret) {
				t.Fatalf("a credential appears in a response: %s", body)
			}
		}
	}

	// The audit log records that an administrator did something, never what they typed.
	var events []persistence.AdminAuditEvent
	if err := n.api.store.DB.Where("action LIKE 'notification%'").Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("the console wrote no audit events")
	}
	for _, event := range events {
		if strings.Contains(event.DetailJSON, telegramTestToken) || strings.Contains(event.DetailJSON, "-1001234567890") {
			t.Fatalf("the audit log carries a credential: %s", event.DetailJSON)
		}
	}
}
