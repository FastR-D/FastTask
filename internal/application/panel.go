package application

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

const PanelSnapshotScope = "panel:snapshot:read"
const maxPanelAvatarBytes = 1 << 20

type PanelService struct{ Store *persistence.Store }

func NewPanelService(store *persistence.Store) *PanelService { return &PanelService{Store: store} }

type PanelProfileInput struct {
	DisplayName     string                                   `json:"display_name" maxLength:"40"`
	Seat            string                                   `json:"seat" maxLength:"40"`
	Title           string                                   `json:"title" maxLength:"40"`
	AvatarDataURL   string                                   `json:"avatar_data_url" maxLength:"1400000"`
	WeatherOverride string                                   `json:"weather_override" maxLength:"80"`
	TaskOverrides   map[string]persistence.PanelTaskOverride `json:"task_overrides"`
}

func (s *PanelService) Profile(ctx context.Context, userID string) (*persistence.PanelProfile, error) {
	var result persistence.PanelProfile
	err := s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var err error
		result, err = panelProfileTx(tx, userID)
		return err
	})
	return &result, err
}

func panelProfileTx(tx *gorm.DB, userID string) (persistence.PanelProfile, error) {
	var user persistence.User
	var profile persistence.PanelProfile
	if err := tx.Where("id = ? AND status = 'active'", userID).First(&user).Error; err != nil {
		return profile, notFound(err)
	}
	err := tx.Where("user_id = ?", userID).First(&profile).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		now := persistence.Now()
		profile = persistence.PanelProfile{UserID: userID, DisplayName: user.DisplayName, Title: "今日任务", TaskOverrides: map[string]persistence.PanelTaskOverride{}, Revision: 1, CreatedAt: now, UpdatedAt: now}
		err = tx.Create(&profile).Error
	}
	return profile, err
}

func (s *PanelService) PutProfile(ctx context.Context, userID string, expected int, input PanelProfileInput) (*persistence.PanelProfile, error) {
	if !panelText(input.DisplayName, 40) || !panelText(input.Seat, 40) || !panelText(input.Title, 40) || !panelText(input.WeatherOverride, 80) {
		return nil, fmt.Errorf("%w: name, seat, title and weather must be short single-line text", ErrValidation)
	}
	if len(input.TaskOverrides) > 100 {
		return nil, fmt.Errorf("%w: at most 100 display corrections are allowed", ErrValidation)
	}
	for key, override := range input.TaskOverrides {
		if key == "" || !panelText(key, 200) || (override.Title != nil && !panelMultilineText(*override.Title, 300)) || (override.Detail != nil && !panelMultilineText(*override.Detail, 500)) || (override.Position != nil && (*override.Position < 0 || *override.Position > 999)) {
			return nil, fmt.Errorf("%w: invalid task display correction", ErrValidation)
		}
	}
	if input.TaskOverrides == nil {
		input.TaskOverrides = map[string]persistence.PanelTaskOverride{}
	}
	encodedOverrides, err := json.Marshal(input.TaskOverrides)
	if err != nil {
		return nil, ErrValidation
	}
	avatar, err := normalizePanelAvatar(input.AvatarDataURL)
	if err != nil {
		return nil, err
	}
	var result persistence.PanelProfile
	err = s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var err error
		result, err = panelProfileTx(tx, userID)
		if err != nil {
			return err
		}
		if expected != result.Revision {
			return ErrRevision
		}
		if input.AvatarDataURL == result.AvatarDataURL {
			// Saving unrelated text must not repeatedly recompress an existing JPEG.
			avatar = result.AvatarDataURL
		}
		updates := map[string]any{"display_name": strings.TrimSpace(input.DisplayName), "seat": strings.TrimSpace(input.Seat), "title": strings.TrimSpace(input.Title), "avatar_data_url": avatar, "weather_override": strings.TrimSpace(input.WeatherOverride), "task_overrides_json": string(encodedOverrides), "revision": result.Revision + 1, "updated_at": persistence.Now()}
		if err := tx.Model(&persistence.PanelProfile{}).Where("user_id = ? AND revision = ?", userID, expected).Updates(updates).Error; err != nil {
			return err
		}
		return tx.Where("user_id = ?", userID).First(&result).Error
	})
	return &result, err
}

func panelText(value string, limit int) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > limit {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func panelMultilineText(value string, limit int) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > limit {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
	}
	return true
}

