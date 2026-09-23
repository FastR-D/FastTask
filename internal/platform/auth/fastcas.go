package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"time"

	fastcas "github.com/FastR-D/FastCAS/sdk/go"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

var ErrFastCASDisabled = errors.New("FastCAS is not configured")

// FastCASTransactions uses the project database, including across process restarts.
type FastCASTransactions struct{ Store *persistence.Store }

func (s FastCASTransactions) Put(ctx context.Context, t fastcas.Transaction) error {
	payload, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("expires_at <= ?", persistence.Now()).Delete(&persistence.FastCASTransaction{}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.FastCASTransaction{State: t.State, Payload: string(payload), ExpiresAt: t.ExpiresAt}).Error
	})
}
func (s FastCASTransactions) Take(ctx context.Context, state string) (*fastcas.Transaction, error) {
	var result *fastcas.Transaction
	err := s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		result = nil
		var row persistence.FastCASTransaction
		err := tx.First(&row, "state = ?", state).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		deleted := tx.Delete(&row)
		if deleted.Error != nil {
			return deleted.Error
		}
		if deleted.RowsAffected != 1 {
			return nil
		}
		if !row.ExpiresAt.After(persistence.Now()) {
			return nil
		}
		var value fastcas.Transaction
		if err = json.Unmarshal([]byte(row.Payload), &value); err != nil {
			return err
		}
		result = &value
		return nil
	})
	return result, err
}

func (s *Service) FastCASClient() (*fastcas.Client, error) {
	if s.config.FastCASIssuer == "" {
		return nil, ErrFastCASDisabled
	}
	// Client construction is lazy and makes no network call. Cache discovery/JWKS.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.casClient != nil {
		return s.casClient, nil
	}
	c, err := fastcas.New(fastcas.Config{Issuer: s.config.FastCASIssuer, ClientID: s.config.FastCASClientID, ClientSecret: s.config.FastCASClientSecret, RedirectURI: s.config.FastCASRedirectURI, AllowLoopbackHTTP: s.config.FastCASLoopbackHTTP}, FastCASTransactions{s.store})
	if err == nil {
		s.casClient = c
	}
	return c, err
}

func (s *Service) ProveFastCASLocal(ctx context.Context, p Principal, password string) error {
	var user persistence.User
	if err := s.store.DB.WithContext(ctx).First(&user, "id = ? AND status = 'active'", p.UserID).Error; err != nil {
		return errors.New("local account unavailable")
	}
	if s.loginBlocked(user.Identifier) {
		return errors.New("login temporarily blocked")
	}
	if !VerifyPassword(password, user.PasswordHash) {
		s.recordLoginFailure(user.Identifier)
		return errors.New("confirm your local password")
	}
	var count int64
	if err := s.store.DB.WithContext(ctx).Model(&persistence.Session{}).Where("id = ? AND user_id = ? AND status = 'active' AND expires_at > ?", p.SessionID, p.UserID, persistence.Now()).Count(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return errors.New("local session expired")
	}
	s.clearLoginFailures(user.Identifier)
	return nil
}

func (s *Service) BeginFastCAS(ctx context.Context, p *Principal, password, binding string) (string, error) {
	c, err := s.FastCASClient()
	if err != nil {
		return "", err
	}
	options := fastcas.BeginOptions{BrowserBinding: binding, ReturnTo: "/"}
	if p == nil {
		return c.BeginLogin(ctx, options)
	}
	if err = s.ProveFastCASLocal(ctx, *p, password); err != nil {
		return "", err
	}
	options.LocalAccountRef = p.UserID
	options.LocalSessionID = p.SessionID
	return c.BeginLink(ctx, options)
}

type FastCASResult struct {
	User         persistence.User `json:"user"`
	AccessToken  string           `json:"access_token"`
	RefreshToken string           `json:"refresh_token"`
	Linked       bool             `json:"linked"`
}

