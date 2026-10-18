package auth

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

var ErrFastResearchDisabled = errors.New("FastResearch login is not configured")

type fastResearchIdentity struct {
	AccountID string `json:"accountId"`
	KeyID     string `json:"keyId"`
	Person    string `json:"person"`
}

type FastResearchResult struct {
	User         persistence.User `json:"user"`
	AccessToken  string           `json:"access_token"`
	RefreshToken string           `json:"refresh_token"`
}

// LoginFastResearch consumes the one-time ticket on the server and maps the stable
// FastResearch account ID to one local user. The personal Key never reaches FastTask.
func (s *Service) LoginFastResearch(ctx context.Context, ticket string) (FastResearchResult, error) {
	if !s.config.FastResearchLoginEnabled {
		return FastResearchResult{}, ErrFastResearchDisabled
	}
	ticket = strings.TrimSpace(ticket)
	if len(ticket) < 24 || len(ticket) > 512 {
		return FastResearchResult{}, errors.New("invalid FastResearch ticket")
	}
	body, _ := json.Marshal(map[string]string{"ticket": ticket, "audience": "fast-task"})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.FastResearchURL+"/api/sso/consume", bytes.NewReader(body))
	if err != nil {
		return FastResearchResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-FastTask-SSO-Secret", s.config.FastResearchSSOSecret)
	client := &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return FastResearchResult{}, fmt.Errorf("FastResearch ticket exchange failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return FastResearchResult{}, errors.New("FastResearch ticket was rejected")
	}
	var identity fastResearchIdentity
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&identity); err != nil {
		return FastResearchResult{}, errors.New("invalid FastResearch identity response")
	}
	identity.AccountID = strings.TrimSpace(identity.AccountID)
	identity.KeyID = strings.TrimSpace(identity.KeyID)
	identity.Person = strings.TrimSpace(identity.Person)
	if identity.AccountID == "" || len(identity.AccountID) > 200 || identity.KeyID == "" || len(identity.KeyID) > 200 {
		return FastResearchResult{}, errors.New("incomplete FastResearch identity")
	}
	return s.issueFastResearchSession(ctx, identity)
}

// CompareFastResearchSecret checks the shared server credential without a timing leak.
func (s *Service) CompareFastResearchSecret(candidate string) bool {
	if !s.config.FastResearchLoginEnabled || len(candidate) != len(s.config.FastResearchSSOSecret) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(s.config.FastResearchSSOSecret)) == 1
}

func (s *Service) issueFastResearchSession(ctx context.Context, identity fastResearchIdentity) (FastResearchResult, error) {
	var user persistence.User
	refresh, err := randomToken(32)
	if err != nil {
		return FastResearchResult{}, err
	}
	now := persistence.Now()
	err = s.store.Transaction(ctx, func(tx *gorm.DB) error {
		var mapping persistence.FastResearchAccount
		err := tx.First(&mapping, "account_id = ?", identity.AccountID).Error
		if err != nil && !persistence.IsNotFound(err) {
			return err
		}
		if persistence.IsNotFound(err) {
			user = persistence.User{
				ID: persistence.NewID("user"), Identifier: "fastresearch:" + identity.AccountID,
				DisplayName: identity.Person, Timezone: "Asia/Shanghai", Locale: "zh-CN",
				Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now,
			}
			if user.DisplayName == "" {
				user.DisplayName = "FastResearch 用户"
			}
			if err := tx.Create(&user).Error; err != nil {
				return err
			}
			mapping = persistence.FastResearchAccount{AccountID: identity.AccountID, UserID: user.ID, KeyID: identity.KeyID, Person: identity.Person, CreatedAt: now, UpdatedAt: now}
			if err := tx.Create(&mapping).Error; err != nil {
				return err
			}
		} else {
			if err := tx.First(&user, "id = ?", mapping.UserID).Error; err != nil {
				return err
			}
			if user.Status != "active" {
				return errors.New("FastTask account is disabled")
			}
			updates := map[string]any{"key_id": identity.KeyID, "person": identity.Person, "updated_at": now}
			if identity.Person != "" && user.DisplayName != identity.Person {
				if err := tx.Model(&user).Updates(map[string]any{"display_name": identity.Person, "revision": user.Revision + 1, "updated_at": now}).Error; err != nil {
					return err
				}
				user.DisplayName, user.Revision, user.UpdatedAt = identity.Person, user.Revision+1, now
			}
			if err := tx.Model(&mapping).Updates(updates).Error; err != nil {
				return err
			}
		}
		session := persistence.Session{
			ID: persistence.NewID("session"), FamilyID: persistence.NewID("family"), UserID: user.ID,
			RefreshHash: persistence.Hash(refresh), Status: "active", ExpiresAt: now.Add(s.config.RefreshTTL),
			CreatedAt: now, UpdatedAt: now, AuthSource: "fastresearch",
		}
		return tx.Create(&session).Error
	})
	if err != nil {
		return FastResearchResult{}, err
	}
	// Access tokens are tied to a session ID. Read the session created in the same
	// transaction by its refresh hash, then issue the normal local bearer token.
	var session persistence.Session
	if err := s.store.DB.WithContext(ctx).First(&session, "refresh_hash = ?", persistence.Hash(refresh)).Error; err != nil {
		return FastResearchResult{}, err
	}
	access, err := s.issueAccess(user, session.ID)
	if err != nil {
		return FastResearchResult{}, err
	}
	return FastResearchResult{User: user, AccessToken: access, RefreshToken: refresh}, nil
}
