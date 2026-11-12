package auth

import (
	"context"
	"errors"
	fastcas "github.com/FastR-D/FastCAS/sdk/go"
	"github.com/FastR-D/FastTask/internal/persistence"
	"testing"
)

func TestCASPromotesExistingMemberAndKeepsLocalAdmin(t *testing.T) {
	store, s := casFixture(t)
	ctx := context.Background()
	user, _, _, err := s.Login(ctx, "admin", "local-password-for-test")
	if err != nil {
		t.Fatal(err)
	}
	store.DB.Model(&user).Update("role", "member")
	link := fastcas.Link{ID: "promotion", ClientID: "task", LocalRef: user.ID, Subject: "cas-admin", State: "active", Version: 1}
	row := localLink(s.config.FastCASIssuer, link)
	if err = store.DB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	identity := fastcas.Identity{Issuer: s.config.FastCASIssuer, Subject: link.Subject, Role: "admin"}
	result, err := s.loginFastCAS(ctx, identity, link)
	if err != nil || result.User.Role != "admin" {
		t.Fatal(result.User.Role, err)
	}
	identity.Role = "member"
	result, err = s.loginFastCAS(ctx, identity, link)
	if err != nil || result.User.Role != "admin" {
		t.Fatal("demoted", err)
	}
	store.DB.Model(&user).Update("status", "disabled")
	if _, err = s.loginFastCAS(ctx, identity, link); err == nil {
		t.Fatal("disabled account accepted")
	}
}
func TestCASRegistrationPolicyAndBrowserProof(t *testing.T) {
	store, s := casFixture(t)
	ctx := context.Background()
	binding := fastcas.RandomBinding()
	identity := fastcas.Identity{Issuer: s.config.FastCASIssuer, Subject: "new", Email: "new@example.test", EmailVerified: true}
	missing := &fastcas.APIError{Status: 401, Code: "authentication_required"}
	if _, err := s.promptFastCASRegistration(ctx, binding, identity, errors.New("provider down")); err == nil {
		t.Fatal("outage became signup")
	}
	result, err := s.promptFastCASRegistration(ctx, binding, identity, missing)
	if err != nil || !result.RegistrationRequired {
		t.Fatal(result, err)
	}
	if _, err = s.FastCASRegistrationStatus(ctx, "wrong-browser"); err == nil {
		t.Fatal("wrong browser accepted")
	}
	if _, _, err = s.BeginFastCASRegistration(ctx, binding, FastCASRegistrationInput{Identifier: "admin", Password: "valid-password-long", DisplayName: "Existing"}); err == nil {
		t.Fatal("existing account merged")
	}
	var count int64
	store.DB.Model(&persistence.User{}).Count(&count)
	if count != 1 {
		t.Fatal("unexpected users", count)
	}
	s.config.FastCASLinkDisabled = true
	if _, err = s.FastCASRegistrationStatus(ctx, binding); err == nil {
		t.Fatal("disabled signup accepted")
	}
}
