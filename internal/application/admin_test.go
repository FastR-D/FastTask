package application

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/FastR-D/FastTask/internal/persistence"
)

func TestCreateUserAudit(t *testing.T) {
	store, err := persistence.Open(filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	app := NewWithSecret(store, "application-test-secret-with-length")
	now := persistence.Now()
	actor := persistence.User{ID: persistence.NewID("user"), Identifier: "actor", PasswordHash: "not-used", DisplayName: "Actor", Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "admin", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&actor).Error; err != nil {
		t.Fatal(err)
	}
	created, err := app.CreateUser(context.Background(), actor, CreateUserCommand{Identifier: "member", Password: "password-for-tests", DisplayName: "Member", Role: "member"})
	if err != nil {
		t.Fatalf("create user: %#v", err)
	}
	var count int64
	if err := store.DB.Model(&persistence.AdminAuditEvent{}).Where("target_user_id = ?", created.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("audit events=%d", count)
	}
}
