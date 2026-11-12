package auth

import (
	"context"
	"encoding/json"
	"errors"
	fastcas "github.com/FastR-D/FastCAS/sdk/go"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
	"strings"
	"time"
	"unicode/utf8"
)

type fastCASSignup struct {
	Binding   string    `gorm:"primaryKey" json:"-"`
	Issuer    string    `json:"-"`
	Subject   string    `json:"-"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Prepared  string    `json:"-"`
	ExpiresAt time.Time `json:"-"`
}

func (fastCASSignup) TableName() string { return "fastcas_signups" }

type FastCASRegistrationInput struct {
	Identifier  string `json:"identifier"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
}
type preparedCASUser struct {
	User         persistence.User
	PasswordHash string
}

func (s *Service) fastCASSignup(ctx context.Context, binding string) (*fastCASSignup, error) {
	if s.config.FastCASLoginDisabled || s.config.FastCASLinkDisabled {
		return nil, ErrFastCASDisabled
	}
	var p fastCASSignup
	err := s.store.DB.WithContext(ctx).First(&p, "binding = ? AND issuer = ? AND expires_at > ?", persistence.Hash(binding), s.config.FastCASIssuer, persistence.Now()).Error
	if err != nil || binding == "" {
		return nil, errors.New("FastCAS 验证已过期，请重新登录")
	}
	return &p, nil
}
func (s *Service) FastCASRegistrationStatus(ctx context.Context, binding string) (*fastCASSignup, error) {
	p, err := s.fastCASSignup(ctx, binding)
	if err != nil {
		return nil, err
	}
	if p.Prepared != "" {
		return nil, errors.New("请完成 FastCAS 认证")
	}
	return p, nil
}
func (s *Service) promptFastCASRegistration(ctx context.Context, binding string, identity fastcas.Identity, lookupError error) (FastCASResult, error) {
	var problem *fastcas.APIError
	if !errors.As(lookupError, &problem) || !(problem.Status == 404 || problem.Status == 401 && problem.Code == "authentication_required") {
		return FastCASResult{}, lookupError
	}
	if s.config.FastCASLinkDisabled {
		return FastCASResult{}, ErrFastCASDisabled
	}
	var count int64
	if err := s.store.DB.WithContext(ctx).Model(&persistence.FastCASLink{}).Where("issuer = ? AND subject = ?", s.config.FastCASIssuer, identity.Subject).Count(&count).Error; err != nil {
		return FastCASResult{}, err
	}
	if count != 0 {
		return FastCASResult{}, errors.New("原绑定已失效，请登录原账号后重新绑定")
	}
	email := ""
	if identity.EmailVerified {
		email = identity.Email
	}
	pending := fastCASSignup{Binding: persistence.Hash(binding), Issuer: s.config.FastCASIssuer, Subject: identity.Subject, Email: email, Name: identity.Name, ExpiresAt: persistence.Now().Add(5 * time.Minute)}
	err := s.store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("expires_at <= ? OR binding = ?", persistence.Now(), pending.Binding).Delete(&fastCASSignup{}).Error; err != nil {
			return err
		}
		return tx.Create(&pending).Error
	})
	return FastCASResult{RegistrationRequired: true}, err
}
func (s *Service) BeginFastCASRegistration(ctx context.Context, binding string, in FastCASRegistrationInput) (string, string, error) {
	p, err := s.FastCASRegistrationStatus(ctx, binding)
	if err != nil {
		return "", "", err
	}
	in.Identifier = strings.ToLower(strings.TrimSpace(in.Identifier))
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	if len(in.Identifier) < 3 || len(in.Identifier) > 120 || in.DisplayName == "" || utf8.RuneCountInString(in.DisplayName) > 120 {
		return "", "", errors.New("请填写有效账号和姓名")
	}
	hash, err := HashPassword(in.Password)
	if err != nil {
		return "", "", err
	}
	var count int64
	if err = s.store.DB.WithContext(ctx).Model(&persistence.User{}).Where("identifier = ?", in.Identifier).Count(&count).Error; err != nil {
		return "", "", err
	}
	if count > 0 {
		return "", "", errors.New("此账号已存在，请登录后绑定 FastCAS")
	}
	now := persistence.Now()
	user := persistence.User{ID: persistence.NewID("user"), Identifier: in.Identifier, DisplayName: in.DisplayName, Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	client, err := s.FastCASClient()
	if err != nil {
		return "", "", err
	}
	next := fastcas.RandomBinding()
	target, err := client.BeginRegistration(ctx, user.ID, fastcas.BeginOptions{BrowserBinding: next, ReturnTo: "/"})
	if err != nil {
		return "", "", err
	}
	prepared, _ := json.Marshal(preparedCASUser{user, hash})
	update := s.store.DB.WithContext(ctx).Model(&fastCASSignup{}).Where("binding = ? AND prepared = '' AND expires_at > ?", p.Binding, persistence.Now()).Updates(map[string]any{"binding": persistence.Hash(next), "prepared": string(prepared)})
	if update.Error != nil {
		return "", "", update.Error
	}
	if update.RowsAffected != 1 {
		return "", "", errors.New("请重新使用 FastCAS 登录")
	}
	return next, target, nil
}
func (s *Service) finishFastCASRegistration(ctx context.Context, binding string, result *fastcas.LoginResult) (FastCASResult, error) {
	p, err := s.fastCASSignup(ctx, binding)
	if err != nil {
		return FastCASResult{}, err
	}
	var prepared preparedCASUser
	if json.Unmarshal([]byte(p.Prepared), &prepared) != nil || p.Subject != result.Identity.Subject || prepared.User.ID != result.Transaction.LocalAccountRef {
		return FastCASResult{}, errors.New("FastCAS 身份已改变，请重新登录")
	}
	client, err := s.FastCASClient()
	if err != nil {
		return FastCASResult{}, err
	}
	link, err := client.PrepareLink(ctx, result)
	if err != nil {
		return FastCASResult{}, err
	}
	if link.LocalRef != prepared.User.ID || link.Subject != p.Subject || link.ClientID != s.config.FastCASClientID {
		return FastCASResult{}, errors.New("认证关系不匹配")
	}
	prepared.User.PasswordHash = prepared.PasswordHash
	err = s.store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Create(&prepared.User).Error; err != nil {
			return err
		}
		row := localLink(s.config.FastCASIssuer, *link)
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		if err := tx.Delete(p).Error; err != nil {
			return err
		}
		return casAudit(tx, prepared.User.ID, "fastcas.register", link.ID)
	})
	if err != nil {
		return FastCASResult{}, err
	}
	link, err = client.ActivateLink(ctx, link.ID)
	if err != nil {
		return FastCASResult{}, err
	}
	if err = s.applyFastCASLink(ctx, *link, ""); err != nil {
		return FastCASResult{}, err
	}
	return s.loginFastCAS(ctx, result.Identity, *link)
}
func promoteCASAdmin(tx *gorm.DB, userID string, identity fastcas.Identity) error {
	if identity.Role != "admin" {
		return nil
	}
	return tx.Model(&persistence.User{}).Where("id = ? AND status = 'active' AND role <> 'admin'", userID).Updates(map[string]any{"role": "admin", "revision": gorm.Expr("revision + 1"), "updated_at": persistence.Now()}).Error
}
