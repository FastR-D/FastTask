package auth

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/persistence"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/argon2"
	"gorm.io/gorm"
)

type principalKey struct{}

type Principal struct {
	UserID    string
	SessionID string
	Role      string
	Scopes    []string
}

type Service struct {
	store    *persistence.Store
	config   config.Config
	mu       sync.Mutex
	failures map[string]loginFailure
}

type loginFailure struct {
	Count        int
	BlockedUntil time.Time
}

type claims struct {
	Role string `json:"role"`
	SID  string `json:"sid"`
	jwt.RegisteredClaims
}

type serviceClaims struct {
	Scopes []string `json:"scopes"`
	UserID string   `json:"represented_user_id"`
	jwt.RegisteredClaims
}

func New(store *persistence.Store, cfg config.Config) *Service {
	return &Service{store: store, config: cfg, failures: map[string]loginFailure{}}
}

func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, principal)
}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalKey{}).(Principal)
	return principal, ok
}

func (s *Service) EnsureAdmin(ctx context.Context) error {
	var count int64
	if err := s.store.DB.WithContext(ctx).Model(&persistence.User{}).Where("identifier = ?", s.config.AdminIdentifier).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	hash, err := HashPassword(s.config.AdminPassword)
	if err != nil {
		return err
	}
	now := persistence.Now()
	user := persistence.User{
		ID: persistence.NewID("user"), Identifier: s.config.AdminIdentifier,
		PasswordHash: hash, DisplayName: s.config.AdminName, Timezone: "Asia/Shanghai",
		Locale: "zh-CN", Role: "admin", Status: "active", Revision: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	return s.store.DB.WithContext(ctx).Create(&user).Error
}

func (s *Service) Login(ctx context.Context, identifier, password string) (persistence.User, string, string, error) {
	identifier = strings.TrimSpace(identifier)
	if s.loginBlocked(identifier) {
		return persistence.User{}, "", "", errors.New("login temporarily blocked")
	}
	var user persistence.User
	if err := s.store.DB.WithContext(ctx).Where("identifier = ? AND status = 'active'", identifier).First(&user).Error; err != nil {
		s.recordLoginFailure(identifier)
		return user, "", "", errors.New("invalid credentials")
	}
	if !VerifyPassword(password, user.PasswordHash) {
		s.recordLoginFailure(identifier)
		return user, "", "", errors.New("invalid credentials")
	}
	s.clearLoginFailures(identifier)
	refresh, err := randomToken(32)
	if err != nil {
		return user, "", "", err
	}
	now := persistence.Now()
	session := persistence.Session{
		ID: persistence.NewID("session"), FamilyID: persistence.NewID("family"), UserID: user.ID,
		RefreshHash: persistence.Hash(refresh), Status: "active", ExpiresAt: now.Add(s.config.RefreshTTL),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.DB.WithContext(ctx).Create(&session).Error; err != nil {
		return user, "", "", err
	}
	access, err := s.issueAccess(user, session.ID)
	return user, access, refresh, err
}

func (s *Service) loginBlocked(identifier string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failures[identifier].BlockedUntil.After(persistence.Now())
}

func (s *Service) recordLoginFailure(identifier string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	failure := s.failures[identifier]
	failure.Count++
	if failure.Count >= 5 {
		failure.BlockedUntil = persistence.Now().Add(time.Duration(failure.Count-4) * 30 * time.Second)
	}
	s.failures[identifier] = failure
}

func (s *Service) clearLoginFailures(identifier string) {
	s.mu.Lock()
	delete(s.failures, identifier)
	s.mu.Unlock()
}

func (s *Service) Refresh(ctx context.Context, refreshToken string) (persistence.User, string, string, error) {
	var user persistence.User
	var session persistence.Session
	var reused bool
	hash := persistence.Hash(refreshToken)
	err := s.store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("refresh_hash = ?", hash).First(&session).Error; err != nil {
			return errors.New("invalid refresh token")
		}
		if session.Status != "active" || session.ExpiresAt.Before(persistence.Now()) {
			if session.ReplacedByHash != "" {
				if err := tx.Model(&persistence.Session{}).Where("family_id = ?", session.FamilyID).Updates(map[string]any{"status": "revoked", "updated_at": persistence.Now()}).Error; err != nil {
					return err
				}
				reused = true
				return nil
			}
			return errors.New("invalid refresh token")
		}
		if err := tx.First(&user, "id = ?", session.UserID).Error; err != nil {
			return err
		}
		newRefresh, err := randomToken(32)
		if err != nil {
			return err
		}
		now := persistence.Now()
		newSession := persistence.Session{
			ID: persistence.NewID("session"), FamilyID: session.FamilyID, UserID: session.UserID,
			RefreshHash: persistence.Hash(newRefresh), Status: "active", ExpiresAt: now.Add(s.config.RefreshTTL),
			CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.Create(&newSession).Error; err != nil {
			return err
		}
		if err := tx.Model(&session).Updates(map[string]any{"status": "rotated", "replaced_by_hash": newSession.RefreshHash, "updated_at": now}).Error; err != nil {
			return err
		}
		session = newSession
		refreshToken = newRefresh
		return nil
	})
	if err != nil {
		return user, "", "", err
	}
	if reused {
		return user, "", "", errors.New("refresh token reuse detected")
	}
	access, err := s.issueAccess(user, session.ID)
	return user, access, refreshToken, err
}

func (s *Service) Logout(ctx context.Context, sessionID string) error {
	return s.store.DB.WithContext(ctx).Model(&persistence.Session{}).Where("id = ?", sessionID).Updates(map[string]any{"status": "revoked", "updated_at": persistence.Now()}).Error
}

func (s *Service) Authenticate(token string) (Principal, error) {
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	if token == "" {
		return Principal{}, errors.New("missing bearer token")
	}
	parsed, err := jwt.ParseWithClaims(token, &claims{}, func(token *jwt.Token) (any, error) {
		if token.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, errors.New("unexpected signing algorithm")
		}
		return []byte(s.config.JWTSecret), nil
	}, jwt.WithIssuer("fasttask"), jwt.WithAudience("fasttask-web"))
	if err != nil || !parsed.Valid {
		return Principal{}, errors.New("invalid bearer token")
	}
	c, ok := parsed.Claims.(*claims)
	if !ok {
		return Principal{}, errors.New("invalid claims")
	}
	var count int64
	if err := s.store.DB.Model(&persistence.Session{}).Where("id = ? AND user_id = ? AND status = 'active' AND expires_at > ?", c.SID, c.Subject, persistence.Now()).Count(&count).Error; err != nil || count != 1 {
		return Principal{}, errors.New("session revoked")
	}
	return Principal{UserID: c.Subject, SessionID: c.SID, Role: c.Role}, nil
}

