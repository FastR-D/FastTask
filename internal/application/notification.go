package application

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FastR-D/FastTask/internal/notify"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

// NotificationService owns the outbound notification channels and their delivery
// (doc/notification.md). It is the application side of the `Notifier` Port that
// doc/tech.md §2.3 reserved: it decides *who* is told *what*, and internal/notify
// decides how one provider's wire format is spoken.
//
// Two things are deliberately kept apart here:
//
//   - The channel configuration an administrator manages. Its credential is stored
//     encrypted and never returned by any API (doc/notification.md §7).
//   - The delivery queue. Every message is a row that is claimed, sent outside any
//     transaction, and then recorded — so a crash mid-send leaves a row that becomes
//     due again rather than a delivery nobody can account for (§5).
//
// Targets (a user's address on a channel) live in NotificationTargetService, which
// holds this service to reuse its adapters and its delivery recorder.

// NotificationSettings is the operator's tuning for delivery. The composition root
// passes the whole struct from config; a service built without it (tests, the harden
// command) keeps these defaults.
type NotificationSettings struct {
	// Enabled is the master switch. Off means Publish is a no-op and the dispatcher
	// claims nothing — the configuration stays readable, so an operator can turn
	// delivery back on without re-entering a credential.
	Enabled bool
	// Timeout bounds one provider call.
	Timeout time.Duration
	// Batch is how many messages one dispatcher pass claims.
	Batch int
	// Retention is how long finished deliveries stay in the log.
	Retention time.Duration
}

// DefaultNotificationSettings is what a service uses when the composition root
// supplies none.
func DefaultNotificationSettings() NotificationSettings {
	return NotificationSettings{
		Enabled:   true,
		Timeout:   notify.DefaultTimeout,
		Batch:     defaultNotificationBatch,
		Retention: defaultNotificationRetention,
	}
}

const (
	defaultNotificationBatch     = 20
	maxNotificationBatch         = 100
	defaultNotificationRetention = 14 * 24 * time.Hour
	// defaultMaxAttempts is how many times one message is attempted. Three is enough
	// for a provider having a bad minute and short enough that a wrong configuration
	// does not pile up retries for an hour.
	defaultMaxAttempts = 3

	// sendLease is how long a claimed message stays "sending" before another
	// dispatcher may take it. It is much longer than a provider call, so a slow
	// network is not mistaken for a dead dispatcher, and short enough that a process
	// killed mid-send resumes within minutes.
	sendLease = 5 * time.Minute

	// Retry backoff: exponential with a ceiling (doc/tech.md §11.3). A notification is
	// not urgent enough to hammer a provider that is rate limiting us.
	notificationBackoffBase = 30 * time.Second
	notificationBackoffMax  = 15 * time.Minute

	// senderCacheLimit bounds the adapter cache. Entries are keyed by channel and
	// revision, so an edited channel simply stops being looked up; the limit is what
	// keeps deleted channels from accumulating.
	senderCacheLimit = 64

	// maxTitleRunes and maxBodyRunes keep a message inside every provider's limits
	// (APNs caps a payload at 4 KiB) and inside a banner that cannot show more.
	maxTitleRunes = 160
	maxBodyRunes  = 1000
	// pruneInterval is how often the delivery log is trimmed.
	pruneInterval = time.Hour
)

// Notification topics. They are the stable names a client can filter on, so they are
// constants rather than strings assembled at each call site.
const (
	TopicNotificationTest      = "notification.test"
	TopicNotificationBroadcast = "notification.broadcast"
	TopicProposalPending       = "proposal.pending"
	TopicRunFailed             = "agent.run_failed"
)

// NotificationService delivers messages through administrator-configured channels.
type NotificationService struct {
	Store     *persistence.Store
	secretKey []byte

	settings NotificationSettings
	// newSender builds an adapter. Tests replace it to deliver without a provider.
	newSender func(notify.Spec) (notify.Sender, error)

	mu        sync.Mutex
	client    *http.Client
	senders   map[string]notify.Sender
	lastPrune time.Time
}

// NewNotificationService builds the service. secretKey may be nil, in which case
// storing a credential fails closed rather than writing it in plaintext.
func NewNotificationService(store *persistence.Store, secretKey []byte) *NotificationService {
	return &NotificationService{
		Store: store, secretKey: secretKey,
		settings:  DefaultNotificationSettings(),
		newSender: notify.NewSender,
		senders:   map[string]notify.Sender{},
	}
}

// WithSettings applies the operator's delivery settings and drops the cached HTTP
// client, which was built for the previous timeout.
func (s *NotificationService) WithSettings(settings NotificationSettings) *NotificationService {
	if settings.Timeout <= 0 {
		settings.Timeout = notify.DefaultTimeout
	}
	if settings.Batch <= 0 {
		settings.Batch = defaultNotificationBatch
	}
	if settings.Batch > maxNotificationBatch {
		settings.Batch = maxNotificationBatch
	}
	if settings.Retention <= 0 {
		settings.Retention = defaultNotificationRetention
	}
	s.mu.Lock()
	s.settings, s.client = settings, nil
	s.mu.Unlock()
	return s
}

// WithSenderFactory replaces the adapter factory. Tests use it to deliver without a
// provider; nothing in production calls it.
func (s *NotificationService) WithSenderFactory(factory func(notify.Spec) (notify.Sender, error)) *NotificationService {
	if factory == nil {
		factory = notify.NewSender
	}
	s.mu.Lock()
	s.newSender, s.senders = factory, map[string]notify.Sender{}
	s.mu.Unlock()
	return s
}

