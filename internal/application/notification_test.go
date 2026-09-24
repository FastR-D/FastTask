package application

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/notify"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// notificationFixture is a store, an app with an encryption key configured, one member
// and one administrator. Every notification test starts from here.
type notificationFixture struct {
	app    *App
	store  *persistence.Store
	user   persistence.User
	admin  persistence.User
	specs  []notify.Spec
	mu     sync.Mutex
	sender *scriptedSender
}

// scriptedSender is the provider stand-in. It records what it was asked to send and
// answers with the error the test scripted, so delivery semantics are tested without a
// network and without pretending a provider behaved like one.
//
// It is guarded by a mutex because the test that runs two dispatchers at once has two
// goroutines that could both reach it.
type scriptedSender struct {
	mu        sync.Mutex
	kind      string
	verifyErr error
	sendErr   error
	receipt   notify.Receipt
	sent      []sentMessage
}

type sentMessage struct {
	Address string
	Message notify.Message
}

func (s *scriptedSender) Kind() string { return s.kind }

func (s *scriptedSender) Verify(context.Context) error { return s.verifyErr }

func (s *scriptedSender) failWith(err error) {
	s.mu.Lock()
	s.sendErr = err
	s.mu.Unlock()
}

func (s *scriptedSender) deliveries() []sentMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sentMessage(nil), s.sent...)
}

func (s *scriptedSender) forget() {
	s.mu.Lock()
	s.sent = nil
	s.mu.Unlock()
}

func (s *scriptedSender) Send(_ context.Context, address string, msg notify.Message) (notify.Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, sentMessage{Address: address, Message: msg})
	if s.sendErr != nil {
		return notify.Receipt{}, s.sendErr
	}
	if s.receipt.ProviderID == "" {
		return notify.Receipt{ProviderID: "provider-1"}, nil
	}
	return s.receipt, nil
}

