package persistence

import "time"

// PanelProfile belongs to one FastTask account. The avatar is a validated,
// normalized image, never an arbitrary remote URL or a shared public file.
type PanelProfile struct {
	UserID          string                       `json:"user_id" gorm:"primaryKey"`
	DisplayName     string                       `json:"display_name"`
	Seat            string                       `json:"seat"`
	Title           string                       `json:"title"`
	AvatarDataURL   string                       `json:"avatar_data_url"`
	WeatherOverride string                       `json:"weather_override"`
	TaskOverrides   map[string]PanelTaskOverride `json:"task_overrides" gorm:"column:task_overrides_json;serializer:json"`
	Revision        int                          `json:"revision"`
	CreatedAt       time.Time                    `json:"created_at"`
	UpdatedAt       time.Time                    `json:"updated_at"`
}

// An override changes display text and ordering only, never completion state.
// Keys are the stable logical IDs of daily plan items.
type PanelTaskOverride struct {
	Title    *string `json:"title,omitempty" maxLength:"300"`
	Detail   *string `json:"detail,omitempty" maxLength:"500"`
	Selected *bool   `json:"selected,omitempty"`
	Position *int    `json:"position,omitempty" minimum:"0" maximum:"999"`
}

type PanelPairing struct {
	ID         string     `json:"id" gorm:"primaryKey"`
	UserID     string     `json:"user_id"`
	DeviceName string     `json:"device_name"`
	Scope      string     `json:"scope"`
	TokenHash  string     `json:"-"`
	ExpiresAt  time.Time  `json:"expires_at"`
	CreatedAt  time.Time  `json:"created_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}
