package auth

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	fastcas "github.com/FastR-D/FastCAS/sdk/go"
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
	ClientID  string
	Role      string
	Scopes    []string
}

type Service struct {
	store     *persistence.Store
	config    config.Config
	mu        sync.Mutex
	throttle  *loginThrottle
	casClient *fastcas.Client
}

// ErrLoginThrottled is returned while a client or account is cooling down after repeated failures.
var ErrLoginThrottled = errors.New("login temporarily blocked")

// ErrInvalidCredentials covers both an unknown identifier and a wrong password; callers must not be
// able to tell them apart.
var ErrInvalidCredentials = errors.New("invalid credentials")

type claims struct {
	Role string `json:"role"`
	SID  string `json:"sid"`
	jwt.RegisteredClaims
}

type serviceClaims struct {
	Scopes   []string `json:"scopes"`
	UserID   string   `json:"represented_user_id"`
	ClientID string   `json:"client_id"`
	jwt.RegisteredClaims
}

func New(store *persistence.Store, cfg config.Config) *Service {
	return &Service{store: store, config: cfg, throttle: newLoginThrottle()}
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
	ticket, ok := s.throttle.begin(identifier, clientIPFromContext(ctx))
	if !ok {
		return persistence.User{}, "", "", ErrLoginThrottled
	}
	var user persistence.User
	err := s.store.DB.WithContext(ctx).Where("identifier = ? AND status = 'active'", identifier).First(&user).Error
	if err != nil && !persistence.IsNotFound(err) {
		return persistence.User{}, "", "", err
	}
	// An unknown identifier costs the same Argon2id work as a wrong password, so response time does not
	// reveal which accounts exist.
	encoded := user.PasswordHash
	if err != nil {
		encoded = dummyPasswordHash()
	}
	if !VerifyPassword(password, encoded) || err != nil {
		return persistence.User{}, "", "", ErrInvalidCredentials
	}
	s.throttle.succeed(ticket)
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
		if user.Status != "active" {
			return errors.New("user is disabled")
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
			AuthSource: session.AuthSource, CasIssuer: session.CasIssuer, CasSID: session.CasSID, CasLinkID: session.CasLinkID, CasLinkVersion: session.CasLinkVersion,
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
	var user persistence.User
	if err := s.store.DB.First(&user, "id = ? AND status = 'active'", c.Subject).Error; err != nil {
		return Principal{}, errors.New("user is disabled")
	}
	return Principal{UserID: c.Subject, SessionID: c.SID, Role: user.Role}, nil
}

func (s *Service) AuthenticatePanel(token string) (Principal, error) {
	return s.AuthenticateService(token, "panel:summary:read")
}

func (s *Service) AuthenticateService(token, requiredScope string) (Principal, error) {
	principal, err := s.AuthenticateServicePrincipal(token)
	if err != nil {
		return Principal{}, err
	}
	if !HasScopes(principal, requiredScope) {
		return Principal{}, errors.New("missing service scope")
	}
	return principal, nil
}

func (s *Service) AuthenticateServicePrincipal(token string) (Principal, error) {
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
	clientID := strings.TrimSpace(claims.ClientID)
	if clientID == "" {
		clientID = strings.TrimSpace(claims.Subject)
	}
	if clientID == "" {
		return Principal{}, errors.New("missing service client identity")
	}
	return Principal{UserID: claims.UserID, ClientID: clientID, Role: "service", Scopes: claims.Scopes}, nil
}

func HasScopes(principal Principal, required ...string) bool {
	available := make(map[string]bool, len(principal.Scopes))
	for _, scope := range principal.Scopes {
		available[scope] = true
	}
	for _, scope := range required {
		if !available[scope] {
			return false
		}
	}
	return true
}

func (s *Service) IssuePanelToken(userID string, ttl time.Duration) (string, error) {
	return s.IssueServiceTokenForClient(userID, "fastresearch-panel", []string{"panel:summary:read"}, ttl)
}

func (s *Service) IssueServiceToken(userID string, scopes []string, ttl time.Duration) (string, error) {
	return s.IssueServiceTokenForClient(userID, "fastresearch-service", scopes, ttl)
}

func (s *Service) IssueServiceTokenForClient(userID, clientID string, scopes []string, ttl time.Duration) (string, error) {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return "", errors.New("service client identity is required")
	}
	now := persistence.Now()
	claims := serviceClaims{Scopes: scopes, UserID: userID, ClientID: clientID, RegisteredClaims: jwt.RegisteredClaims{Issuer: "fasttask-panel", Subject: clientID, Audience: jwt.ClaimStrings{"fasttask-panel-api"}, ExpiresAt: jwt.NewNumericDate(now.Add(ttl)), IssuedAt: jwt.NewNumericDate(now), ID: persistence.NewID("servicejwt")}}
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

// Argon2id with these parameters holds 64 MiB for the duration of one hash. Unbounded, a burst of login
// requests is a memory-exhaustion attack that needs no valid account, so hashing is gated by a small
// fixed number of slots; waiting callers hold a goroutine, not 64 MiB.
const (
	argonMemoryKiB   = 64 * 1024
	argonIterations  = 1
	argonParallelism = 4
	// maxPasswordBytes bounds the input. Argon2 pre-hashes it, so length is not a work factor, but
	// nothing legitimate is this long and a bound keeps a request body from becoming a password.
	maxPasswordBytes = 1024
)

var argonSlots = make(chan struct{}, max(2, min(runtime.NumCPU(), 4)))

func argon2Key(password, salt []byte, iterations, memory uint32, parallelism uint8, length uint32) []byte {
	argonSlots <- struct{}{}
	defer func() { <-argonSlots }()
	return argon2.IDKey(password, salt, iterations, memory, parallelism, length)
}

var (
	dummyHashOnce sync.Once
	dummyHash     string
)

// dummyPasswordHash is a valid encoded hash of a random password nobody knows, verified in place of a
// real one when the identifier does not exist.
func dummyPasswordHash() string {
	dummyHashOnce.Do(func() {
		secret, err := randomToken(32)
		if err == nil {
			dummyHash, err = HashPassword(secret)
		}
		if err != nil {
			// Without a random source there is nothing secure to fall back to; a fixed, well-formed
			// hash still keeps the timing equal and matches no password.
			dummyHash = "argon2id$v=19$m=65536,t=1,p=4$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		}
	})
	return dummyHash
}

func HashPassword(password string) (string, error) {
	if len(password) < 12 {
		return "", errors.New("password must contain at least 12 characters")
	}
	if len(password) > maxPasswordBytes {
		return "", fmt.Errorf("password must not exceed %d bytes", maxPasswordBytes)
	}
	salt := make([]byte, 16)
	if _, err := cryptorand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2Key([]byte(password), salt, argonIterations, argonMemoryKiB, argonParallelism, 32)
	return fmt.Sprintf("argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemoryKiB, argonIterations, argonParallelism, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

func VerifyPassword(password, encoded string) bool {
	if len(password) > maxPasswordBytes {
		return false
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" {
		return false
	}
	var memory uint32
	var iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil {
		return false
	}
	// Parameters come from the database. Refuse values no FastTask build ever wrote rather than let a
	// damaged row turn one login into a multi-gigabyte allocation.
	if memory == 0 || memory > 1024*1024 || iterations == 0 || iterations > 16 || parallelism == 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(want) < 16 || len(want) > 64 {
		return false
	}
	got := argon2Key([]byte(password), salt, iterations, memory, parallelism, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func randomToken(bytes int) (string, error) {
	buffer := make([]byte, bytes)
	if _, err := cryptorand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}