func newNotificationFixture(t *testing.T) *notificationFixture {
	t.Helper()
	store, err := persistence.Open(filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := persistence.Now()
	user := persistence.User{ID: persistence.NewID("user"), Identifier: "member", PasswordHash: "hash", DisplayName: "成员", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	admin := persistence.User{ID: persistence.NewID("user"), Identifier: "admin", PasswordHash: "hash", DisplayName: "管理员", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "admin", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	for _, record := range []*persistence.User{&user, &admin} {
		if err := store.DB.Create(record).Error; err != nil {
			t.Fatal(err)
		}
	}
	app := NewWithSecret(store, "notification-test-secret-with-length")
	// The fixture is returned as a pointer because the sender factory's closure captures
	// this variable: the specs it records have to be readable through the same struct the
	// test holds.
	fixture := &notificationFixture{app: app, store: store, user: user, admin: admin, sender: &scriptedSender{kind: notify.KindTelegram}}
	app.NotificationService.WithSenderFactory(func(spec notify.Spec) (notify.Sender, error) {
		// Structural validation still runs through the real adapters: it makes no network
		// call, and skipping it would let a test pass a configuration production refuses.
		if _, err := notify.NewSender(spec); err != nil {
			return nil, err
		}
		fixture.mu.Lock()
		fixture.specs = append(fixture.specs, spec)
		fixture.mu.Unlock()
		fixture.sender.mu.Lock()
		fixture.sender.kind = spec.Kind
		fixture.sender.mu.Unlock()
		return fixture.sender, nil
	})
	return fixture
}

// channel creates an active telegram channel and returns its view.
func (f *notificationFixture) channel(t *testing.T, name string) ChannelView {
	t.Helper()
	view, err := f.app.CreateChannel(context.Background(), f.admin, ChannelCommand{
		Name: name, Provider: notify.KindTelegram, Secret: "123456789:bot-token-value",
		Settings: map[string]string{"api_base": "https://telegram.example.test"},
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	return *view
}

// target registers an address for the fixture's member and returns its view.
func (f *notificationFixture) target(t *testing.T, channelID, address, label string) TargetView {
	t.Helper()
	view, err := f.app.CreateTarget(context.Background(), &f.admin, f.user.ID, TargetCommand{
		ChannelID: channelID, Address: address, Label: label,
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	return *view
}

// messages reads the queue for the fixture's member.
func (f *notificationFixture) messages(t *testing.T) []persistence.NotificationMessage {
	t.Helper()
	var rows []persistence.NotificationMessage
	if err := f.store.DB.Where("user_id = ?", f.user.ID).Order("created_at").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestCreateChannelEncryptsTheCredentialAndNeverReturnsIt(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "研究群机器人")

	if view.Provider != notify.KindTelegram || view.Status != "active" {
		t.Fatalf("view=%+v", view)
	}
	if view.SecretSet != true || view.SecretHint == "" || strings.Contains(view.SecretHint, "bot-token-value") {
		t.Fatalf("hint=%q secret_set=%v; the credential must be hinted, not shown", view.SecretHint, view.SecretSet)
	}
	if view.Settings["api_base"] != "https://telegram.example.test" {
		t.Fatalf("settings=%+v", view.Settings)
	}
	if view.TargetCount != 0 || view.ActiveTargetCount != 0 {
		t.Fatalf("a new channel has no targets: %+v", view)
	}

	var row persistence.NotificationChannel
	if err := f.store.DB.First(&row, "id = ?", view.ID).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.SecretCiphertext, "bot-token-value") {
		t.Fatal("the credential was stored in plaintext")
	}
	if !strings.HasPrefix(row.SecretCiphertext, "aesgcm.v1:") {
		t.Fatalf("ciphertext=%q", row.SecretCiphertext)
	}
	// The notification key is derived under its own label, so a model-provider ciphertext
	// and a notification credential are not interchangeable.
	if decrypted, err := decryptWithKey(deriveSecretKey("notification-test-secret-with-length"), row.SecretCiphertext); err == nil {
		t.Fatalf("the provider key decrypted a notification secret: %q", decrypted)
	}
	decrypted, err := decryptWithKey(deriveNotificationKey("notification-test-secret-with-length"), row.SecretCiphertext)
	if err != nil || decrypted != "123456789:bot-token-value" {
		t.Fatalf("decrypted=%q err=%v", decrypted, err)
	}
}

func TestCreateChannelValidatesThroughTheAdapter(t *testing.T) {
	f := newNotificationFixture(t)
	// A telegram channel without a token cannot send, so it must not be storable. The
	// adapter is the authority on what its provider needs, and it is asked at the moment
	// an administrator types the configuration rather than at the first delivery.
	_, err := f.app.CreateChannel(context.Background(), f.admin, ChannelCommand{Name: "空令牌", Provider: notify.KindTelegram})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err=%v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "bot token") {
		t.Fatalf("the diagnosis was dropped: %v", err)
	}
	if _, err := f.app.CreateChannel(context.Background(), f.admin, ChannelCommand{Name: "未知", Provider: "wechat"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("err=%v, want ErrValidation for an unsupported provider", err)
	}
	// Bark needs no credential, so an empty secret is not a fault for it.
	view, err := f.app.CreateChannel(context.Background(), f.admin, ChannelCommand{
		Name: "Bark 推送", Provider: notify.KindBark, Endpoint: "https://bark.example.test",
	})
	if err != nil {
		t.Fatalf("a credential-free provider must be storable: %v", err)
	}
	if view.SecretSet {
		t.Fatal("secret_set must be false when no credential was given")
	}
}

func TestCreateChannelRejectsADuplicateName(t *testing.T) {
	f := newNotificationFixture(t)
	f.channel(t, "重复")
	if _, err := f.app.CreateChannel(context.Background(), f.admin, ChannelCommand{
		Name: "重复", Provider: notify.KindTelegram, Secret: "123456789:another-token",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("err=%v, want ErrConflict", err)
	}
}

func TestUpdateChannelKeepsTheStoredCredentialAndGuardsTheRevision(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "待编辑")

	endpoint := "https://telegram2.example.test"
	updated, err := f.app.UpdateChannel(context.Background(), f.admin, view.ID, view.Revision, UpdateChannelCommand{Endpoint: &endpoint})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Revision != view.Revision+1 || updated.Endpoint != endpoint {
		t.Fatalf("updated=%+v", updated)
	}
	if !updated.SecretSet {
		t.Fatal("an update that does not mention the secret must keep it")
	}

	// The adapter is asked about the effective configuration, so the stored credential has
	// to be decrypted and handed to it — which is also how a settings edit that breaks the
	// provider is caught here.
	f.mu.Lock()
	spec := f.specs[len(f.specs)-1]
	f.mu.Unlock()
	if spec.Secret != "123456789:bot-token-value" || spec.Endpoint != endpoint {
		t.Fatalf("spec=%+v", spec)
	}

	if _, err := f.app.UpdateChannel(context.Background(), f.admin, view.ID, view.Revision, UpdateChannelCommand{Endpoint: &endpoint}); !errors.Is(err, ErrRevision) {
		t.Fatalf("err=%v, want ErrRevision", err)
	}
	empty := "  "
	if _, err := f.app.UpdateChannel(context.Background(), f.admin, updated.ID, updated.Revision, UpdateChannelCommand{Name: &empty}); !errors.Is(err, ErrValidation) {
		t.Fatalf("err=%v, want ErrValidation", err)
	}
}

func TestUpdateChannelAuditsTheChangeWithoutTheCredential(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "审计")
	replacement := "987654321:replacement-token"
	if _, err := f.app.UpdateChannel(context.Background(), f.admin, view.ID, view.Revision, UpdateChannelCommand{Secret: &replacement}); err != nil {
		t.Fatal(err)
	}
	var events []persistence.AdminAuditEvent
	if err := f.store.DB.Where("action LIKE 'notification_channel.%'").Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events=%d, want the create and the update", len(events))
	}
	for _, event := range events {
		if strings.Contains(event.DetailJSON, "replacement-token") || strings.Contains(event.DetailJSON, "bot-token-value") {
			t.Fatalf("the audit log carries a credential: %s", event.DetailJSON)
		}
	}
	if !strings.Contains(events[1].DetailJSON, `"secret_replaced":true`) {
		t.Fatalf("the audit log lost the fact that the credential changed: %s", events[1].DetailJSON)
	}
}

func TestDeleteChannelTakesItsTargetsAndHistory(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "待删除")
	f.target(t, view.ID, "-1001234567890", "研究群")
	if _, err := f.app.Publish(context.Background(), f.user.ID, Event{Topic: TopicProposalPending, Title: "有提案"}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.DeleteChannel(context.Background(), f.admin, view.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var targets, messages int64
	if err := f.store.DB.Model(&persistence.NotificationTarget{}).Count(&targets).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB.Model(&persistence.NotificationMessage{}).Count(&messages).Error; err != nil {
		t.Fatal(err)
	}
	if targets != 0 || messages != 0 {
		t.Fatalf("targets=%d messages=%d; a revoked credential must not leave reachable addresses behind", targets, messages)
	}
}

func TestTargetStoresAnEncryptedAddressAndAMaskedHint(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "地址")
	target := f.target(t, view.ID, "-1001234567890", "研究群")

	if target.AddressHint == "-1001234567890" || !strings.HasSuffix(target.AddressHint, "7890") {
		t.Fatalf("hint=%q", target.AddressHint)
	}
	if target.UserID != f.user.ID || target.ChannelName != "地址" || target.Provider != notify.KindTelegram {
		t.Fatalf("target=%+v", target)
	}
	var row persistence.NotificationTarget
	if err := f.store.DB.First(&row, "id = ?", target.ID).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.AddressCiphertext, "1001234567890") || !strings.HasPrefix(row.AddressCiphertext, "aesgcm.v1:") {
		t.Fatalf("ciphertext=%q", row.AddressCiphertext)
	}
	if row.AddressHash != persistence.Hash("-1001234567890") {
		t.Fatal("the hash must be of the plaintext address, or re-registration cannot be recognised")
	}
	// The plaintext is available to the dispatcher and to nobody else.
	f.mu.Lock()
	f.specs = nil
	f.mu.Unlock()
	f.sender.failWith(errors.New("stop here"))
	if _, err := f.app.Publish(context.Background(), f.user.ID, Event{Topic: TopicProposalPending, Title: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.DispatchDue(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if len(f.sender.deliveries()) != 1 || f.sender.deliveries()[0].Address != "-1001234567890" {
		t.Fatalf("sent=%+v", f.sender.deliveries())
	}
}

func TestTargetRejectsAnAddressTheProviderCannotUse(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "校验")
	if _, err := f.app.CreateTarget(context.Background(), &f.admin, f.user.ID, TargetCommand{
		ChannelID: view.ID, Address: "https://t.me/joinchat/whatever",
	}); !errors.Is(err, ErrValidation) {
		t.Fatalf("err=%v, want ErrValidation for a pasted invite link", err)
	}
	if _, err := f.app.CreateTarget(context.Background(), &f.admin, "user_does_not_exist", TargetCommand{
		ChannelID: view.ID, Address: "-1001234567890",
	}); !errors.Is(err, ErrValidation) {
		t.Fatalf("err=%v, want ErrValidation for an unknown user", err)
	}
	if _, err := f.app.CreateTarget(context.Background(), &f.admin, f.user.ID, TargetCommand{
		ChannelID: "notify_channel_missing", Address: "-1001234567890",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v, want ErrNotFound", err)
	}
}

func TestTargetReRegistrationRevivesTheSameRow(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "轮换")
	first := f.target(t, view.ID, "-1001234567890", "旧手机")
	// A provider retired the address: the dispatcher marked it invalid.
	f.sender.failWith(notify.ErrInvalidTarget)
	if _, err := f.app.Publish(context.Background(), f.user.ID, Event{Topic: TopicProposalPending, Title: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.DispatchDue(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	var retired persistence.NotificationTarget
	if err := f.store.DB.First(&retired, "id = ?", first.ID).Error; err != nil {
		t.Fatal(err)
	}
	if retired.Status != "invalid" || retired.FailureCount == 0 {
		t.Fatalf("target=%+v, want it retired by the provider's verdict", retired)
	}

	// The same address is registered again — a reinstall, a re-join. It must revive the
	// existing row rather than fail on the unique index, and keep its delivery history.
	f.sender.failWith(nil)
	second := f.target(t, view.ID, "-1001234567890", "新手机")
	if second.ID != first.ID {
		t.Fatalf("ids %q and %q differ; a re-registration is the same device", second.ID, first.ID)
	}
	if second.Status != "active" || second.FailureCount != 0 || second.Label != "新手机" {
		t.Fatalf("target=%+v", second)
	}
	var history int64
	if err := f.store.DB.Model(&persistence.NotificationMessage{}).Where("target_id = ?", first.ID).Count(&history).Error; err != nil {
		t.Fatal(err)
	}
	if history == 0 {
		t.Fatal("the delivery history was detached from the revived target")
	}
}

func TestPublishQueuesOneMessagePerActiveTarget(t *testing.T) {
	f := newNotificationFixture(t)
	first := f.channel(t, "主通道")
	f.target(t, first.ID, "-1001234567890", "研究群")
	second := f.channel(t, "备用通道")
	f.target(t, second.ID, "@fasttask_channel", "频道")
	// A disabled target and a disabled channel are both unreachable, and neither may queue.
	third := f.target(t, first.ID, "-1009999999999", "停用的手机")
	if _, err := f.app.UpdateTarget(context.Background(), "", &f.admin, third.ID, third.Revision, UpdateTargetCommand{Status: ptr("disabled")}); err != nil {
		t.Fatal(err)
	}
	disabled := f.channel(t, "已停用")
	f.target(t, disabled.ID, "-1008888888888", "停用通道")
	if _, err := f.app.UpdateChannel(context.Background(), f.admin, disabled.ID, disabled.Revision, UpdateChannelCommand{Status: ptr("disabled")}); err != nil {
		t.Fatal(err)
	}

	queued, err := f.app.Publish(context.Background(), f.user.ID, Event{
		Topic: TopicProposalPending, Title: "有待确认的任务树变更", Body: "拆解《论文初稿》",
		Data: map[string]string{"proposal_id": "proposal_1"}, CollapseID: "proposal-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if queued != 2 {
		t.Fatalf("queued=%d, want the two active targets", queued)
	}
	rows := f.messages(t)
	if len(rows) != 2 {
		t.Fatalf("rows=%d", len(rows))
	}
	for _, row := range rows {
		if row.Status != persistence.NotificationQueued || row.Attempts != 0 || row.MaxAttempts != defaultMaxAttempts {
			t.Fatalf("row=%+v", row)
		}
		if !row.RunAfter.Equal(row.CreatedAt) {
			t.Fatalf("run_after=%v created_at=%v; a new message is due immediately", row.RunAfter, row.CreatedAt)
		}
		if !strings.Contains(row.PayloadJSON, `"proposal_id":"proposal_1"`) || !strings.Contains(row.PayloadJSON, `"collapse_id":"proposal-1"`) {
			t.Fatalf("payload=%s", row.PayloadJSON)
		}
	}
}

func TestPublishIsANoOpWithoutTargetsAndWithoutATopic(t *testing.T) {
	f := newNotificationFixture(t)
	queued, err := f.app.Publish(context.Background(), f.user.ID, Event{Topic: TopicProposalPending, Title: "x"})
	if err != nil || queued != 0 {
		t.Fatalf("queued=%d err=%v; a user with nothing configured is the normal case", queued, err)
	}
	view := f.channel(t, "通道")
	f.target(t, view.ID, "-1001234567890", "研究群")
	if _, err := f.app.Publish(context.Background(), f.user.ID, Event{Title: "没有主题"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("err=%v, want ErrValidation", err)
	}
	// The master switch off means nothing is queued, and the configuration survives.
	f.app.NotificationService.WithSettings(NotificationSettings{Enabled: false})
	if queued, err := f.app.Publish(context.Background(), f.user.ID, Event{Topic: TopicProposalPending, Title: "x"}); err != nil || queued != 0 {
		t.Fatalf("queued=%d err=%v, want a silent no-op", queued, err)
	}
	report, err := f.app.DispatchDue(context.Background(), 0)
	if err != nil || report.Claimed != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	f.app.NotificationService.WithSettings(DefaultNotificationSettings())
}

func TestDispatchSendsAndRecordsTheReceipt(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "投递")
	target := f.target(t, view.ID, "-1001234567890", "研究群")
	f.sender.receipt = notify.Receipt{ProviderID: "telegram-4217"}
	if _, err := f.app.Publish(context.Background(), f.user.ID, Event{
		Topic: TopicProposalPending, Title: "有待确认的任务树变更", Body: "拆解《论文初稿》",
	}); err != nil {
		t.Fatal(err)
	}

	report, err := f.app.DispatchDue(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Claimed != 1 || report.Sent != 1 || report.Failed != 0 || report.Retried != 0 {
		t.Fatalf("report=%+v", report)
	}
	rows := f.messages(t)
	if len(rows) != 1 || rows[0].Status != persistence.NotificationSent {
		t.Fatalf("rows=%+v", rows)
	}
	if rows[0].ProviderMessageID != "telegram-4217" || rows[0].Attempts != 1 || rows[0].SentAt == nil {
		t.Fatalf("row=%+v", rows[0])
	}
	if rows[0].Title != "有待确认的任务树变更" || rows[0].Body != "拆解《论文初稿》" {
		t.Fatalf("row=%+v", rows[0])
	}
	// The rendered message is what the adapter saw, including the event's metadata.
	if len(f.sender.deliveries()) != 1 {
		t.Fatalf("sent=%+v", f.sender.deliveries())
	}
	if got := f.sender.deliveries()[0].Message; got.Topic != TopicProposalPending || got.Title != "有待确认的任务树变更" || got.Body != "拆解《论文初稿》" {
		t.Fatalf("message=%+v", got)
	}
	var stored persistence.NotificationTarget
	if err := f.store.DB.First(&stored, "id = ?", target.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.LastSentAt == nil || stored.FailureCount != 0 || stored.LastError != "" {
		t.Fatalf("target=%+v", stored)
	}
	// Nothing is due any more, so a second pass does nothing.
	report, err = f.app.DispatchDue(context.Background(), 0)
	if err != nil || report.Claimed != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestDispatchRetriesATemporaryFailureWithBackoff(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "重试")
	target := f.target(t, view.ID, "-1001234567890", "研究群")
	f.sender.failWith(errors.New("telegram: provider answered 502: Bad Gateway"))
	if _, err := f.app.Publish(context.Background(), f.user.ID, Event{Topic: TopicProposalPending, Title: "x"}); err != nil {
		t.Fatal(err)
	}

	report, err := f.app.DispatchDue(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Retried != 1 || report.Failed != 0 || report.Sent != 0 {
		t.Fatalf("report=%+v", report)
	}
	row := f.messages(t)[0]
	if row.Status != persistence.NotificationQueued || row.Attempts != 1 {
		t.Fatalf("row=%+v", row)
	}
	if delay := row.RunAfter.Sub(row.UpdatedAt); delay != notificationBackoffBase {
		t.Fatalf("backoff=%v, want %v", delay, notificationBackoffBase)
	}
	var stored persistence.NotificationTarget
	if err := f.store.DB.First(&stored, "id = ?", target.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.FailureCount != 1 || stored.Status != "active" {
		t.Fatalf("target=%+v; a temporary failure must not retire the address", stored)
	}

	// Not due yet: the dispatcher leaves it alone.
	report, err = f.app.DispatchDue(context.Background(), 0)
	if err != nil || report.Claimed != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	// Once due, the attempts run out and the message fails rather than looping forever.
	f.store.DB.Model(&persistence.NotificationMessage{}).Where("id = ?", row.ID).Update("run_after", persistence.Now())
	for i := 0; i < defaultMaxAttempts; i++ {
		f.store.DB.Model(&persistence.NotificationMessage{}).Where("id = ?", row.ID).Update("run_after", persistence.Now())
		if _, err := f.app.DispatchDue(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
	}
	row = f.messages(t)[0]
	if row.Status != persistence.NotificationFailed || row.Attempts != defaultMaxAttempts {
		t.Fatalf("row=%+v, want it failed after %d attempts", row, defaultMaxAttempts)
	}
	if !strings.Contains(row.LastError, "502") {
		t.Fatalf("the diagnosis was lost: %q", row.LastError)
	}
}

func TestDispatchRetiresAnAddressTheProviderRejects(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "失效地址")
	target := f.target(t, view.ID, "-1001234567890", "研究群")
	f.sender.failWith(fmt.Errorf("telegram refused the chat: %w", notify.ErrInvalidTarget))
	if _, err := f.app.Publish(context.Background(), f.user.ID, Event{Topic: TopicProposalPending, Title: "x"}); err != nil {
		t.Fatal(err)
	}
	report, err := f.app.DispatchDue(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Retired != 1 || report.Failed != 1 || report.Retried != 0 {
		t.Fatalf("report=%+v", report)
	}
	row := f.messages(t)[0]
	if row.Status != persistence.NotificationFailed {
		t.Fatalf("row=%+v; a dead address must not be retried", row)
	}
	var stored persistence.NotificationTarget
	if err := f.store.DB.First(&stored, "id = ?", target.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != "invalid" || stored.LastError == "" {
		t.Fatalf("target=%+v", stored)
	}
}

func TestDispatchKeepsTheTargetWhenTheChannelIsAtFault(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "凭据失效")
	target := f.target(t, view.ID, "-1001234567890", "研究群")
	f.sender.failWith(fmt.Errorf("telegram refused the bot token: %w", notify.ErrUndeliverable))
	if _, err := f.app.Publish(context.Background(), f.user.ID, Event{Topic: TopicProposalPending, Title: "x"}); err != nil {
		t.Fatal(err)
	}
	report, err := f.app.DispatchDue(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Failed != 1 || report.Retried != 0 || report.Retired != 0 {
		t.Fatalf("report=%+v", report)
	}
	var stored persistence.NotificationTarget
	if err := f.store.DB.First(&stored, "id = ?", target.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != "active" {
		t.Fatalf("target=%+v; fixing the channel must find the subscriptions intact", stored)
	}
}

func TestDispatchSkipsADisabledChannel(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "先停用")
	f.target(t, view.ID, "-1001234567890", "研究群")
	if _, err := f.app.Publish(context.Background(), f.user.ID, Event{Topic: TopicProposalPending, Title: "x"}); err != nil {
		t.Fatal(err)
	}
	// The channel is disabled after the message was queued: the queue must notice at
	// delivery time, not send anyway.
	if _, err := f.app.UpdateChannel(context.Background(), f.admin, view.ID, view.Revision, UpdateChannelCommand{Status: ptr("disabled")}); err != nil {
		t.Fatal(err)
	}
	report, err := f.app.DispatchDue(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Claimed != 1 || report.Failed != 1 {
		t.Fatalf("report=%+v", report)
	}
	if len(f.sender.deliveries()) != 0 {
		t.Fatalf("a disabled channel was delivered through: %+v", f.sender.deliveries())
	}
	row := f.messages(t)[0]
	if row.Status != persistence.NotificationFailed || !strings.Contains(row.LastError, "disabled") {
		t.Fatalf("row=%+v", row)
	}
}

func TestDispatchHandsARowToExactlyOneDispatcher(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "并发")
	f.target(t, view.ID, "-1001234567890", "研究群")
	if _, err := f.app.Publish(context.Background(), f.user.ID, Event{Topic: TopicProposalPending, Title: "x"}); err != nil {
		t.Fatal(err)
	}
	// The worker and the scheduler can both drain the queue (doc/notification.md §5). The
	// claim is a conditional update, so one of them wins the row and the other finds
	// nothing due — which is what makes the fallback safe rather than a double-send.
	reports := make([]DispatchReport, 2)
	var wg sync.WaitGroup
	for i := range reports {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			report, err := f.app.DispatchDue(context.Background(), 0)
			if err != nil {
				t.Error(err)
			}
			reports[i] = report
		}(i)
	}
	wg.Wait()
	claimed := reports[0].Claimed + reports[1].Claimed
	if claimed != 1 {
		t.Fatalf("claimed=%d across two dispatchers, want exactly 1", claimed)
	}
	if got := len(f.sender.deliveries()); got != 1 {
		t.Fatalf("sent=%d, want one delivery", got)
	}
}

func TestDispatchDoesNotReclaimAFreshClaim(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "租约")
	f.target(t, view.ID, "-1001234567890", "研究群")
	if _, err := f.app.Publish(context.Background(), f.user.ID, Event{Topic: TopicProposalPending, Title: "x"}); err != nil {
		t.Fatal(err)
	}
	// Simulate a dispatcher that died mid-send: the row is "sending" with a lease that has
	// not expired. Nobody may take it, or a slow provider would be delivered to twice.
	row := f.messages(t)[0]
	if err := f.store.DB.Model(&persistence.NotificationMessage{}).Where("id = ?", row.ID).Updates(map[string]any{
		"status": persistence.NotificationSending, "attempts": 1, "run_after": persistence.Now().Add(sendLease),
	}).Error; err != nil {
		t.Fatal(err)
	}
	report, err := f.app.DispatchDue(context.Background(), 0)
	if err != nil || report.Claimed != 0 {
		t.Fatalf("report=%+v err=%v, want the live claim left alone", report, err)
	}
	// Once the lease expires the row is due again, and the attempt counter continues.
	if err := f.store.DB.Model(&persistence.NotificationMessage{}).Where("id = ?", row.ID).
		Update("run_after", persistence.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	report, err = f.app.DispatchDue(context.Background(), 0)
	if err != nil || report.Claimed != 1 {
		t.Fatalf("report=%+v err=%v, want the abandoned claim recovered", report, err)
	}
	if got := f.messages(t)[0].Attempts; got != 2 {
		t.Fatalf("attempts=%d, want the recovered claim to continue counting", got)
	}
}

func TestVerifyChannelRecordsTheOutcome(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "验证")

	check, err := f.app.VerifyChannel(context.Background(), f.admin, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !check.Verified || check.Provider != notify.KindTelegram || check.Channel != "验证" {
		t.Fatalf("check=%+v", check)
	}
	var row persistence.NotificationChannel
	if err := f.store.DB.First(&row, "id = ?", view.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.LastCheckStatus != "ok" || row.LastCheckAt == nil || row.LastError != "" {
		t.Fatalf("channel=%+v", row)
	}
	if row.Revision != view.Revision {
		t.Fatalf("revision=%d; a credential check changes no configuration, so a client's ETag must stay valid", row.Revision)
	}

	f.sender.verifyErr = errors.New("telegram: provider answered 401: Unauthorized")
	check, err = f.app.VerifyChannel(context.Background(), f.admin, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if check.Verified || !strings.Contains(check.Detail, "401") {
		t.Fatalf("check=%+v", check)
	}
	if err := f.store.DB.First(&row, "id = ?", view.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.LastCheckStatus != "failed" || !strings.Contains(row.LastError, "401") {
		t.Fatalf("channel=%+v", row)
	}
	if _, err := f.app.VerifyChannel(context.Background(), f.admin, "notify_channel_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v, want ErrNotFound", err)
	}
}

func TestSendTestDeliversWithoutStoringATarget(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "测试发送")

	result, err := f.app.SendTest(context.Background(), f.admin, view.ID, "-1001234567890")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Delivered || result.ProviderMessageID != "provider-1" {
		t.Fatalf("result=%+v", result)
	}
	var targets, messages int64
	if err := f.store.DB.Model(&persistence.NotificationTarget{}).Count(&targets).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB.Model(&persistence.NotificationMessage{}).Count(&messages).Error; err != nil {
		t.Fatal(err)
	}
	if targets != 0 || messages != 0 {
		t.Fatalf("targets=%d messages=%d; a test send must not create state nobody asked for", targets, messages)
	}

	// An address the provider cannot use is reported as such, so the operator knows
	// re-entering it will not help.
	f.sender.failWith(fmt.Errorf("telegram refused the chat: %w", notify.ErrInvalidTarget))
	result, err = f.app.SendTest(context.Background(), f.admin, view.ID, "-1000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if result.Delivered || !result.InvalidTarget || result.Detail == "" {
		t.Fatalf("result=%+v", result)
	}
	// A malformed address never reaches the provider.
	f.sender.forget()
	f.sender.failWith(nil)
	if _, err := f.app.SendTest(context.Background(), f.admin, view.ID, "not an address"); !errors.Is(err, ErrValidation) {
		t.Fatalf("err=%v, want ErrValidation", err)
	}
	if deliveries := f.sender.deliveries(); len(deliveries) != 0 {
		t.Fatalf("a malformed address reached the provider: %+v", deliveries)
	}
}

func TestTestTargetRecordsTheDeliveryInTheLog(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "接收端测试")
	target := f.target(t, view.ID, "-1001234567890", "研究群")

	result, err := f.app.TestTarget(context.Background(), &f.admin, "", target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Delivered {
		t.Fatalf("result=%+v", result)
	}
	rows := f.messages(t)
	if len(rows) != 1 || rows[0].Topic != TopicNotificationTest || rows[0].Status != persistence.NotificationSent {
		t.Fatalf("rows=%+v; a test send belongs in the log with its outcome", rows)
	}
	if rows[0].MaxAttempts != 1 {
		t.Fatalf("max_attempts=%d; a test must not requeue itself minutes later", rows[0].MaxAttempts)
	}

	f.sender.failWith(errors.New("telegram: provider answered 500: Internal Server Error"))
	result, err = f.app.TestTarget(context.Background(), &f.admin, "", target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Delivered || !strings.Contains(result.Detail, "500") {
		t.Fatalf("result=%+v", result)
	}
	rows = f.messages(t)
	if rows[len(rows)-1].Status != persistence.NotificationFailed {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestBroadcastReachesEveryActiveTarget(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "全站广播")
	f.target(t, view.ID, "-1001234567890", "研究群")
	other := persistence.User{ID: persistence.NewID("user"), Identifier: "second", PasswordHash: "hash", DisplayName: "第二个人", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: persistence.Now(), UpdatedAt: persistence.Now()}
	if err := f.store.DB.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.CreateTarget(context.Background(), &f.admin, other.ID, TargetCommand{ChannelID: view.ID, Address: "-1002222222222"}); err != nil {
		t.Fatal(err)
	}
	// A retired address is not reachable and must not be queued.
	retired, err := f.app.CreateTarget(context.Background(), &f.admin, other.ID, TargetCommand{ChannelID: view.ID, Address: "-1003333333333"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.UpdateTarget(context.Background(), "", &f.admin, retired.ID, retired.Revision, UpdateTargetCommand{Status: ptr("disabled")}); err != nil {
		t.Fatal(err)
	}

	queued, err := f.app.Broadcast(context.Background(), f.admin, BroadcastCommand{Title: "维护通知", Body: "今晚 23:00 停机十分钟"})
	if err != nil {
		t.Fatal(err)
	}
	if queued != 2 {
		t.Fatalf("queued=%d, want the two active targets", queued)
	}
	var count int64
	if err := f.store.DB.Model(&persistence.NotificationMessage{}).Where("topic = ?", TopicNotificationBroadcast).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("messages=%d", count)
	}
	var events []persistence.AdminAuditEvent
	if err := f.store.DB.Where("action = 'notification.broadcast'").Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || !strings.Contains(events[0].DetailJSON, `"queued":2`) {
		t.Fatalf("events=%+v; an announcement must record the reach it had", events)
	}
	if _, err := f.app.Broadcast(context.Background(), f.admin, BroadcastCommand{Body: "没有标题"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("err=%v, want ErrValidation", err)
	}
}

func TestListMessagesNamesThePeopleAndChannels(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "日志")
	f.target(t, view.ID, "-1001234567890", "研究群")
	if _, err := f.app.Publish(context.Background(), f.user.ID, Event{Topic: TopicProposalPending, Title: "有待确认的任务树变更"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.DispatchDue(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	views, err := f.app.ListMessages(context.Background(), MessageFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("views=%+v", views)
	}
	got := views[0]
	if got.UserIdentifier != "member" || got.UserDisplayName != "成员" || got.ChannelName != "日志" || got.Provider != notify.KindTelegram {
		t.Fatalf("view=%+v", got)
	}
	if got.TargetHint == "" || got.Status != persistence.NotificationSent {
		t.Fatalf("view=%+v", got)
	}
	// The filters are what the admin console narrows the log with.
	if views, err := f.app.ListMessages(context.Background(), MessageFilter{Status: "failed"}); err != nil || len(views) != 0 {
		t.Fatalf("views=%+v err=%v", views, err)
	}
	if views, err := f.app.ListMessages(context.Background(), MessageFilter{UserID: f.user.ID, Topic: TopicProposalPending}); err != nil || len(views) != 1 {
		t.Fatalf("views=%+v err=%v", views, err)
	}
	if views, err := f.app.ListMessages(context.Background(), MessageFilter{ChannelID: view.ID}); err != nil || len(views) != 1 {
		t.Fatalf("views=%+v err=%v", views, err)
	}
}

func TestListTargetsScopesAndNames(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "订阅列表")
	f.target(t, view.ID, "-1001234567890", "研究群")

	views, err := f.app.ListTargets(context.Background(), TargetFilter{UserID: f.user.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Label != "研究群" || views[0].UserIdentifier != "member" || views[0].ChannelName != "订阅列表" {
		t.Fatalf("views=%+v", views)
	}
	if views, err := f.app.ListTargets(context.Background(), TargetFilter{ChannelID: view.ID, Status: "active"}); err != nil || len(views) != 1 {
		t.Fatalf("views=%+v err=%v", views, err)
	}
	if views, err := f.app.ListTargets(context.Background(), TargetFilter{Status: "invalid"}); err != nil || len(views) != 0 {
		t.Fatalf("views=%+v err=%v", views, err)
	}
}

func TestTargetSelfServiceCannotReachAnotherUser(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "越权")
	mine := f.target(t, view.ID, "-1001234567890", "研究群")
	other := persistence.User{ID: persistence.NewID("user"), Identifier: "second", PasswordHash: "hash", DisplayName: "第二个人", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: persistence.Now(), UpdatedAt: persistence.Now()}
	if err := f.store.DB.Create(&other).Error; err != nil {
		t.Fatal(err)
	}

	// A self-service call is scoped by its own user id at every step: the update and the
	// delete simply find no row, which is a 404 rather than a 403 that would confirm the
	// id exists.
	if _, err := f.app.UpdateTarget(context.Background(), other.ID, nil, mine.ID, mine.Revision, UpdateTargetCommand{Label: ptr("偷来的")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v, want ErrNotFound", err)
	}
	if err := f.app.DeleteTarget(context.Background(), other.ID, nil, mine.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v, want ErrNotFound", err)
	}
	if _, err := f.app.TestTarget(context.Background(), nil, other.ID, mine.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v, want ErrNotFound", err)
	}
	if views, err := f.app.ListTargets(context.Background(), TargetFilter{UserID: other.ID}); err != nil || len(views) != 0 {
		t.Fatalf("views=%+v err=%v", views, err)
	}
	var row persistence.NotificationTarget
	if err := f.store.DB.First(&row, "id = ?", mine.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.Label != "研究群" {
		t.Fatalf("target=%+v", row)
	}
}

func TestPruneKeepsRecentDeliveriesAndDropsOldOnes(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "保留期")
	target := f.target(t, view.ID, "-1001234567890", "研究群")
	if _, err := f.app.Publish(context.Background(), f.user.ID, Event{Topic: TopicProposalPending, Title: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.DispatchDue(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	// Backdate one finished delivery past the retention window and leave the fresh one.
	old := persistence.Now().Add(-2 * defaultNotificationRetention)
	fresh := f.messages(t)[0]
	stale := newMessage(f.user.ID, target.ID, view.ID, normalizeEvent(Event{Topic: TopicProposalPending, Title: "旧消息"}), old)
	stale.Status, stale.SentAt = persistence.NotificationSent, &old
	if err := f.store.DB.Create(&stale).Error; err != nil {
		t.Fatal(err)
	}

	removed, err := f.app.Prune(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed=%d, want the one delivery past the retention window", removed)
	}
	rows := f.messages(t)
	if len(rows) != 1 || rows[0].ID != fresh.ID {
		t.Fatalf("rows=%+v, want the recent delivery kept", rows)
	}
	// Prune gates itself: a second call inside the hour does no work.
	if removed, err := f.app.Prune(context.Background()); err != nil || removed != 0 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
}

func TestNotifyHooksPublishForAWaitingDecision(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "对话")
	f.target(t, view.ID, "-1001234567890", "研究群")
	now := persistence.Now()
	thread := persistence.AgentThread{ID: persistence.NewID("thread"), UserID: f.user.ID, Title: "对话", Status: persistence.ThreadRegular, CreatedAt: now, UpdatedAt: now}
	if err := f.store.DB.Create(&thread).Error; err != nil {
		t.Fatal(err)
	}
	run := persistence.AgentRun{ID: persistence.NewID("run"), UserID: f.user.ID, ThreadID: thread.ID, Status: persistence.RunAwaitingApproval, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := f.store.DB.Create(&run).Error; err != nil {
		t.Fatal(err)
	}

	notifyProposalPending(context.Background(), f.app, &run, &PendingProposalRef{
		ProposalID: "proposal_1", GoalID: "goal_1", Summary: "把《论文初稿》拆成 12 个任务",
	})
	rows := f.messages(t)
	if len(rows) != 1 || rows[0].Topic != TopicProposalPending {
		t.Fatalf("rows=%+v", rows)
	}
	if rows[0].Title != "有待确认的任务树变更" || !strings.Contains(rows[0].Body, "把《论文初稿》拆成 12 个任务") {
		t.Fatalf("row=%+v", rows[0])
	}
	if !strings.Contains(rows[0].PayloadJSON, `"proposal_id":"proposal_1"`) {
		t.Fatalf("payload=%s", rows[0].PayloadJSON)
	}

	notifyRunFailed(context.Background(), f.app, &run, "PROVIDER_ERROR", "上游模型超时")
	rows = f.messages(t)
	if len(rows) != 2 || rows[1].Topic != TopicRunFailed {
		t.Fatalf("rows=%+v", rows)
	}
	if !strings.Contains(rows[1].Body, "PROVIDER_ERROR") || !strings.Contains(rows[1].Body, "上游模型超时") {
		t.Fatalf("row=%+v", rows[1])
	}
	// A hook with nothing configured is a no-op, not a panic: notifications are a courtesy
	// and the run has already been committed.
	notifyProposalPending(context.Background(), nil, &run, &PendingProposalRef{ProposalID: "p"})
	notifyRunFailed(context.Background(), f.app, nil, "X", "y")
	if got := len(f.messages(t)); got != 2 {
		t.Fatalf("messages=%d, want the nil guards to hold", got)
	}
}

func TestChannelSpecCarriesTheDecryptedCredentialToTheAdapter(t *testing.T) {
	f := newNotificationFixture(t)
	view := f.channel(t, "凭据传递")
	f.target(t, view.ID, "-1001234567890", "研究群")
	f.mu.Lock()
	f.specs = nil
	f.mu.Unlock()
	if _, err := f.app.Publish(context.Background(), f.user.ID, Event{Topic: TopicProposalPending, Title: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.DispatchDue(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.specs) == 0 {
		t.Fatal("no adapter was built")
	}
	spec := f.specs[0]
	if spec.Kind != notify.KindTelegram || spec.Secret != "123456789:bot-token-value" {
		t.Fatalf("spec=%+v", spec)
	}
	if spec.Settings["api_base"] != "https://telegram.example.test" {
		t.Fatalf("settings=%+v", spec.Settings)
	}
	if spec.Client == nil || spec.Timeout <= 0 {
		t.Fatalf("the shared client and timeout must reach the adapter: %+v", spec)
	}
}

func TestNotificationBackoffHasACeiling(t *testing.T) {
	if got := notificationBackoff(0); got != notificationBackoffBase {
		t.Fatalf("backoff(0)=%v", got)
	}
	if got := notificationBackoff(1); got != notificationBackoffBase {
		t.Fatalf("backoff(1)=%v", got)
	}
	if got := notificationBackoff(2); got != 2*notificationBackoffBase {
		t.Fatalf("backoff(2)=%v", got)
	}
	if got := notificationBackoff(40); got != notificationBackoffMax {
		t.Fatalf("backoff(40)=%v, want the ceiling", got)
	}
}

func TestNormalizeEventClampsAndFallsBack(t *testing.T) {
	event := normalizeEvent(Event{Topic: "  proposal.pending  ", Title: "  " + strings.Repeat("长", maxTitleRunes+50), Body: strings.Repeat("b", maxBodyRunes+500)})
	if event.Topic != "proposal.pending" {
		t.Fatalf("topic=%q", event.Topic)
	}
	if got := len([]rune(event.Title)); got != maxTitleRunes {
		t.Fatalf("title runes=%d, want %d", got, maxTitleRunes)
	}
	if got := len([]rune(event.Body)); got != maxBodyRunes {
		t.Fatalf("body runes=%d, want %d", got, maxBodyRunes)
	}
	if !strings.HasSuffix(event.Title, "…") {
		t.Fatalf("a clamped title must show the cut: %q", event.Title[len(event.Title)-3:])
	}
	// A title nobody supplied falls back to the topic, so a banner is never blank.
	if got := normalizeEvent(Event{Topic: "agent.run_failed"}).Title; got != "agent.run_failed" {
		t.Fatalf("title=%q", got)
	}
	if got := normalizeEvent(Event{Topic: "x", Data: map[string]string{"  ": "v", "k": "  "}}).Data; len(got) != 1 || got["k"] != "" {
		t.Fatalf("data=%+v; blank keys and values must not travel", got)
	}
}

func TestAddressHintMasksEveryShapeOfAddress(t *testing.T) {
	for _, testCase := range []struct{ address, want string }{
		{"-1001234567890", "…7890"},
		{"@fasttask_channel", "…nnel"},
		{"short", "••••"},
		{"12345678", "••••"},
	} {
		if got := addressHint(testCase.address); got != testCase.want {
			t.Fatalf("hint(%q)=%q, want %q", testCase.address, got, testCase.want)
		}
	}
}

func ptr[T any](value T) *T { return &value }