func (s *Service) AuthenticatePanel(token string) (Principal, error) {
	return s.AuthenticateService(token, "panel:summary:read")
}

func (s *Service) AuthenticateService(token, requiredScope string) (Principal, error) {
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	if token == "" {
		return Principal{}, errors.New("missing service token")
	}
	parsed, err := jwt.ParseWithClaims(token, &serviceClaims{}, func(token *jwt.Token) (any, error) {
		if token.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, errors.New("unexpected signing algorithm")
		}
		return []byte(s.config.PanelJWTSecret), nil
	}, jwt.WithIssuer("fasttask-panel"), jwt.WithAudience("fasttask-panel-api"))
	if err != nil || !parsed.Valid {
		return Principal{}, errors.New("invalid service token")
	}
	claims, ok := parsed.Claims.(*serviceClaims)
	if !ok || claims.UserID == "" {
		return Principal{}, errors.New("invalid service claims")
	}
	hasScope := false
	for _, scope := range claims.Scopes {
		if scope == requiredScope {
			hasScope = true
		}
	}
	if !hasScope {
		return Principal{}, errors.New("missing service scope")
	}
	return Principal{UserID: claims.UserID, Role: "service", Scopes: claims.Scopes}, nil
}

func (s *Service) IssuePanelToken(userID string, ttl time.Duration) (string, error) {
	return s.IssueServiceToken(userID, []string{"panel:summary:read"}, ttl)
}

func (s *Service) IssueServiceToken(userID string, scopes []string, ttl time.Duration) (string, error) {
	now := persistence.Now()
	claims := serviceClaims{Scopes: scopes, UserID: userID, RegisteredClaims: jwt.RegisteredClaims{Issuer: "fasttask-panel", Subject: "fastresearch-service", Audience: jwt.ClaimStrings{"fasttask-panel-api"}, ExpiresAt: jwt.NewNumericDate(now.Add(ttl)), IssuedAt: jwt.NewNumericDate(now), ID: persistence.NewID("servicejwt")}}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(s.config.PanelJWTSecret))
}

func (s *Service) issueAccess(user persistence.User, sessionID string) (string, error) {
	now := persistence.Now()
	c := claims{Role: user.Role, SID: sessionID, RegisteredClaims: jwt.RegisteredClaims{
		Issuer: "fasttask", Subject: user.ID, Audience: jwt.ClaimStrings{"fasttask-web"},
		ExpiresAt: jwt.NewNumericDate(now.Add(s.config.AccessTTL)), IssuedAt: jwt.NewNumericDate(now),
		ID: persistence.NewID("jwt"),
	}}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(s.config.JWTSecret))
}

func HashPassword(password string) (string, error) {
	if len(password) < 8 {
		return "", errors.New("password must contain at least 8 characters")
	}
	salt := make([]byte, 16)
	if _, err := cryptorand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, 1, 64*1024, 4, 32)
	return fmt.Sprintf("argon2id$v=19$m=65536,t=1,p=4$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

func VerifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 {
		return false
	}
	var memory uint32
	var iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, uint32(len(want)))
	return subtleEqual(got, want)
}

func subtleEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var result byte
	for i := range a {
		result |= a[i] ^ b[i]
	}
	return result == 0
}

func randomToken(bytes int) (string, error) {
	buffer := make([]byte, bytes)
	if _, err := cryptorand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}
