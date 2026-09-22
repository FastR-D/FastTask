package application

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/persistence"
	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
	"gorm.io/gorm"
)

type CreateUserCommand struct {
	Identifier  string
	Password    string
	DisplayName string
	Timezone    string
	Locale      string
	Role        string
}

type UpdateUserCommand struct {
	DisplayName *string
	Timezone    *string
	Locale      *string
	Role        *string
	Status      *string
}

type SessionView struct {
	ID        string    `json:"id"`
	FamilyID  string    `json:"family_id"`
	UserID    string    `json:"user_id"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ModelProviderCommand struct {
	Name               string
	BaseURL            string
	ModelName          string
	TranscriptionModel string
	APIKey             string
	Status             string
	IsDefault          bool
}

type UpdateModelProviderCommand struct {
	Name               *string
	BaseURL            *string
	ModelName          *string
	TranscriptionModel *string
	APIKey             *string
	Status             *string
	IsDefault          *bool
}

type ProviderRuntimeConfig struct {
	Provider    agent.Provider
	Transcriber agent.Transcriber
	Record      persistence.ModelProvider
}

type AuditView struct {
	ID                string    `json:"id"`
	ActorUserID       string    `json:"actor_user_id"`
	TargetUserID      *string   `json:"target_user_id,omitempty"`
	Action            string    `json:"action"`
	DetailJSON        string    `json:"detail_json"`
	CreatedAt         time.Time `json:"created_at"`
	ActorIdentifier   string    `json:"actor_identifier"`
	ActorDisplayName  string    `json:"actor_display_name"`
	TargetIdentifier  string    `json:"target_identifier,omitempty"`
	TargetDisplayName string    `json:"target_display_name,omitempty"`
}

func (a *AdminService) CurrentAdmin(ctx context.Context) (persistence.User, error) {
	return currentAdmin(ctx, a.Store)
}

func (a *AdminService) ListUsers(ctx context.Context, query, status string) ([]persistence.User, error) {
	q := a.Store.DB.WithContext(ctx).Model(&persistence.User{})
	if text := strings.TrimSpace(query); text != "" {
		like := "%" + text + "%"
		q = q.Where("identifier LIKE ? OR display_name LIKE ?", like, like)
	}
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var users []persistence.User
	return users, q.Order("created_at DESC").Limit(500).Find(&users).Error
}

func (a *AdminService) CreateUser(ctx context.Context, actor persistence.User, command CreateUserCommand) (*persistence.User, error) {
	command.Identifier = strings.ToLower(strings.TrimSpace(command.Identifier))
	command.DisplayName = strings.TrimSpace(command.DisplayName)
	command.Role = normalizeRole(command.Role)
	if command.Timezone == "" {
		command.Timezone = "Asia/Shanghai"
	}
	if command.Locale == "" {
		command.Locale = "zh-CN"
	}
	if len(command.Identifier) < 3 || len(command.Identifier) > 120 || command.DisplayName == "" || !validRole(command.Role) {
		return nil, ErrValidation
	}
	if _, err := time.LoadLocation(command.Timezone); err != nil {
		return nil, ErrValidation
	}
	hash, err := platformauth.HashPassword(command.Password)
	if err != nil {
		return nil, ErrValidation
	}
	now := persistence.Now()
	user := &persistence.User{ID: persistence.NewID("user"), Identifier: command.Identifier, PasswordHash: hash, DisplayName: command.DisplayName, Timezone: command.Timezone, Locale: command.Locale, Role: command.Role, Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	err = a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Create(user).Error; err != nil {
			return uniqueConflict(err)
		}
		return auditAdmin(tx, actor, user.ID, "user.created", map[string]any{"role": user.Role})
	})
	if err != nil {
		return nil, err
	}
	return user, nil
}

func (a *AdminService) UpdateUser(ctx context.Context, actor persistence.User, id string, expected int, command UpdateUserCommand) (*persistence.User, error) {
	if command.Role != nil {
		*command.Role = normalizeRole(*command.Role)
		if !validRole(*command.Role) {
			return nil, ErrValidation
		}
	}
	if command.Status != nil && *command.Status != "active" && *command.Status != "disabled" {
		return nil, ErrValidation
	}
	if command.Timezone != nil {
		if _, err := time.LoadLocation(*command.Timezone); err != nil {
			return nil, ErrValidation
		}
	}
	var user persistence.User
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.First(&user, "id = ?", id).Error; err != nil {
			return notFound(err)
		}
		if user.Revision != expected {
			return ErrRevision
		}
		if (command.Role != nil && user.Role == "admin" && *command.Role != "admin") || (command.Status != nil && user.Status == "active" && *command.Status != "active") {
			var count int64
			if err := tx.Model(&persistence.User{}).Where("role = ? AND status = ? AND id <> ?", "admin", "active", id).Count(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				return ErrConflict
			}
		}
		changes := map[string]any{"revision": expected + 1, "updated_at": persistence.Now()}
		if command.DisplayName != nil {
			value := strings.TrimSpace(*command.DisplayName)
			if value == "" {
				return ErrValidation
			}
			changes["display_name"] = value
		}
		if command.Timezone != nil {
			changes["timezone"] = *command.Timezone
		}
		if command.Locale != nil {
			changes["locale"] = *command.Locale
		}
		if command.Role != nil {
			changes["role"] = *command.Role
		}
		if command.Status != nil {
			changes["status"] = *command.Status
			if *command.Status == "disabled" {
				if err := tx.Model(&persistence.Session{}).Where("user_id = ? AND status = ?", id, "active").Update("status", "revoked").Error; err != nil {
					return err
				}
			}
		}
		result := tx.Model(&persistence.User{}).Where("id = ? AND revision = ?", id, expected).Updates(changes)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevision
		}
		return auditAdmin(tx, actor, id, "user.updated", map[string]any{"changes": command})
	})
	if err != nil {
		return nil, err
	}
	if err := a.Store.DB.WithContext(ctx).First(&user, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

func (a *AdminService) ResetUserPassword(ctx context.Context, actor persistence.User, id, password string) error {
	hash, err := platformauth.HashPassword(password)
	if err != nil {
		return ErrValidation
	}
	return a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		result := tx.Model(&persistence.User{}).Where("id = ?", id).Updates(map[string]any{"password_hash": hash, "revision": gorm.Expr("revision + 1"), "updated_at": persistence.Now()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrNotFound
		}
		if err := tx.Model(&persistence.Session{}).Where("user_id = ? AND status = ?", id, "active").Update("status", "revoked").Error; err != nil {
			return err
		}
		return auditAdmin(tx, actor, id, "user.password_reset", map[string]any{"sessions_revoked": true})
	})
}

func (a *AdminService) ListUserSessions(ctx context.Context, userID string, activeOnly bool) ([]SessionView, error) {
	q := a.Store.DB.WithContext(ctx).Model(&persistence.Session{}).Where("user_id = ?", userID)
	if activeOnly {
		q = q.Where("status = ? AND expires_at > ?", "active", persistence.Now())
	}
	var sessions []persistence.Session
	if err := q.Order("created_at DESC").Limit(500).Find(&sessions).Error; err != nil {
		return nil, err
	}
	views := make([]SessionView, 0, len(sessions))
	for _, item := range sessions {
		views = append(views, SessionView{ID: item.ID, FamilyID: item.FamilyID, UserID: item.UserID, Status: item.Status, ExpiresAt: item.ExpiresAt, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt})
	}
	return views, nil
}

func (a *AdminService) RevokeUserSessions(ctx context.Context, actor persistence.User, id string) error {
	return a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		result := tx.Model(&persistence.Session{}).Where("user_id = ? AND status = ?", id, "active").Updates(map[string]any{"status": "revoked", "updated_at": persistence.Now()})
		if result.Error != nil {
			return result.Error
		}
		return auditAdmin(tx, actor, id, "user.sessions_revoked", map[string]any{"count": result.RowsAffected})
	})
}

func (a *ProviderService) ListModelProviders(ctx context.Context) ([]persistence.ModelProvider, error) {
	var providers []persistence.ModelProvider
	err := a.Store.DB.WithContext(ctx).Order("is_default DESC, updated_at DESC").Find(&providers).Error
	return providers, err
}

func (a *AdminService) ListAuditEvents(ctx context.Context, action string, limit int) ([]AuditView, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	q := a.Store.DB.WithContext(ctx).Model(&persistence.AdminAuditEvent{})
	if action != "" {
		q = q.Where("action = ?", action)
	}
	var events []persistence.AdminAuditEvent
	if err := q.Order("created_at DESC").Limit(limit).Find(&events).Error; err != nil {
		return nil, err
	}
	userIDs := make([]string, 0, len(events)*2)
	for _, event := range events {
		userIDs = append(userIDs, event.ActorUserID)
		if event.TargetUserID != nil {
			userIDs = append(userIDs, *event.TargetUserID)
		}
	}
	users := map[string]persistence.User{}
	if len(userIDs) > 0 {
		var records []persistence.User
		if err := a.Store.DB.WithContext(ctx).Where("id IN ?", userIDs).Find(&records).Error; err != nil {
			return nil, err
		}
		for _, user := range records {
			users[user.ID] = user
		}
	}
	views := make([]AuditView, 0, len(events))
	for _, event := range events {
		view := AuditView{ID: event.ID, ActorUserID: event.ActorUserID, TargetUserID: event.TargetUserID, Action: event.Action, DetailJSON: event.DetailJSON, CreatedAt: event.CreatedAt, ActorIdentifier: users[event.ActorUserID].Identifier, ActorDisplayName: users[event.ActorUserID].DisplayName}
		if event.TargetUserID != nil {
			view.TargetIdentifier = users[*event.TargetUserID].Identifier
			view.TargetDisplayName = users[*event.TargetUserID].DisplayName
		}
		views = append(views, view)
	}
	return views, nil
}

func (a *ProviderService) CreateModelProvider(ctx context.Context, actor persistence.User, command ModelProviderCommand) (*persistence.ModelProvider, error) {
	if err := validateProviderCommand(command); err != nil {
		return nil, err
	}
	ciphertext, hint, err := a.encryptProviderKey(command.APIKey)
	if err != nil {
		return nil, err
	}
	now := persistence.Now()
	provider := &persistence.ModelProvider{ID: persistence.NewID("provider"), Name: strings.TrimSpace(command.Name), ProviderType: "openai_compatible", BaseURL: strings.TrimRight(command.BaseURL, "/"), ModelName: strings.TrimSpace(command.ModelName), TranscriptionModel: strings.TrimSpace(command.TranscriptionModel), APIKeyCiphertext: ciphertext, APIKeyHint: hint, Status: "active", IsDefault: command.IsDefault, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if command.Status == "disabled" {
		provider.Status = "disabled"
		provider.IsDefault = false
	}
	err = a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := clearDefaultProvider(tx, provider.IsDefault); err != nil {
			return err
		}
		if err := tx.Create(provider).Error; err != nil {
			return uniqueConflict(err)
		}
		return auditAdmin(tx, actor, "", "model_provider.created", map[string]any{"provider_id": provider.ID, "name": provider.Name, "is_default": provider.IsDefault})
	})
	if err != nil {
		return nil, err
	}
	return provider, nil
}

func (a *ProviderService) UpdateModelProvider(ctx context.Context, actor persistence.User, id string, expected int, command UpdateModelProviderCommand) (*persistence.ModelProvider, error) {
	var provider persistence.ModelProvider
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.First(&provider, "id = ?", id).Error; err != nil {
			return notFound(err)
		}
		if provider.Revision != expected {
			return ErrRevision
		}
		if command.Name != nil && strings.TrimSpace(*command.Name) == "" {
			return ErrValidation
		}
		if command.BaseURL != nil {
			parsed, err := url.Parse(strings.TrimSpace(*command.BaseURL))
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
				return ErrValidation
			}
		}
		if command.ModelName != nil && strings.TrimSpace(*command.ModelName) == "" {
			return ErrValidation
		}
		if command.Status != nil && *command.Status != "active" && *command.Status != "disabled" {
			return ErrValidation
		}
		ciphertext, hint := "", ""
		var err error
		if command.APIKey != nil && *command.APIKey != "" {
			ciphertext, hint, err = a.encryptProviderKey(*command.APIKey)
			if err != nil {
				return err
			}
		}
		changes := map[string]any{"revision": expected + 1, "updated_at": persistence.Now()}
		if command.Name != nil {
			changes["name"] = strings.TrimSpace(*command.Name)
		}
		if command.BaseURL != nil {
			changes["base_url"] = strings.TrimRight(strings.TrimSpace(*command.BaseURL), "/")
		}
		if command.ModelName != nil {
			changes["model_name"] = strings.TrimSpace(*command.ModelName)
		}
		if command.TranscriptionModel != nil {
			changes["transcription_model"] = strings.TrimSpace(*command.TranscriptionModel)
		}
		if ciphertext != "" {
			changes["api_key_ciphertext"] = ciphertext
			changes["api_key_hint"] = hint
		}
		if command.Status != nil {
			changes["status"] = *command.Status
		}
		isDefault := provider.IsDefault
		if command.IsDefault != nil {
			isDefault = *command.IsDefault
		}
		if isDefault {
			changes["is_default"] = true
			changes["status"] = "active"
		} else if command.IsDefault != nil {
			changes["is_default"] = false
		}
		if err := clearDefaultProvider(tx, isDefault); err != nil {
			return err
		}
		result := tx.Model(&persistence.ModelProvider{}).Where("id = ? AND revision = ?", id, expected).Updates(changes)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevision
		}
		safeCommand := command
		safeCommand.APIKey = nil
		return auditAdmin(tx, actor, "", "model_provider.updated", map[string]any{"provider_id": id, "changes": safeCommand})
	})
	if err != nil {
		return nil, err
	}
	if err := a.Store.DB.WithContext(ctx).First(&provider, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &provider, nil
}

func (a *ProviderService) ActivateModelProvider(ctx context.Context, actor persistence.User, id string) (*persistence.ModelProvider, error) {
	var provider persistence.ModelProvider
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.First(&provider, "id = ?", id).Error; err != nil {
			return notFound(err)
		}
		if err := clearDefaultProvider(tx, true); err != nil {
			return err
		}
		result := tx.Model(&persistence.ModelProvider{}).Where("id = ?", id).Updates(map[string]any{"status": "active", "is_default": true, "revision": provider.Revision + 1, "updated_at": persistence.Now()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevision
		}
		return auditAdmin(tx, actor, "", "model_provider.activated", map[string]any{"provider_id": id})
	})
	if err != nil {
		return nil, err
	}
	if err := a.Store.DB.WithContext(ctx).First(&provider, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &provider, nil
}

func (a *ProviderService) DeleteModelProvider(ctx context.Context, actor persistence.User, id string) error {
	return a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var provider persistence.ModelProvider
		if err := tx.First(&provider, "id = ?", id).Error; err != nil {
			return notFound(err)
		}
		if provider.IsDefault {
			return ErrConflict
		}
		if err := tx.Delete(&provider).Error; err != nil {
			return err
		}
		return auditAdmin(tx, actor, "", "model_provider.deleted", map[string]any{"provider_id": id, "name": provider.Name})
	})
}

func (a *ProviderService) VerifyModelProvider(ctx context.Context, actor persistence.User, id string) (string, error) {
	if _, err := currentAdmin(ctx, a.Store); err != nil {
		return "", err
	}
	var provider persistence.ModelProvider
	if err := a.Store.DB.WithContext(ctx).First(&provider, "id = ?", id).Error; err != nil {
		return "", notFound(err)
	}
	key, err := a.decryptSecret(provider.APIKeyCiphertext)
	if err != nil {
		return "", err
	}
	if err := agent.NewOpenAIValues(provider.BaseURL, provider.ModelName, key, provider.TranscriptionModel).Verify(ctx); err != nil {
		return provider.Name, err
	}
	return provider.Name, nil
}

func (a *ProviderService) ActiveProviderRuntime(ctx context.Context) (*ProviderRuntimeConfig, error) {
	var provider persistence.ModelProvider
	err := a.Store.DB.WithContext(ctx).Where("status = ?", "active").Order("is_default DESC, updated_at DESC").First(&provider).Error
	if persistence.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	key, err := a.decryptSecret(provider.APIKeyCiphertext)
	if err != nil {
		return nil, err
	}
	openai := agent.NewOpenAIValues(provider.BaseURL, provider.ModelName, key, provider.TranscriptionModel)
	runtime := &ProviderRuntimeConfig{Record: provider, Provider: openai}
	if provider.TranscriptionModel != "" {
		runtime.Transcriber = openai
	}
	return runtime, nil
}

func (a *ProviderService) encryptProviderKey(key string) (string, string, error) {
	key = strings.TrimSpace(key)
	if len(key) < 12 {
		return "", "", ErrValidation
	}
	ciphertext, err := a.encryptSecret(key)
	if err != nil {
		return "", "", err
	}
	hint := key
	if len(key) > 8 {
		hint = key[:4] + "..." + key[len(key)-4:]
	}
	return ciphertext, hint, nil
}

func (a *ProviderService) EncryptProviderKey(key string) (string, string, error) {
	return a.encryptProviderKey(key)
}

func (a *ProviderService) DecryptProviderKey(ciphertext string) (string, error) {
	return a.decryptSecret(ciphertext)
}

func validateProviderCommand(command ModelProviderCommand) error {
	command.Name, command.BaseURL, command.ModelName = strings.TrimSpace(command.Name), strings.TrimSpace(command.BaseURL), strings.TrimSpace(command.ModelName)
	if command.Name == "" || command.ModelName == "" || command.BaseURL == "" {
		return ErrValidation
	}
	parsed, err := url.Parse(command.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return ErrValidation
	}
	if command.Status != "" && command.Status != "active" && command.Status != "disabled" {
		return ErrValidation
	}
	return nil
}

func clearDefaultProvider(tx *gorm.DB, clear bool) error {
	if !clear {
		return nil
	}
	return tx.Model(&persistence.ModelProvider{}).Where("is_default = ?", true).Update("is_default", false).Error
}

func normalizeRole(role string) string {
	if role == "" {
		return "member"
	}
	return strings.ToLower(strings.TrimSpace(role))
}

func validRole(role string) bool { return role == "member" || role == "admin" }

func auditAdmin(tx *gorm.DB, actor persistence.User, targetID, action string, detail any) error {
	encoded, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	var target *string
	if targetID != "" {
		target = &targetID
	}
	return tx.Create(&persistence.AdminAuditEvent{ID: persistence.NewID("audit"), ActorUserID: actor.ID, TargetUserID: target, Action: action, DetailJSON: string(encoded), CreatedAt: persistence.Now()}).Error
}

func uniqueConflict(err error) error {
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		return ErrConflict
	}
	return err
}