// Event is one thing worth telling a user about.
type Event struct {
	// Topic is the stable event name (TopicProposalPending, ...).
	Topic string
	Title string
	Body  string
	// URL is the deep link the notification opens.
	URL string
	// Data is small string metadata a native client can act on.
	Data map[string]string
	// CollapseID lets a newer message replace an undelivered older one.
	CollapseID string
	// ChannelID restricts the event to one channel. Empty means every active one.
	ChannelID string
}

// BroadcastCommand is an administrator's message to every reachable user.
type BroadcastCommand struct {
	Topic     string
	Title     string
	Body      string
	URL       string
	ChannelID string
}

// ChannelCommand creates a channel.
type ChannelCommand struct {
	Name     string
	Provider string
	Endpoint string
	Settings map[string]string
	Secret   string
	Status   string
}

// UpdateChannelCommand patches a channel. A nil field is left alone; a nil or empty
// Secret keeps the stored credential, which is how the admin UI edits a channel
// without the operator re-typing a key it never saw.
type UpdateChannelCommand struct {
	Name     *string
	Endpoint *string
	Settings *map[string]string
	Secret   *string
	Status   *string
}

// ChannelView is a channel as an API returns it: the settings an operator typed and a
// hint at the credential, never the credential.
type ChannelView struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Provider          string            `json:"provider"`
	Endpoint          string            `json:"endpoint"`
	Settings          map[string]string `json:"settings"`
	SecretHint        string            `json:"secret_hint"`
	SecretSet         bool              `json:"secret_set"`
	Status            string            `json:"status"`
	LastCheckAt       *time.Time        `json:"last_check_at,omitempty"`
	LastCheckStatus   string            `json:"last_check_status"`
	LastError         string            `json:"last_error,omitempty"`
	TargetCount       int               `json:"target_count"`
	ActiveTargetCount int               `json:"active_target_count"`
	Revision          int               `json:"revision"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

// ChannelCheck is the outcome of a credential check.
type ChannelCheck struct {
	Verified  bool      `json:"verified"`
	Provider  string    `json:"provider"`
	Channel   string    `json:"channel"`
	Detail    string    `json:"detail,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// DeliveryResult is what one immediate send produced.
type DeliveryResult struct {
	Delivered         bool   `json:"delivered"`
	Provider          string `json:"provider"`
	Channel           string `json:"channel"`
	ProviderMessageID string `json:"provider_message_id,omitempty"`
	Detail            string `json:"detail,omitempty"`
	// InvalidTarget reports that the provider permanently rejected the address, so the
	// operator knows re-entering it will not help.
	InvalidTarget bool `json:"invalid_target,omitempty"`
}

// DispatchReport summarises one dispatcher pass.
type DispatchReport struct {
	Claimed int `json:"claimed"`
	Sent    int `json:"sent"`
	Retried int `json:"retried"`
	Failed  int `json:"failed"`
	// Retired counts the targets a provider permanently rejected.
	Retired int `json:"retired"`
}

// MessageFilter narrows the delivery log.
type MessageFilter struct {
	UserID    string
	ChannelID string
	TargetID  string
	Status    string
	Topic     string
	Limit     int
}

