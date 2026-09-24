package application

import (
	"context"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/notify"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

// NotificationTargetService owns the addresses a user can be reached at
// (doc/notification.md §4). It is separate from NotificationService for the same reason
// the other aggregates are separate (doc/wiring.md §4): a target is the user's
// subscription, a channel is the operator's credential, and the two have different
// owners, different authorization rules and different lifetimes.
//
// It holds the NotificationService to reuse two things it must not duplicate: the
// adapter cache, and the recorder that writes a delivery outcome.
type NotificationTargetService struct {
	Store     *persistence.Store
	secretKey []byte
	channels  *NotificationService
}

// NewNotificationTargetService builds the target service over the channel service.
func NewNotificationTargetService(store *persistence.Store, secretKey []byte, channels *NotificationService) *NotificationTargetService {
	return &NotificationTargetService{Store: store, secretKey: secretKey, channels: channels}
}

// maxTargetLabel is the length of the note a person gives a device ("张三的 iPhone").
const maxTargetLabel = 60

// TargetCommand registers an address.
type TargetCommand struct {
	ChannelID string
	Address   string
	Label     string
	// Status defaults to active. An administrator registering a target they are not
	// ready to use stores it disabled.
	Status string
}

// UpdateTargetCommand patches a target. A nil field is left alone. A new Address is a
// re-registration: it replaces the stored credential and clears the failure history,
// which is what a rotated push token needs.
type UpdateTargetCommand struct {
	Label   *string
	Status  *string
	Address *string
}

// TargetFilter narrows a target listing. UserID is always set for a self-service call;
// an administrator may leave it empty and filter by channel instead.
type TargetFilter struct {
	UserID    string
	ChannelID string
	Status    string
	Limit     int
}

// TargetView is a target as an API returns it. The address itself is never in it: for
// FCM and APNs it is a device credential, and the masked hint plus the label are what a
// person recognises it by.
type TargetView struct {
	ID              string     `json:"id"`
	UserID          string     `json:"user_id"`
	UserIdentifier  string     `json:"user_identifier,omitempty"`
	UserDisplayName string     `json:"user_display_name,omitempty"`
	ChannelID       string     `json:"channel_id"`
	ChannelName     string     `json:"channel_name,omitempty"`
	Provider        string     `json:"provider,omitempty"`
	Label           string     `json:"label"`
	AddressHint     string     `json:"address_hint"`
	Status          string     `json:"status"`
	FailureCount    int        `json:"failure_count"`
	LastError       string     `json:"last_error,omitempty"`
	LastSentAt      *time.Time `json:"last_sent_at,omitempty"`
	Revision        int        `json:"revision"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// ListTargets returns the targets a caller may see, with the user and channel names a
// list needs.
func (s *NotificationTargetService) ListTargets(ctx context.Context, filter TargetFilter) ([]TargetView, error) {
	limit := filter.Limit
	if limit < 1 || limit > 500 {
		limit = 200
	}
	query := s.Store.DB.WithContext(ctx).Model(&persistence.NotificationTarget{})
	if filter.UserID != "" {
		query = query.Where("user_id = ?", filter.UserID)
	}
	if filter.ChannelID != "" {
		query = query.Where("channel_id = ?", filter.ChannelID)
	}
	if filter.Status != "" {
		query = query.Where("status = ?", filter.Status)
	}
	var targets []persistence.NotificationTarget
	if err := query.Order("created_at DESC").Limit(limit).Find(&targets).Error; err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return []TargetView{}, nil
	}
	userIDs := make([]string, 0, len(targets))
	channelIDs := make([]string, 0, len(targets))
	for _, target := range targets {
		userIDs = append(userIDs, target.UserID)
		channelIDs = append(channelIDs, target.ChannelID)
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
	views := make([]TargetView, 0, len(targets))
	for _, target := range targets {
		views = append(views, targetView(target, users[target.UserID], channels[target.ChannelID]))
	}
	return views, nil
}

// CreateTarget registers an address for user owner. actor is non-nil exactly when an
// administrator acts on somebody else's behalf, and is who the audit event names; a
// self-service registration passes nil and writes no audit row.
//
// Re-registering an address that already exists revives it instead of failing: a push
// token rotates every time an app is reinstalled, and the second registration is the
// same device, not a conflict.
func (s *NotificationTargetService) CreateTarget(ctx context.Context, actor *persistence.User, owner string, command TargetCommand) (*TargetView, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, ErrValidation
	}
	var ownerExists int64
	if err := s.Store.DB.WithContext(ctx).Model(&persistence.User{}).Where("id = ?", owner).Count(&ownerExists).Error; err != nil {
		return nil, err
	}
	if ownerExists == 0 {
		// Without this the foreign key would report the mistake as an internal error.
		return nil, ErrValidation
	}
	address := strings.TrimSpace(command.Address)
	label := clampRunes(command.Label, maxTargetLabel)
	status := strings.TrimSpace(command.Status)
	if status == "" {
		status = "active"
	}
	if status != "active" && status != "disabled" {
		return nil, ErrValidation
	}
	var channel persistence.NotificationChannel
	if err := s.Store.DB.WithContext(ctx).First(&channel, "id = ?", strings.TrimSpace(command.ChannelID)).Error; err != nil {
		return nil, notFound(err)
	}
	if channel.Status != "active" {
		return nil, ErrConflict
	}
	if err := notify.ValidateAddress(channel.Provider, address); err != nil {
		return nil, validationError(err)
	}
	ciphertext, err := encryptWithKey(s.secretKey, address)
	if err != nil {
		return nil, err
	}
	now := persistence.Now()
	target := persistence.NotificationTarget{
		ID: persistence.NewID("notify_target"), UserID: owner, ChannelID: channel.ID, Label: label,
		AddressCiphertext: ciphertext, AddressHash: persistence.Hash(address), AddressHint: addressHint(address),
		Status: status, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	err = s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var existing persistence.NotificationTarget
		err := tx.Where("user_id = ? AND channel_id = ? AND address_hash = ?", owner, channel.ID, target.AddressHash).
			First(&existing).Error
		if err == nil {
			// The same address on the same channel: revive it rather than rejecting the
			// registration, and keep its id so its delivery history stays attached.
			result := tx.Model(&persistence.NotificationTarget{}).Where("id = ?", existing.ID).Updates(map[string]any{
				"status": status, "label": label, "failure_count": 0, "last_error": "",
				"address_ciphertext": ciphertext, "revision": gorm.Expr("revision + 1"), "updated_at": now,
			})
			if result.Error != nil {
				return result.Error
			}
			target = existing
			target.Status, target.Label, target.FailureCount, target.LastError = status, label, 0, ""
			target.Revision, target.UpdatedAt = existing.Revision+1, now
			return auditTarget(tx, actor, &target, channel, "notification_target.registered")
		}
		if !persistence.IsNotFound(err) {
			return err
		}
		if err := tx.Create(&target).Error; err != nil {
			return uniqueConflict(err)
		}
		return auditTarget(tx, actor, &target, channel, "notification_target.created")
	})
	if err != nil {
		return nil, err
	}
	var user persistence.User
	_ = s.Store.DB.WithContext(ctx).First(&user, "id = ?", owner).Error
	view := targetView(target, user, channel)
	return &view, nil
}

// UpdateTarget patches a target's label, status or address.
func (s *NotificationTargetService) UpdateTarget(ctx context.Context, scopeUserID string, actor *persistence.User, id string, expected int, command UpdateTargetCommand) (*TargetView, error) {
	var target persistence.NotificationTarget
	var channel persistence.NotificationChannel
	err := s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		query := tx.Where("id = ?", id)
		if scopeUserID != "" {
			query = query.Where("user_id = ?", scopeUserID)
		}
		if err := query.First(&target).Error; err != nil {
			return notFound(err)
		}
		if target.Revision != expected {
			return ErrRevision
		}
		if err := tx.First(&channel, "id = ?", target.ChannelID).Error; err != nil {
			return notFound(err)
		}
		changes := map[string]any{"revision": expected + 1, "updated_at": persistence.Now()}
		if command.Label != nil {
			label := clampRunes(*command.Label, maxTargetLabel)
			changes["label"] = label
			target.Label = label
		}
		if command.Status != nil {
			status := strings.TrimSpace(*command.Status)
			if status != "active" && status != "disabled" {
				return ErrValidation
			}
			changes["status"] = status
			if status == "active" {
				// Re-enabling is a person asserting the address is reachable again, so the
				// provider's verdict and its failure count are cleared with it.
				changes["failure_count"] = 0
				changes["last_error"] = ""
				target.FailureCount, target.LastError = 0, ""
			}
			target.Status = status
		}
		if command.Address != nil {
			address := strings.TrimSpace(*command.Address)
			if err := notify.ValidateAddress(channel.Provider, address); err != nil {
				return validationError(err)
			}
			ciphertext, err := encryptWithKey(s.secretKey, address)
			if err != nil {
				return err
			}
			changes["address_ciphertext"] = ciphertext
			changes["address_hash"] = persistence.Hash(address)
			changes["address_hint"] = addressHint(address)
			changes["failure_count"] = 0
			changes["last_error"] = ""
			target.AddressHash, target.AddressHint = persistence.Hash(address), addressHint(address)
			target.FailureCount, target.LastError = 0, ""
		}
		result := tx.Model(&persistence.NotificationTarget{}).
			Where("id = ? AND revision = ?", id, expected).Updates(changes)
		if result.Error != nil {
			return uniqueConflict(result.Error)
		}
		if result.RowsAffected != 1 {
			return ErrRevision
		}
		target.Revision, target.UpdatedAt = expected+1, persistence.Now()
		action := "notification_target.updated"
		if command.Address != nil {
			action = "notification_target.reregistered"
		}
		return auditTarget(tx, actor, &target, channel, action)
	})
	if err != nil {
		return nil, err
	}
	var user persistence.User
	_ = s.Store.DB.WithContext(ctx).First(&user, "id = ?", target.UserID).Error
	view := targetView(target, user, channel)
	return &view, nil
}

// DeleteTarget removes a subscription. Its delivery history goes with it (migration
// 000010's CASCADE), which is the point: the history is a diagnostic for a target that
// no longer exists, and it holds a masked copy of a device credential.
func (s *NotificationTargetService) DeleteTarget(ctx context.Context, scopeUserID string, actor *persistence.User, id string) error {
	return s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var target persistence.NotificationTarget
		query := tx.Where("id = ?", id)
		if scopeUserID != "" {
			query = query.Where("user_id = ?", scopeUserID)
		}
		if err := query.First(&target).Error; err != nil {
			return notFound(err)
		}
		if err := tx.Delete(&target).Error; err != nil {
			return err
		}
		var channel persistence.NotificationChannel
		_ = tx.First(&channel, "id = ?", target.ChannelID).Error
		return auditTarget(tx, actor, &target, channel, "notification_target.deleted")
	})
}

// TestTarget sends one message to a stored target and reports what the provider said.
// The delivery is recorded like any other, so the test appears in the log and an
// administrator can see why it failed rather than only that it did.
func (s *NotificationTargetService) TestTarget(ctx context.Context, actor *persistence.User, scopeUserID, id string) (DeliveryResult, error) {
	var target persistence.NotificationTarget
	var channel persistence.NotificationChannel
	query := s.Store.DB.WithContext(ctx).Model(&persistence.NotificationTarget{}).Where("notification_targets.id = ?", id)
	if scopeUserID != "" {
		query = query.Where("notification_targets.user_id = ?", scopeUserID)
	}
	if err := query.First(&target).Error; err != nil {
		return DeliveryResult{}, notFound(err)
	}
	if err := s.Store.DB.WithContext(ctx).First(&channel, "id = ?", target.ChannelID).Error; err != nil {
		return DeliveryResult{}, notFound(err)
	}
	result := DeliveryResult{Provider: channel.Provider, Channel: channel.Name}
	if target.Status != "active" {
		result.Detail = "该接收端已停用，测试未发送"
		return result, nil
	}
	now := persistence.Now()
	message := newMessage(target.UserID, target.ID, channel.ID, normalizeEvent(Event{
		Topic: TopicNotificationTest, Title: "FastTask 通知测试",
		Body: "这条消息来自后台的测试发送。收到即表示该接收端可用。",
	}), now)
	// The test is claimed up front and gets one attempt: a test that requeues itself
	// would deliver minutes later, after the administrator stopped watching.
	message.Status, message.Attempts, message.MaxAttempts = persistence.NotificationSending, 1, 1
	message.RunAfter = now.Add(sendLease)
	if err := s.Store.DB.WithContext(ctx).Create(&message).Error; err != nil {
		return result, err
	}
	outcome := sendClaimed(ctx, s.channels, message)
	var stored persistence.NotificationMessage
	if err := s.Store.DB.WithContext(ctx).First(&stored, "id = ?", message.ID).Error; err != nil {
		return result, err
	}
	result.Delivered = stored.Status == persistence.NotificationSent
	result.ProviderMessageID = stored.ProviderMessageID
	result.Detail = outcome.Detail
	if !result.Delivered {
		result.Detail = stored.LastError
	}
	result.InvalidTarget = outcome.RetireTarget
	if actor != nil {
		if err := auditAdmin(s.Store.DB.WithContext(ctx), *actor, target.UserID, "notification_target.test", map[string]any{
			"target_id": target.ID, "channel_id": channel.ID, "delivered": result.Delivered,
		}); err != nil {
			return result, err
		}
	}
	return result, nil
}

// auditTarget writes an audit event for an administrator's action on a target. A
// self-service change has no actor and writes nothing: the audit log is the record of
// what administrators did, and a user editing their own phone is not that.
//
// The address is never in the event, only its masked hint.
func auditTarget(tx *gorm.DB, actor *persistence.User, target *persistence.NotificationTarget, channel persistence.NotificationChannel, action string) error {
	if actor == nil {
		return nil
	}
	return auditAdmin(tx, *actor, target.UserID, action, map[string]any{
		"target_id": target.ID, "channel_id": target.ChannelID, "provider": channel.Provider,
		"status": target.Status, "address_hint": target.AddressHint,
	})
}

// targetView renders a target for an API response.
func targetView(target persistence.NotificationTarget, user persistence.User, channel persistence.NotificationChannel) TargetView {
	view := TargetView{
		ID: target.ID, UserID: target.UserID, ChannelID: target.ChannelID, Label: target.Label,
		AddressHint: target.AddressHint, Status: target.Status, FailureCount: target.FailureCount,
		LastError: target.LastError, LastSentAt: target.LastSentAt,
		Revision: target.Revision, CreatedAt: target.CreatedAt, UpdatedAt: target.UpdatedAt,
	}
	if user.ID != "" {
		view.UserIdentifier, view.UserDisplayName = user.Identifier, user.DisplayName
	}
	if channel.ID != "" {
		view.ChannelName, view.Provider = channel.Name, channel.Provider
	}
	return view
}

// addressHint masks an address for display. A Bark key, an FCM registration token and an
// APNs device token are all credentials — holding one is the ability to push to
// somebody's phone — so the hint shows only enough to tell two devices apart.
func addressHint(address string) string {
	address = strings.TrimSpace(address)
	if len(address) <= 8 {
		return "••••"
	}
	return "…" + address[len(address)-4:]
}