func (s *Service) CompleteFastCAS(ctx context.Context, callback, binding string, p *Principal) (FastCASResult, error) {
	c, err := s.FastCASClient()
	if err != nil {
		return FastCASResult{}, err
	}
	options := fastcas.FinishOptions{BrowserBinding: binding}
	if p != nil {
		options.LocalAccountRef = p.UserID
		options.LocalSessionID = p.SessionID
		// The SPA restores its local access token after the redirect. A refresh
		// rotates the physical session ID but preserves the authenticated family.
		if u, e := url.Parse(callback); e == nil {
			var row persistence.FastCASTransaction
			if e = s.store.DB.WithContext(ctx).First(&row, "state = ?", u.Query().Get("state")).Error; e == nil {
				var transaction fastcas.Transaction
				if json.Unmarshal([]byte(row.Payload), &transaction) == nil && transaction.Purpose == "link" && transaction.LocalAccountRef == p.UserID {
					var original, current persistence.Session
					if s.store.DB.WithContext(ctx).First(&original, "id = ? AND user_id = ?", transaction.LocalSessionID, p.UserID).Error == nil && s.store.DB.WithContext(ctx).First(&current, "id = ? AND user_id = ? AND status = 'active' AND expires_at > ?", p.SessionID, p.UserID, persistence.Now()).Error == nil && original.FamilyID == current.FamilyID && original.FamilyID != "" {
						options.LocalSessionID = transaction.LocalSessionID
					}
				}
			}
		}
	}
	result, err := c.FinishLogin(ctx, callback, options)
	if err != nil {
		return FastCASResult{}, err
	}
	if result.Transaction.Purpose == "link" {
		if p == nil {
			return FastCASResult{}, errors.New("local account proof required")
		}
		link, err := c.PrepareLink(ctx, result)
		if err != nil {
			return FastCASResult{}, err
		}
		if link.LocalRef != p.UserID {
			return FastCASResult{}, errors.New("local account changed")
		}
		err = s.store.Transaction(ctx, func(tx *gorm.DB) error {
			var user persistence.User
			if err := tx.First(&user, "id = ? AND status = 'active'", p.UserID).Error; err != nil {
				return err
			}
			var count int64
			if err := tx.Model(&persistence.Session{}).Where("id = ? AND user_id = ? AND status = 'active' AND expires_at > ?", p.SessionID, p.UserID, persistence.Now()).Count(&count).Error; err != nil {
				return err
			}
			if count != 1 {
				return errors.New("local session changed")
			}
			row := localLink(s.config.FastCASIssuer, *link)
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			return casAudit(tx, p.UserID, "fastcas.prepare", link.ID)
		})
		if err != nil {
			return FastCASResult{}, err
		}
		link, err = c.ActivateLink(ctx, link.ID)
		if err != nil {
			return FastCASResult{}, err
		}
		if err = s.applyFastCASLink(ctx, *link, ""); err != nil {
			return FastCASResult{}, err
		}
		return FastCASResult{Linked: true}, nil
	}
	if result.Transaction.Purpose != "login" {
		return FastCASResult{}, errors.New("FastTask does not allow public registration")
	}
	var pending persistence.FastCASLink
	if err = s.store.DB.WithContext(ctx).First(&pending, "issuer = ? AND subject = ? AND state = 'prepared'", s.config.FastCASIssuer, result.Identity.Subject).Error; err == nil {
		remote, e := c.GetLink(ctx, pending.ID)
		if e != nil {
			return FastCASResult{}, e
		}
		if remote.State == "prepared" {
			remote, e = c.ActivateLink(ctx, pending.ID)
			if e != nil {
				return FastCASResult{}, e
			}
		}
		if e = s.applyFastCASLink(ctx, *remote, ""); e != nil {
			return FastCASResult{}, e
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return FastCASResult{}, err
	}
	link, err := c.ResolveLink(ctx, result.Identity.Subject)
	if err != nil {
		return FastCASResult{}, err
	}
	return s.loginFastCAS(ctx, result.Identity, *link)
}

func (s *Service) loginFastCAS(ctx context.Context, identity fastcas.Identity, link fastcas.Link) (FastCASResult, error) {
	var result FastCASResult
	refresh, err := randomToken(32)
	if err != nil {
		return result, err
	}
	err = s.store.Transaction(ctx, func(tx *gorm.DB) error {
		var local persistence.FastCASLink
		if err := tx.First(&local, "id = ? AND issuer = ? AND subject = ? AND state = 'active' AND version = ? AND user_id = ?", link.ID, identity.Issuer, identity.Subject, link.Version, link.LocalRef).Error; err != nil {
			return errors.New("authenticate your existing FastTask account first")
		}
		if err := tx.First(&result.User, "id = ? AND status = 'active'", local.UserID).Error; err != nil {
			return errors.New("local account disabled")
		}
		now := persistence.Now()
		session := persistence.Session{ID: persistence.NewID("session"), FamilyID: persistence.NewID("family"), UserID: local.UserID, RefreshHash: persistence.Hash(refresh), Status: "active", ExpiresAt: now.Add(s.config.RefreshTTL), CreatedAt: now, UpdatedAt: now, AuthSource: "fastcas", CasIssuer: identity.Issuer, CasSID: identity.SessionID, CasLinkID: link.ID, CasLinkVersion: link.Version}
		if err := tx.Create(&session).Error; err != nil {
			return err
		}
		var err error
		result.AccessToken, err = s.issueAccess(result.User, session.ID)
		if err != nil {
			return err
		}
		result.RefreshToken = refresh
		return casAudit(tx, local.UserID, "fastcas.login", link.ID)
	})
	return result, err
}

func (s *Service) FastCASLinks(ctx context.Context, p Principal) ([]persistence.FastCASLink, error) {
	rows := []persistence.FastCASLink{}
	err := s.store.DB.WithContext(ctx).Where("issuer = ? AND user_id = ?", s.config.FastCASIssuer, p.UserID).Find(&rows).Error
	return rows, err
}
func (s *Service) UpdateFastCASLink(ctx context.Context, p Principal, id, password string, revoke bool) (*fastcas.Link, error) {
	c, err := s.FastCASClient()
	if err != nil {
		return nil, err
	}
	var row persistence.FastCASLink
	if err = s.store.DB.WithContext(ctx).First(&row, "id = ? AND issuer = ? AND user_id = ?", id, s.config.FastCASIssuer, p.UserID).Error; err != nil {
		return nil, err
	}
	if revoke {
		if err = s.ProveFastCASLocal(ctx, p, password); err != nil {
			return nil, err
		}
	}
	link, err := c.GetLink(ctx, id)
	if err != nil {
		return nil, err
	}
	if revoke {
		link, err = c.RevokeLink(ctx, link)
	} else if link.State == "prepared" {
		link, err = c.ActivateLink(ctx, id)
	}
	if err != nil {
		return nil, err
	}
	return link, s.applyFastCASLink(ctx, *link, "")
}
func (s *Service) HandleFastCASEvent(ctx context.Context, raw string) error {
	c, err := s.FastCASClient()
	if err != nil {
		return err
	}
	return c.HandleNotification(ctx, raw, s.ApplyFastCASNotification)
}
func (s *Service) ApplyFastCASNotification(ctx context.Context, event fastcas.Notification) error {
	if event.Type == "account_link.revoked" {
		return s.applyFastCASLink(ctx, *event.Link, event.ID)
	}
	if event.Status == "disabled" {
		return s.applyFastCASIdentityDisable(ctx, event.ID, event.Subject)
	}
	return nil
}
func (s *Service) applyFastCASIdentityDisable(ctx context.Context, eventID, subject string) error {
	return s.store.Transaction(ctx, func(tx *gorm.DB) error {
		created := tx.Exec("INSERT OR IGNORE INTO fastcas_events(issuer,id,processed_at) VALUES(?,?,?)", s.config.FastCASIssuer, eventID, persistence.Now())
		if created.Error != nil {
			return created.Error
		}
		if created.RowsAffected == 0 {
			return nil
		}
		return tx.Model(&persistence.Session{}).Where("auth_source = 'fastcas' AND cas_issuer = ? AND cas_link_id IN (SELECT id FROM fastcas_links WHERE issuer = ? AND client_id = ? AND subject = ?)", s.config.FastCASIssuer, s.config.FastCASIssuer, s.config.FastCASClientID, subject).Updates(map[string]any{"status": "revoked", "updated_at": persistence.Now()}).Error
	})
}
func (s *Service) HandleFastCASLogout(ctx context.Context, raw string) error {
	c, err := s.FastCASClient()
	if err != nil {
		return err
	}
	notice, err := c.VerifyLogout(ctx, raw)
	if err != nil {
		return err
	}
	return s.ApplyFastCASLogout(ctx, *notice)
}
func (s *Service) ApplyFastCASLogout(ctx context.Context, notice fastcas.LogoutNotice) error {
	return s.store.Transaction(ctx, func(tx *gorm.DB) error {
		created := tx.Exec("INSERT OR IGNORE INTO fastcas_events(issuer,id,processed_at) VALUES(?,?,?)", s.config.FastCASIssuer, notice.ID, persistence.Now())
		if created.Error != nil {
			return created.Error
		}
		if created.RowsAffected == 0 {
			return nil
		}
		query := tx.Model(&persistence.Session{}).Where("auth_source = 'fastcas' AND cas_issuer = ? AND cas_link_id IN (SELECT id FROM fastcas_links WHERE issuer = ? AND client_id = ? AND subject = ?)", s.config.FastCASIssuer, s.config.FastCASIssuer, s.config.FastCASClientID, notice.Subject)
		if notice.SessionID != "" {
			query = query.Where("cas_sid = ?", notice.SessionID)
		}
		if err := query.Updates(map[string]any{"status": "revoked", "updated_at": persistence.Now()}).Error; err != nil {
			return err
		}
		return nil
	})
}
func (s *Service) applyFastCASLink(ctx context.Context, link fastcas.Link, eventID string) error {
	return s.store.Transaction(ctx, func(tx *gorm.DB) error {
		if eventID != "" {
			created := tx.Exec("INSERT OR IGNORE INTO fastcas_events(issuer,id,processed_at) VALUES(?,?,?)", s.config.FastCASIssuer, eventID, persistence.Now())
			if created.Error != nil {
				return created.Error
			}
			if created.RowsAffected == 0 {
				return nil
			}
		}
		updated := tx.Model(&persistence.FastCASLink{}).Where("id = ? AND issuer = ? AND subject = ? AND user_id = ? AND version <= ? AND state != 'revoked'", link.ID, s.config.FastCASIssuer, link.Subject, link.LocalRef, link.Version).Updates(map[string]any{"state": link.State, "version": link.Version})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected == 0 {
			return nil
		}
		if link.State == "revoked" {
			if err := tx.Model(&persistence.Session{}).Where("auth_source = 'fastcas' AND cas_issuer = ? AND cas_link_id = ?", s.config.FastCASIssuer, link.ID).Updates(map[string]any{"status": "revoked", "updated_at": persistence.Now()}).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("processed_at < ?", persistence.Now().Add(-30*24*time.Hour)).Delete(&persistence.FastCASEvent{}).Error; err != nil {
			return err
		}
		return casAudit(tx, link.LocalRef, "fastcas."+link.State, link.ID)
	})
}
func localLink(issuer string, l fastcas.Link) persistence.FastCASLink {
	return persistence.FastCASLink{ID: l.ID, Issuer: issuer, ClientID: l.ClientID, UserID: l.LocalRef, Subject: l.Subject, State: l.State, Version: l.Version, VerifiedAt: l.VerifiedAt}
}
func casAudit(tx *gorm.DB, user, action, id string) error {
	detail, _ := json.Marshal(map[string]string{"link_id": id})
	return tx.Create(&persistence.AdminAuditEvent{ID: persistence.NewID("audit"), ActorUserID: user, Action: action, DetailJSON: string(detail), CreatedAt: persistence.Now()}).Error
}