// MessageView is one delivery-log row with the names a list needs.
type MessageView struct {
	ID                string     `json:"id"`
	UserID            string     `json:"user_id"`
	UserIdentifier    string     `json:"user_identifier,omitempty"`
	UserDisplayName   string     `json:"user_display_name,omitempty"`
	ChannelID         string     `json:"channel_id"`
	ChannelName       string     `json:"channel_name,omitempty"`
	Provider          string     `json:"provider,omitempty"`
	TargetID          string     `json:"target_id"`
	TargetHint        string     `json:"target_hint,omitempty"`
	Topic             string     `json:"topic"`
	Title             string     `json:"title"`
	Body              string     `json:"body"`
	URL               string     `json:"url"`
	PayloadJSON       string     `json:"payload_json"`
	Status            string     `json:"status"`
	Attempts          int        `json:"attempts"`
	MaxAttempts       int        `json:"max_attempts"`
	ProviderMessageID string     `json:"provider_message_id,omitempty"`
	LastError         string     `json:"last_error,omitempty"`
	RunAfter          time.Time  `json:"run_after"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	SentAt            *time.Time `json:"sent_at,omitempty"`
}

// messagePayload is what notification_messages.payload_json carries: the event's
// metadata plus the one delivery hint that has no column of its own.
type messagePayload struct {
	Data       map[string]string `json:"data,omitempty"`
	CollapseID string            `json:"collapse_id,omitempty"`
}

// ListChannels returns every channel with how many users it can reach.
func (s *NotificationService) ListChannels(ctx context.Context) ([]ChannelView, error) {
	var channels []persistence.NotificationChannel
	if err := s.Store.DB.WithContext(ctx).Order("provider, name").Find(&channels).Error; err != nil {
		return nil, err
	}
	type targetCounts struct {
		ChannelID string
		Total     int
		Active    int
	}
	var rows []targetCounts
	if err := s.Store.DB.WithContext(ctx).Model(&persistence.NotificationTarget{}).
		Select("channel_id, COUNT(*) AS total, SUM(CASE WHEN status = 'active' THEN 1 ELSE 0 END) AS active").
		Group("channel_id").Scan(&rows).Error; err != nil {
		return nil, err
	}
	counts := make(map[string]targetCounts, len(rows))
	for _, row := range rows {
		counts[row.ChannelID] = row
	}
	views := make([]ChannelView, 0, len(channels))
	for _, channel := range channels {
		views = append(views, channelView(channel, counts[channel.ID].Total, counts[channel.ID].Active))
	}
	return views, nil
}

// CreateChannel stores a new channel. The provider's own adapter validates the
// configuration first, so a channel that could never send is refused when an
// administrator types it rather than at the first delivery.
func (s *NotificationService) CreateChannel(ctx context.Context, actor persistence.User, command ChannelCommand) (*ChannelView, error) {
	command.Name = strings.TrimSpace(command.Name)
	command.Provider = strings.ToLower(strings.TrimSpace(command.Provider))
	command.Endpoint = strings.TrimSpace(command.Endpoint)
	if command.Status == "" {
		command.Status = "active"
	}
	if command.Status != "active" && command.Status != "disabled" {
		return nil, ErrValidation
	}
	if command.Name == "" || len(command.Name) > 120 {
		return nil, ErrValidation
	}
	if _, err := s.newSender(s.specFor(command.Provider, command.Endpoint, command.Settings, command.Secret, nil)); err != nil {
		return nil, validationError(err)
	}
	ciphertext, err := encryptWithKey(s.secretKey, strings.TrimSpace(command.Secret))
	if err != nil {
		return nil, err
	}
	now := persistence.Now()
	channel := &persistence.NotificationChannel{
		ID: persistence.NewID("notify_channel"), Name: command.Name, Provider: command.Provider,
		Endpoint: strings.TrimRight(command.Endpoint, "/"), SettingsJSON: encodeSettings(command.Settings),
		SecretCiphertext: ciphertext, SecretHint: secretHint(command.Secret), Status: command.Status,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	err = s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Create(channel).Error; err != nil {
			return uniqueConflict(err)
		}
		return auditAdmin(tx, actor, "", "notification_channel.created", map[string]any{
			"channel_id": channel.ID, "provider": channel.Provider, "status": channel.Status,
		})
	})
	if err != nil {
		return nil, err
	}
	view := channelViewFor(ctx, s, *channel)
	return &view, nil
}

// UpdateChannel patches a channel. The revision must match, and the effective result is
// validated as a whole: a channel whose new settings no longer fit its stored
// credential is refused here, because it would otherwise fail at delivery time.
func (s *NotificationService) UpdateChannel(ctx context.Context, actor persistence.User, id string, expected int, command UpdateChannelCommand) (*ChannelView, error) {
	var channel persistence.NotificationChannel
	err := s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.First(&channel, "id = ?", id).Error; err != nil {
			return notFound(err)
		}
		if channel.Revision != expected {
			return ErrRevision
		}
		changes := map[string]any{"revision": expected + 1, "updated_at": persistence.Now()}
		settings := decodeSettings(channel.SettingsJSON)
		secret, err := decryptWithKey(s.secretKey, channel.SecretCiphertext)
		if err != nil {
			return err
		}
		endpoint, name, status := channel.Endpoint, channel.Name, channel.Status
		replacedSecret := false
		if command.Name != nil {
			name = strings.TrimSpace(*command.Name)
			if name == "" || len(name) > 120 {
				return ErrValidation
			}
			changes["name"] = name
		}
		if command.Endpoint != nil {
			endpoint = strings.TrimRight(strings.TrimSpace(*command.Endpoint), "/")
			changes["endpoint"] = endpoint
		}
		if command.Settings != nil {
			settings = normalizeSettings(*command.Settings)
			changes["settings_json"] = encodeSettings(settings)
		}
		if command.Secret != nil && strings.TrimSpace(*command.Secret) != "" {
			secret = strings.TrimSpace(*command.Secret)
			ciphertext, err := encryptWithKey(s.secretKey, secret)
			if err != nil {
				return err
			}
			changes["secret_ciphertext"], changes["secret_hint"] = ciphertext, secretHint(secret)
			replacedSecret = true
		}
		if command.Status != nil {
			status = *command.Status
			if status != "active" && status != "disabled" {
				return ErrValidation
			}
			changes["status"] = status
		}
		if _, err := s.newSender(s.specFor(channel.Provider, endpoint, settings, secret, nil)); err != nil {
			return validationError(err)
		}
		result := tx.Model(&persistence.NotificationChannel{}).Where("id = ? AND revision = ?", id, expected).Updates(changes)
		if result.Error != nil {
			return uniqueConflict(result.Error)
		}
		if result.RowsAffected != 1 {
			return ErrRevision
		}
		detail := map[string]any{"channel_id": id, "status": status}
		if command.Name != nil {
			detail["name"] = name
		}
		if command.Endpoint != nil {
			detail["endpoint"] = endpoint
		}
		if command.Settings != nil {
			detail["settings"] = settings
		}
		// The credential is never audited, only the fact that it changed.
		detail["secret_replaced"] = replacedSecret
		return auditAdmin(tx, actor, "", "notification_channel.updated", detail)
	})
	if err != nil {
		return nil, err
	}
	if err := s.Store.DB.WithContext(ctx).First(&channel, "id = ?", id).Error; err != nil {
		return nil, err
	}
	view := channelViewFor(ctx, s, channel)
	return &view, nil
}

// DeleteChannel removes a channel. Its targets and their delivery history go with it
// (migration 000010's CASCADE): a deleted channel is a revoked credential, and a target
// whose credential is gone can neither be delivered to nor audited.
func (s *NotificationService) DeleteChannel(ctx context.Context, actor persistence.User, id string) error {
	return s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var channel persistence.NotificationChannel
		if err := tx.First(&channel, "id = ?", id).Error; err != nil {
			return notFound(err)
		}
		if err := tx.Delete(&channel).Error; err != nil {
			return err
		}
		return auditAdmin(tx, actor, "", "notification_channel.deleted", map[string]any{
			"channel_id": id, "provider": channel.Provider, "name": channel.Name,
		})
	})
}

// VerifyChannel checks a channel's credentials against its provider and records the
// outcome. It delivers nothing to any user: each adapter's Verify is defined as a
// credential check that cannot reach a real device (doc/notification.md §6).
func (s *NotificationService) VerifyChannel(ctx context.Context, actor persistence.User, id string) (ChannelCheck, error) {
	var channel persistence.NotificationChannel
	if err := s.Store.DB.WithContext(ctx).First(&channel, "id = ?", id).Error; err != nil {
		return ChannelCheck{}, notFound(err)
	}
	check := ChannelCheck{Provider: channel.Provider, Channel: channel.Name, CheckedAt: persistence.Now(), Verified: true}
	sender, err := s.senderFor(channel)
	if err == nil {
		err = sender.Verify(ctx)
	}
	if err != nil {
		check.Verified, check.Detail = false, truncateError(err)
	}
	status := "ok"
	if !check.Verified {
		status = "failed"
	}
	// The diagnostic columns are written without touching revision: nothing about the
	// configuration changed, so a client's ETag stays valid.
	if err := s.Store.DB.WithContext(ctx).Model(&persistence.NotificationChannel{}).Where("id = ?", id).
		Updates(map[string]any{"last_check_at": check.CheckedAt, "last_check_status": status, "last_error": check.Detail}).Error; err != nil {
		return ChannelCheck{}, err
	}
	if err := auditAdmin(s.Store.DB.WithContext(ctx), actor, "", "notification_channel.verified", map[string]any{
		"channel_id": id, "verified": check.Verified,
	}); err != nil {
		return ChannelCheck{}, err
	}
	return check, nil
}

// SendTest delivers one message to an address without storing it as a target. It is how
// an administrator proves a channel works before anybody subscribes to it, and how they
// reach an address that belongs to no user yet.
func (s *NotificationService) SendTest(ctx context.Context, actor persistence.User, channelID, address string) (DeliveryResult, error) {
	var channel persistence.NotificationChannel
	if err := s.Store.DB.WithContext(ctx).First(&channel, "id = ?", channelID).Error; err != nil {
		return DeliveryResult{}, notFound(err)
	}
	result := DeliveryResult{Provider: channel.Provider, Channel: channel.Name}
	address = strings.TrimSpace(address)
	if err := notify.ValidateAddress(channel.Provider, address); err != nil {
		return result, validationError(err)
	}
	sender, err := s.senderFor(channel)
	if err != nil {
		result.Detail = truncateError(err)
		return result, nil
	}
	receipt, err := sender.Send(ctx, address, notify.Message{
		Topic: TopicNotificationTest, Title: "FastTask 通知测试",
		Body: "这条消息来自后台的测试发送。收到即表示该通道可用。",
	})
	if err != nil {
		result.Detail, result.InvalidTarget = truncateError(err), notify.IsInvalidTarget(err)
	} else {
		result.Delivered, result.ProviderMessageID = true, receipt.ProviderID
	}
	// The address is not audited: for FCM and APNs it is a device credential.
	if err := auditAdmin(s.Store.DB.WithContext(ctx), actor, "", "notification_channel.test", map[string]any{
		"channel_id": channelID, "delivered": result.Delivered,
	}); err != nil {
		return result, err
	}
	return result, nil
}

// Publish queues event for every active target of one user. It returns how many
// deliveries were queued; zero is the normal answer for a user who configured nothing,
// and never an error.
//
// Publish is called after the business transaction that produced the event has
// committed. A notification is a courtesy, not part of an invariant: losing one because
// the process died between the commit and the queue is acceptable, and failing the
// user's action over it would not be.
func (s *NotificationService) Publish(ctx context.Context, userID string, event Event) (int, error) {
	if !s.settings.Enabled || strings.TrimSpace(userID) == "" {
		return 0, nil
	}
	event = normalizeEvent(event)
	if event.Topic == "" {
		return 0, ErrValidation
	}
	targets, err := s.activeTargets(ctx, userID, event.ChannelID)
	if err != nil {
		return 0, err
	}
	if len(targets) == 0 {
		return 0, nil
	}
	now := persistence.Now()
	rows := make([]persistence.NotificationMessage, 0, len(targets))
	for _, target := range targets {
		rows = append(rows, newMessage(target.UserID, target.ID, target.ChannelID, event, now))
	}
	if err := s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		return tx.CreateInBatches(&rows, 50).Error
	}); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// Broadcast queues one message for every active target, optionally on one channel. It
// is the administrator's maintenance announcement, and it audits the reach it had.
func (s *NotificationService) Broadcast(ctx context.Context, actor persistence.User, command BroadcastCommand) (int, error) {
	event := normalizeEvent(Event{
		Topic: command.Topic, Title: command.Title, Body: command.Body, URL: command.URL, ChannelID: strings.TrimSpace(command.ChannelID),
	})
	if event.Topic == "" {
		event.Topic = TopicNotificationBroadcast
	}
	if event.Title == "" {
		return 0, ErrValidation
	}
	var targets []targetRef
	query := s.Store.DB.WithContext(ctx).Table("notification_targets AS t").
		Select("t.id AS id, t.user_id AS user_id, t.channel_id AS channel_id").
		Joins("JOIN notification_channels c ON c.id = t.channel_id").
		Where("t.status = 'active' AND c.status = 'active'")
	if event.ChannelID != "" {
		query = query.Where("t.channel_id = ?", event.ChannelID)
	}
	if err := query.Limit(maxBroadcastTargets).Scan(&targets).Error; err != nil {
		return 0, err
	}
	now := persistence.Now()
	queued := 0
	for start := 0; start < len(targets); start += 50 {
		end := min(start+50, len(targets))
		batch := make([]persistence.NotificationMessage, 0, end-start)
		for _, target := range targets[start:end] {
			batch = append(batch, newMessage(target.UserID, target.ID, target.ChannelID, event, now))
		}
		if err := s.Store.Transaction(ctx, func(tx *gorm.DB) error {
			return tx.CreateInBatches(&batch, 50).Error
		}); err != nil {
			return queued, err
		}
		queued += len(batch)
	}
	if err := auditAdmin(s.Store.DB.WithContext(ctx), actor, "", "notification.broadcast", map[string]any{
		"topic": event.Topic, "channel_id": event.ChannelID, "queued": queued,
	}); err != nil {
		return queued, err
	}
	return queued, nil
}

// maxBroadcastTargets bounds one announcement. A deployment with more reachable users
// than this is not a single-binary SQLite deployment, and silently queueing an
// unbounded batch is worse than an operator seeing a number that stopped growing.
const maxBroadcastTargets = 5000

// DispatchDue claims and delivers the messages whose time has come. The scheduler's
// sweeper calls it, and the worker calls it as a fallback for a deployment without a
// scheduler; both may run at once, because a claim is a conditional update and only one
// of them wins a row.
func (s *NotificationService) DispatchDue(ctx context.Context, limit int) (DispatchReport, error) {
	if !s.settings.Enabled {
		return DispatchReport{}, nil
	}
	if limit <= 0 {
		limit = s.settings.Batch
	}
	if limit > maxNotificationBatch {
		limit = maxNotificationBatch
	}
	report := DispatchReport{}
	for i := 0; i < limit; i++ {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		message, err := claimNextMessage(ctx, s)
		if err != nil {
			return report, err
		}
		if message == nil {
			return report, nil
		}
		report.Claimed++
		outcome := sendClaimed(ctx, s, *message)
		switch {
		case outcome.Sent:
			report.Sent++
		case outcome.Retried:
			report.Retried++
		default:
			report.Failed++
		}
		if outcome.RetireTarget {
			report.Retired++
		}
	}
	return report, nil
}

// ListMessages returns the delivery log, newest first, with the names a list needs.
func (s *NotificationService) ListMessages(ctx context.Context, filter MessageFilter) ([]MessageView, error) {
	limit := filter.Limit
	if limit < 1 || limit > 200 {
		limit = 50
	}
	query := s.Store.DB.WithContext(ctx).Model(&persistence.NotificationMessage{})
	if filter.UserID != "" {
		query = query.Where("user_id = ?", filter.UserID)
	}
	if filter.ChannelID != "" {
		query = query.Where("channel_id = ?", filter.ChannelID)
	}
	if filter.TargetID != "" {
		query = query.Where("target_id = ?", filter.TargetID)
	}
	if filter.Status != "" {
		query = query.Where("status = ?", filter.Status)
	}
	if filter.Topic != "" {
		query = query.Where("topic = ?", filter.Topic)
	}
	var messages []persistence.NotificationMessage
	if err := query.Order("created_at DESC").Limit(limit).Find(&messages).Error; err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return []MessageView{}, nil
	}
	userIDs := make([]string, 0, len(messages))
	channelIDs := make([]string, 0, len(messages))
	targetIDs := make([]string, 0, len(messages))
	for _, message := range messages {
		userIDs = append(userIDs, message.UserID)
		channelIDs = append(channelIDs, message.ChannelID)
		targetIDs = append(targetIDs, message.TargetID)
	}
	users := map[string]persistence.User{}
	var userRecords []persistence.User
	if err := s.Store.DB.WithContext(ctx).Where("id IN ?", userIDs).Find(&userRecords).Error; err != nil {
		return nil, err
	}
	for _, record := range userRecords {
		users[record.ID] = record
	}
	channels := map[string]persistence.NotificationChannel{}
	var channelRecords []persistence.NotificationChannel
	if err := s.Store.DB.WithContext(ctx).Where("id IN ?", channelIDs).Find(&channelRecords).Error; err != nil {
		return nil, err
	}
	for _, record := range channelRecords {
		channels[record.ID] = record
	}
	targets := map[string]persistence.NotificationTarget{}
	var targetRecords []persistence.NotificationTarget
	if err := s.Store.DB.WithContext(ctx).Where("id IN ?", targetIDs).Find(&targetRecords).Error; err != nil {
		return nil, err
	}
	for _, record := range targetRecords {
		targets[record.ID] = record
	}

	views := make([]MessageView, 0, len(messages))
	for _, message := range messages {
		view := MessageView{
			ID: message.ID, UserID: message.UserID, ChannelID: message.ChannelID, TargetID: message.TargetID,
			Topic: message.Topic, Title: message.Title, Body: message.Body, URL: message.URL, PayloadJSON: message.PayloadJSON,
			Status: message.Status, Attempts: message.Attempts, MaxAttempts: message.MaxAttempts,
			ProviderMessageID: message.ProviderMessageID, LastError: message.LastError,
			RunAfter: message.RunAfter, CreatedAt: message.CreatedAt, UpdatedAt: message.UpdatedAt, SentAt: message.SentAt,
		}
		if user, ok := users[message.UserID]; ok {
			view.UserIdentifier, view.UserDisplayName = user.Identifier, user.DisplayName
		}
		if channel, ok := channels[message.ChannelID]; ok {
			view.ChannelName, view.Provider = channel.Name, channel.Provider
		}
		if target, ok := targets[message.TargetID]; ok {
			view.TargetHint = target.AddressHint
		}
		views = append(views, view)
	}
	return views, nil
}

// Prune drops finished deliveries older than the retention window. It runs at most once
// an hour per process: the log is a diagnostic, and scanning it on every sweep would
// cost more than the rows it reclaims.
func (s *NotificationService) Prune(ctx context.Context) (int64, error) {
	if !s.settings.Enabled {
		return 0, nil
	}
	s.mu.Lock()
	last := s.lastPrune
	s.mu.Unlock()
	if !last.IsZero() && time.Since(last) < pruneInterval {
		return 0, nil
	}
	cutoff := persistence.Now().Add(-s.settings.Retention)
	result := s.Store.DB.WithContext(ctx).
		Where("status IN ? AND created_at < ?", []string{persistence.NotificationSent, persistence.NotificationFailed}, cutoff).
		Delete(&persistence.NotificationMessage{})
	if result.Error != nil {
		return 0, result.Error
	}
	s.mu.Lock()
	s.lastPrune = persistence.Now()
	s.mu.Unlock()
	return result.RowsAffected, nil
}

// senderFor builds the adapter for a channel, reusing the one for its current revision.
// The cache is keyed by revision, so an edited channel builds a fresh adapter and the
// stale entry is never looked up again — which is also what keeps a cached APNs provider
// token or FCM access token from outliving the credentials it was signed with.
func (s *NotificationService) senderFor(channel persistence.NotificationChannel) (notify.Sender, error) {
	secret, err := decryptWithKey(s.secretKey, channel.SecretCiphertext)
	if err != nil {
		return nil, err
	}
	spec := s.specFor(channel.Provider, channel.Endpoint, decodeSettings(channel.SettingsJSON), secret, s.httpClient())
	key := channel.ID + "@" + strconv.Itoa(channel.Revision)

	s.mu.Lock()
	sender, cached := s.senders[key]
	factory, overflow := s.newSender, len(s.senders) >= senderCacheLimit
	s.mu.Unlock()
	if cached {
		return sender, nil
	}
	built, err := factory(spec)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if overflow {
		s.senders = map[string]notify.Sender{}
	}
	s.senders[key] = built
	s.mu.Unlock()
	return built, nil
}

// specFor assembles the adapter spec from a channel's stored configuration. client may
// be nil, which is what validation at configuration time passes: it never sends.
func (s *NotificationService) specFor(provider, endpoint string, settings map[string]string, secret string, client *http.Client) notify.Spec {
	s.mu.Lock()
	timeout := s.settings.Timeout
	s.mu.Unlock()
	return notify.Spec{
		Kind: strings.ToLower(strings.TrimSpace(provider)), Endpoint: strings.TrimRight(strings.TrimSpace(endpoint), "/"),
		Settings: normalizeSettings(settings), Secret: secret, Timeout: timeout, Client: client,
	}
}

// httpClient returns the shared client every adapter uses, building it for the current
// timeout. One client per service is what makes doc/tech.md §12's connection reuse true
// instead of aspirational: a client built per send would open a TLS handshake per
// message and, for APNs, a new HTTP/2 connection each time.
func (s *NotificationService) httpClient() *http.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		s.client = notify.NewHTTPClient(s.settings.Timeout)
	}
	return s.client
}

// targetRef is one row of the (target, channel) join the publisher and the dispatcher
// both need.
type targetRef struct {
	ID        string
	UserID    string
	ChannelID string
	Provider  string
}

// activeTargets lists the targets a message can be queued for: this user's, on a
// channel that is itself active.
func (s *NotificationService) activeTargets(ctx context.Context, userID, channelID string) ([]targetRef, error) {
	var targets []targetRef
	query := s.Store.DB.WithContext(ctx).Table("notification_targets AS t").
		Select("t.id AS id, t.user_id AS user_id, t.channel_id AS channel_id, c.provider AS provider").
		Joins("JOIN notification_channels c ON c.id = t.channel_id").
		Where("t.user_id = ? AND t.status = 'active' AND c.status = 'active'", userID)
	if channelID != "" {
		query = query.Where("t.channel_id = ?", channelID)
	}
	return targets, query.Limit(maxTargetsPerUser).Scan(&targets).Error
}

// maxTargetsPerUser bounds one user's fan-out. Nobody needs forty phones, and the bound
// is what keeps a data-entry mistake from turning one event into a spam run.
const maxTargetsPerUser = 40

// claimNextMessage takes the oldest due message for this dispatcher. The claim is a
// conditional update, so a second dispatcher loses the row instead of double-sending it,
// and run_after becomes the claim's lease expiry so a dispatcher that dies mid-send
// leaves a row that is due again rather than one stuck forever.
func claimNextMessage(ctx context.Context, s *NotificationService) (*persistence.NotificationMessage, error) {
	var claimed *persistence.NotificationMessage
	err := s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		claimed = nil
		now := persistence.Now()
		var message persistence.NotificationMessage
		err := tx.Where("status IN ? AND run_after <= ?",
			[]string{persistence.NotificationQueued, persistence.NotificationSending}, now).
			Order("run_after, created_at").First(&message).Error
		if err != nil {
			if persistence.IsNotFound(err) {
				return nil
			}
			return err
		}
		result := tx.Model(&persistence.NotificationMessage{}).
			Where("id = ? AND status = ? AND attempts = ?", message.ID, message.Status, message.Attempts).
			Updates(map[string]any{
				"status": persistence.NotificationSending, "attempts": message.Attempts + 1,
				"run_after": now.Add(sendLease), "updated_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}
		message.Status, message.Attempts = persistence.NotificationSending, message.Attempts+1
		message.RunAfter, message.UpdatedAt = now.Add(sendLease), now
		claimed = &message
		return nil
	})
	return claimed, err
}

// deliveryOutcome is what one attempt produced, in the terms the dispatcher counts and
// the recorder writes.
type deliveryOutcome struct {
	Sent bool
	// Retried reports a failure worth another attempt.
	Retried bool
	// RetireTarget reports that the provider permanently rejected the address.
	RetireTarget bool
	// Detail is the provider message id on success, the diagnosis on failure.
	Detail string
}

// sendClaimed performs the provider call for an already-claimed message and records the
// outcome. The call happens outside any transaction (doc/tech.md §11.2: an external call
// never holds a write lock), and the record is written conditionally on the claim this
// dispatcher holds, so a lease that expired and was taken over is never overwritten by
// the slower writer.
func sendClaimed(ctx context.Context, s *NotificationService, message persistence.NotificationMessage) deliveryOutcome {
	var channel persistence.NotificationChannel
	var target persistence.NotificationTarget
	loadErr := s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.First(&channel, "id = ?", message.ChannelID).Error; err != nil {
			return err
		}
		return tx.First(&target, "id = ?", message.TargetID).Error
	})
	if loadErr != nil {
		// The channel or the target was deleted between the claim and the send: the
		// message cannot be delivered, and retrying cannot change that.
		return finishDelivery(ctx, s, message, deliveryOutcome{Detail: "the channel or target no longer exists"}, nil)
	}
	if channel.Status != "active" {
		return finishDelivery(ctx, s, message, deliveryOutcome{Detail: "the channel is disabled"}, nil)
	}
	if target.Status != "active" {
		return finishDelivery(ctx, s, message, deliveryOutcome{Detail: "the target is " + target.Status}, nil)
	}
	address, err := decryptWithKey(s.secretKey, target.AddressCiphertext)
	if err != nil {
		return finishDelivery(ctx, s, message, deliveryOutcome{Detail: truncateError(err), RetireTarget: true}, &target)
	}
	sender, err := s.senderFor(channel)
	if err != nil {
		return finishDelivery(ctx, s, message, deliveryOutcome{Detail: truncateError(err)}, &target)
	}
	receipt, err := sender.Send(ctx, address, notifyMessageFor(message))
	if err != nil {
		outcome := deliveryOutcome{Detail: truncateError(err)}
		switch {
		case notify.IsInvalidTarget(err):
			// A dead address: retire the target so it stops being retried. Nothing about
			// the channel is wrong, so its other targets keep working.
			outcome.RetireTarget = true
		case notify.IsUndeliverable(err):
			// The channel's own credentials or payload are at fault. Give up on this
			// message but leave the target alone: fixing the channel should find the
			// subscriptions intact.
		default:
			// Retryable: requeue with backoff unless the attempts ran out.
			outcome.Retried = message.Attempts < message.MaxAttempts
		}
		return finishDelivery(ctx, s, message, outcome, &target)
	}
	return finishDelivery(ctx, s, message, deliveryOutcome{Sent: true, Detail: receipt.ProviderID}, &target)
}

// finishDelivery writes one attempt's outcome onto the message and, when the attempt
// reached a provider, onto the target's counters. Both writes are conditional on the
// claim (status sending, this attempt number), so a dispatcher whose lease expired
// cannot overwrite the result of the one that took over.
func finishDelivery(ctx context.Context, s *NotificationService, message persistence.NotificationMessage, outcome deliveryOutcome, target *persistence.NotificationTarget) deliveryOutcome {
	now := persistence.Now()
	changes := map[string]any{"updated_at": now}
	switch {
	case outcome.Sent:
		changes["status"] = persistence.NotificationSent
		changes["sent_at"] = now
		changes["provider_message_id"] = outcome.Detail
		changes["last_error"] = ""
	case outcome.Retried:
		changes["status"] = persistence.NotificationQueued
		changes["run_after"] = now.Add(notificationBackoff(message.Attempts))
		changes["last_error"] = outcome.Detail
	default:
		changes["status"] = persistence.NotificationFailed
		changes["last_error"] = outcome.Detail
	}
	// A write that loses the claim is not an error worth reporting: the delivery already
	// happened, and the winner recorded it. The report counts this dispatcher's view.
	_ = s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		result := tx.Model(&persistence.NotificationMessage{}).
			Where("id = ? AND status = ? AND attempts = ?", message.ID, persistence.NotificationSending, message.Attempts).
			Updates(changes)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 || target == nil {
			return nil
		}
		targetChanges := map[string]any{"updated_at": now, "revision": gorm.Expr("revision + 1")}
		switch {
		case outcome.Sent:
			targetChanges["failure_count"] = 0
			targetChanges["last_error"] = ""
			targetChanges["last_sent_at"] = now
			if target.Status == "invalid" {
				// An address the provider accepts again is reachable again.
				targetChanges["status"] = "active"
			}
		case outcome.RetireTarget:
			targetChanges["status"] = "invalid"
			targetChanges["last_error"] = outcome.Detail
			targetChanges["failure_count"] = target.FailureCount + 1
		case outcome.Retried:
			targetChanges["failure_count"] = target.FailureCount + 1
			targetChanges["last_error"] = outcome.Detail
		default:
			return nil
		}
		return tx.Model(&persistence.NotificationTarget{}).Where("id = ?", target.ID).Updates(targetChanges).Error
	})
	return outcome
}

// notificationBackoff is the retry delay for an attempt, exponential with a ceiling
// (doc/tech.md §11.3). attempt is 1-based: the first retry waits half a minute.
func notificationBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := notificationBackoffBase
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= notificationBackoffMax {
			return notificationBackoffMax
		}
	}
	return delay
}

// newMessage builds the queue row for one (event, target) pair.
func newMessage(userID, targetID, channelID string, event Event, now time.Time) persistence.NotificationMessage {
	payload, _ := json.Marshal(messagePayload{Data: event.Data, CollapseID: event.CollapseID})
	return persistence.NotificationMessage{
		ID: persistence.NewID("notify_msg"), UserID: userID, ChannelID: channelID, TargetID: targetID,
		Topic: event.Topic, Title: event.Title, Body: event.Body, URL: event.URL, PayloadJSON: string(payload),
		Status: persistence.NotificationQueued, Attempts: 0, MaxAttempts: defaultMaxAttempts,
		RunAfter: now, CreatedAt: now, UpdatedAt: now,
	}
}

// notifyMessageFor renders a queued row back into what an adapter sends.
func notifyMessageFor(message persistence.NotificationMessage) notify.Message {
	var payload messagePayload
	_ = json.Unmarshal([]byte(message.PayloadJSON), &payload)
	return notify.Message{
		Topic: message.Topic, Title: message.Title, Body: message.Body, URL: message.URL,
		CollapseID: payload.CollapseID, Data: payload.Data,
	}
}

// channelViewFor renders one channel with its live target counts. A count query that
// fails yields zeros rather than an error: the channel itself was written, and the
// counts are a convenience the next list refreshes anyway.
func channelViewFor(ctx context.Context, s *NotificationService, channel persistence.NotificationChannel) ChannelView {
	total, active := targetCounts(ctx, s, channel.ID)
	return channelView(channel, total, active)
}

// targetCounts reports how many targets a channel has, and how many are active.
func targetCounts(ctx context.Context, s *NotificationService, channelID string) (int, int) {
	var row struct {
		Total  int
		Active int
	}
	err := s.Store.DB.WithContext(ctx).Model(&persistence.NotificationTarget{}).
		Select("COUNT(*) AS total, SUM(CASE WHEN status = 'active' THEN 1 ELSE 0 END) AS active").
		Where("channel_id = ?", channelID).Scan(&row).Error
	if err != nil {
		return 0, 0
	}
	return row.Total, row.Active
}

// channelView renders a channel for an API response.
func channelView(channel persistence.NotificationChannel, total, active int) ChannelView {
	return ChannelView{
		ID: channel.ID, Name: channel.Name, Provider: channel.Provider, Endpoint: channel.Endpoint,
		Settings: decodeSettings(channel.SettingsJSON), SecretHint: channel.SecretHint,
		SecretSet: channel.SecretCiphertext != "", Status: channel.Status,
		LastCheckAt: channel.LastCheckAt, LastCheckStatus: channel.LastCheckStatus, LastError: channel.LastError,
		TargetCount: total, ActiveTargetCount: active,
		Revision: channel.Revision, CreatedAt: channel.CreatedAt, UpdatedAt: channel.UpdatedAt,
	}
}

// encodeSettings stores the non-secret settings with sorted keys, so the same
// configuration always produces the same column value and a channel's revision stays
// meaningful.
func encodeSettings(settings map[string]string) string {
	normalized := normalizeSettings(settings)
	if len(normalized) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(normalized))
	for key := range normalized {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	builder.WriteString("{")
	for i, key := range keys {
		if i > 0 {
			builder.WriteString(",")
		}
		encodedKey, _ := json.Marshal(key)
		encodedValue, _ := json.Marshal(normalized[key])
		builder.Write(encodedKey)
		builder.WriteString(":")
		builder.Write(encodedValue)
	}
	builder.WriteString("}")
	return builder.String()
}

func decodeSettings(raw string) map[string]string {
	settings := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return settings
	}
	if err := json.Unmarshal([]byte(raw), &settings); err != nil || settings == nil {
		return map[string]string{}
	}
	return settings
}

// normalizeSettings drops empty keys and values and trims both, so a form that submits
// blank optional fields does not store them.
func normalizeSettings(settings map[string]string) map[string]string {
	normalized := map[string]string{}
	for key, value := range settings {
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		normalized[key] = value
	}
	return normalized
}

// normalizeEvent trims and clamps an event. It never invents a body: a notification with
// nothing to say should not be sent, and the caller decides that by leaving the topic
// empty.
func normalizeEvent(event Event) Event {
	event.Topic = strings.TrimSpace(event.Topic)
	event.Title = clampRunes(event.Title, maxTitleRunes)
	event.Body = clampRunes(event.Body, maxBodyRunes)
	event.URL = strings.TrimSpace(event.URL)
	event.CollapseID = strings.TrimSpace(event.CollapseID)
	event.ChannelID = strings.TrimSpace(event.ChannelID)
	if event.Title == "" {
		event.Title = event.Topic
	}
	if len(event.Data) > 0 {
		data := make(map[string]string, len(event.Data))
		for key, value := range event.Data {
			if key = strings.TrimSpace(key); key != "" {
				data[key] = clampRunes(value, 200)
			}
		}
		event.Data = data
	}
	return event
}

// validationError turns a configuration fault into the 422 the API maps, keeping the
// provider's own diagnosis, which is what tells an administrator what to fix.
func validationError(err error) error {
	return fmt.Errorf("%w: %s", ErrValidation, truncateError(err))
}

// truncateError bounds what a provider's complaint contributes to a stored column or an
// API response, and keeps a credential out of it.
func truncateError(err error) string {
	if err == nil {
		return ""
	}
	text := strings.TrimSpace(err.Error())
	if idx := strings.Index(text, "PRIVATE KEY"); idx >= 0 {
		text = text[:idx] + "PRIVATE KEY=<redacted>"
	}
	return clampRunes(text, 400)
}

// clampRunes trims to a rune budget, marking the cut so a truncated body is visibly
// truncated rather than silently wrong.
func clampRunes(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit <= 1 {
		return string(runes[:limit])
	}
	return string(runes[:limit-1]) + "…"
}