func normalizePanelAvatar(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	invalid := func() (string, error) {
		return "", fmt.Errorf("%w: avatar must be a PNG/JPEG image up to 1 MiB and 1024 by 1024 pixels", ErrValidation)
	}
	if len(value) > 1400000 {
		return invalid()
	}
	prefix, encoded, ok := strings.Cut(value, ",")
	if !ok || (prefix != "data:image/png;base64" && prefix != "data:image/jpeg;base64") {
		return invalid()
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > maxPanelAvatarBytes {
		return invalid()
	}
	info, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || info.Width < 1 || info.Height < 1 || info.Width > 1024 || info.Height > 1024 || prefix != "data:image/"+format+";base64" {
		return invalid()
	}
	decoded, decodedFormat, err := image.Decode(bytes.NewReader(data))
	if err != nil || decodedFormat != format {
		return invalid()
	}
	// Re-encoding removes EXIF GPS and any trailing payload. Nothing supplied by
	// the client is treated as a file path, URL, or browser executable content.
	var clean bytes.Buffer
	if format == "jpeg" {
		err = jpeg.Encode(&clean, decoded, &jpeg.Options{Quality: 85})
	} else {
		encoder := png.Encoder{CompressionLevel: png.BestCompression}
		err = encoder.Encode(&clean, decoded)
	}
	if err != nil || clean.Len() > maxPanelAvatarBytes {
		return invalid()
	}
	return prefix + "," + base64.StdEncoding.EncodeToString(clean.Bytes()), nil
}

func (s *PanelService) Pairings(ctx context.Context, userID string) ([]persistence.PanelPairing, error) {
	items := []persistence.PanelPairing{}
	err := s.Store.DB.WithContext(ctx).Where("user_id = ?", userID).Order("created_at DESC, id DESC").Find(&items).Error
	return items, err
}

func (s *PanelService) CreatePairing(ctx context.Context, userID, deviceName string) (*persistence.PanelPairing, string, error) {
	deviceName = strings.TrimSpace(deviceName)
	if deviceName == "" || !panelText(deviceName, 80) {
		return nil, "", fmt.Errorf("%w: provide a device name up to 80 characters", ErrValidation)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, "", err
	}
	token := "ftp_" + base64.RawURLEncoding.EncodeToString(secret)
	now := persistence.Now()
	result := persistence.PanelPairing{ID: persistence.NewID("panel"), UserID: userID, DeviceName: deviceName, Scope: PanelSnapshotScope, TokenHash: persistence.Hash(token), ExpiresAt: now.Add(180 * 24 * time.Hour), CreatedAt: now}
	err := s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var user persistence.User
		if err := tx.Where("id = ? AND status = 'active'", userID).First(&user).Error; err != nil {
			return notFound(err)
		}
		var count int64
		if err := tx.Model(&persistence.PanelPairing{}).Where("user_id = ? AND revoked_at IS NULL AND expires_at > ?", userID, now).Count(&count).Error; err != nil {
			return err
		}
		if count >= 20 {
			return fmt.Errorf("%w: revoke an old panel pairing before creating another", ErrConflict)
		}
		return tx.Create(&result).Error
	})
	if err != nil {
		return nil, "", err
	}
	return &result, token, nil
}

func (s *PanelService) RevokePairing(ctx context.Context, userID, id string) error {
	return s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var pairing persistence.PanelPairing
		if err := tx.Where("id = ? AND user_id = ?", id, userID).First(&pairing).Error; err != nil {
			return notFound(err)
		}
		if pairing.RevokedAt != nil {
			return nil
		}
		return tx.Model(&persistence.PanelPairing{}).Where("id = ? AND user_id = ?", id, userID).Update("revoked_at", persistence.Now()).Error
	})
}

// AuthenticatePairing is intentionally separate from user/session/device auth.
// Only the snapshot endpoint accepts this capability; it grants no write scope.
func (s *PanelService) AuthenticatePairing(ctx context.Context, token string) (string, error) {
	if len(token) != 47 || !strings.HasPrefix(token, "ftp_") {
		return "", ErrNotFound
	}
	var pairing persistence.PanelPairing
	err := s.Store.DB.WithContext(ctx).Table("panel_pairings").Select("panel_pairings.*").Joins("JOIN users ON users.id = panel_pairings.user_id AND users.status = 'active'").Where("panel_pairings.token_hash = ? AND panel_pairings.scope = ? AND panel_pairings.revoked_at IS NULL", persistence.Hash(token), PanelSnapshotScope).First(&pairing).Error
	if err != nil {
		return "", notFound(err)
	}
	// Compare actual instants in Go: SQLite compares datetime text lexically,
	// so an imported offset such as +08:00 must not extend a credential's life.
	if !pairing.ExpiresAt.After(persistence.Now()) {
		return "", ErrNotFound
	}
	return pairing.UserID, nil
}
