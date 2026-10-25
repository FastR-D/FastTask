package application

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/persistence"
)

func panelTestStore(t *testing.T) (*persistence.Store, string, string) {
	t.Helper()
	store, err := persistence.Open(filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"panel-user-a", "panel-user-b"} {
		now := persistence.Now()
		user := persistence.User{ID: id, Identifier: id, PasswordHash: "not-used", DisplayName: id, Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
		if err := store.DB.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
	}
	return store, "panel-user-a", "panel-user-b"
}

func TestPanelProfileIsolationAndRevision(t *testing.T) {
	store, a, b := panelTestStore(t)
	service := NewPanelService(store)
	ctx := context.Background()
	initial, err := service.Profile(ctx, a)
	if err != nil || initial.DisplayName != a || initial.Revision != 1 || initial.Title != "今日任务" || initial.TaskOverrides == nil {
		t.Fatalf("default profile: %+v %v", initial, err)
	}
	title := "自定义显示"
	detail := "第一行\n第二行"
	selected := false
	position := 2
	input := PanelProfileInput{DisplayName: "姓名", Seat: "A-01", Title: "今日安排", WeatherOverride: "更正天气", TaskOverrides: map[string]persistence.PanelTaskOverride{"logical-1": {Title: &title, Detail: &detail, Selected: &selected, Position: &position}}}
	updated, err := service.PutProfile(ctx, a, initial.Revision, input)
	if err != nil || updated.Revision != 2 || updated.Seat != "A-01" || updated.TaskOverrides["logical-1"].Selected == nil || *updated.TaskOverrides["logical-1"].Selected {
		t.Fatalf("update profile: %+v %v", updated, err)
	}
	if _, err := service.PutProfile(ctx, a, initial.Revision, PanelProfileInput{DisplayName: "stale write"}); !errors.Is(err, ErrRevision) {
		t.Fatalf("stale write accepted: %v", err)
	}
	other, err := service.Profile(ctx, b)
	if err != nil || other.DisplayName != b || other.Seat != "" || len(other.TaskOverrides) != 0 {
		t.Fatalf("another account leaked: %+v %v", other, err)
	}
	stored, err := service.Profile(ctx, a)
	if err != nil || stored.DisplayName != "姓名" || *stored.TaskOverrides["logical-1"].Title != title || *stored.TaskOverrides["logical-1"].Detail != detail {
		t.Fatalf("profile not durable: %+v %v", stored, err)
	}
	invalid := input
	invalid.TaskOverrides = map[string]persistence.PanelTaskOverride{}
	for i := 0; i < 101; i++ {
		invalid.TaskOverrides[strings.Repeat("x", i+1)] = persistence.PanelTaskOverride{}
	}
	if _, err := service.PutProfile(ctx, a, 2, invalid); !errors.Is(err, ErrValidation) {
		t.Fatalf("unbounded overrides accepted: %v", err)
	}
	if _, err := service.PutProfile(ctx, a, 2, PanelProfileInput{DisplayName: "two\nlines"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("control characters accepted: %v", err)
	}
}

func TestPanelAvatarValidationAndMetadataRemoval(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 12, 16))
	img.SetNRGBA(1, 1, color.NRGBA{R: 220, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	withTrailing := append(append([]byte{}, encoded.Bytes()...), []byte("private metadata should disappear")...)
	valid := "data:image/png;base64," + base64.StdEncoding.EncodeToString(withTrailing)
	clean, err := normalizePanelAvatar(valid)
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(strings.SplitN(clean, ",", 2)[1])
	if err != nil || bytes.Contains(data, []byte("private metadata")) {
		t.Fatalf("metadata not stripped: %v", err)
	}
	decoded, format, err := image.Decode(bytes.NewReader(data))
	if err != nil || format != "png" || decoded.Bounds() != img.Bounds() || decoded.At(1, 1) != img.At(1, 1) {
		t.Fatalf("normalization changed photo: %v", err)
	}
	var oversized bytes.Buffer
	if err := png.Encode(&oversized, image.NewGray(image.Rect(0, 0, 1025, 1))); err != nil {
		t.Fatal(err)
	}
	for label, value := range map[string]string{
		"mime spoof":       "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes()),
		"svg":              "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte("<svg></svg>")),
		"remote":           "https://example.com/photo.png",
		"malformed":        "data:image/png;base64,eA==",
		"large dimensions": "data:image/png;base64," + base64.StdEncoding.EncodeToString(oversized.Bytes()),
		"too many bytes":   "data:image/png;base64," + base64.StdEncoding.EncodeToString(make([]byte, maxPanelAvatarBytes+1)),
	} {
		t.Run(label, func(t *testing.T) {
			if _, err := normalizePanelAvatar(value); !errors.Is(err, ErrValidation) {
				t.Fatalf("accepted invalid image: %v", err)
			}
		})
	}
	if empty, err := normalizePanelAvatar(""); err != nil || empty != "" {
		t.Fatal("cannot clear avatar")
	}
}

func TestPanelPairingOwnerScopeExpiryAndRevocation(t *testing.T) {
	store, a, b := panelTestStore(t)
	service := NewPanelService(store)
	ctx := context.Background()
	pairing, token, err := service.CreatePairing(ctx, a, "WL14B00B1F")
	if err != nil {
		t.Fatal(err)
	}
	if pairing.TokenHash == token || pairing.TokenHash != persistence.Hash(token) || pairing.Scope != PanelSnapshotScope {
		t.Fatal("panel capability not hashed or scoped")
	}
	jsonPairing, _ := json.Marshal(pairing)
	if bytes.Contains(jsonPairing, []byte(token)) || bytes.Contains(jsonPairing, []byte(pairing.TokenHash)) {
		t.Fatal("pairing JSON leaks credentials")
	}
	if userID, err := service.AuthenticatePairing(ctx, token); err != nil || userID != a {
		t.Fatalf("valid pairing rejected %s %v", userID, err)
	}
	other, err := service.Pairings(ctx, b)
	if err != nil || len(other) != 0 {
		t.Fatalf("another user lists pairings: %+v %v", other, err)
	}
	if err := service.RevokePairing(ctx, b, pairing.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user revoked pairing: %v", err)
	}
	if err := store.DB.Model(&persistence.User{}).Where("id = ?", a).Update("status", "suspended").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.AuthenticatePairing(ctx, token); err == nil {
		t.Fatal("suspended user capability accepted")
	}
	if err := store.DB.Model(&persistence.User{}).Where("id = ?", a).Update("status", "active").Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB.Model(&persistence.PanelPairing{}).Where("id = ?", pairing.ID).Update("expires_at", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.AuthenticatePairing(ctx, token); err == nil {
		t.Fatal("expired capability accepted")
	}
	if err := store.DB.Model(&persistence.PanelPairing{}).Where("id = ?", pairing.ID).Update("expires_at", time.Now().Add(time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.RevokePairing(ctx, a, pairing.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.RevokePairing(ctx, a, pairing.ID); err != nil {
		t.Fatal("revocation not idempotent", err)
	}
	if _, err := service.AuthenticatePairing(ctx, token); err == nil {
		t.Fatal("revoked capability accepted")
	}
	if _, err := service.AuthenticatePairing(ctx, "ftp_"+strings.Repeat("A", 43)); err == nil {
		t.Fatal("invented capability accepted")
	}
}
