package auth

import (
	"context"
	"errors"
	"slices"
	"strings"

	fastcas "github.com/FastR-D/FastCAS/sdk/go"
)

var (
	ErrFastCASServiceDisabled = errors.New("FastCAS service authentication is not configured")
	ErrFastCASServiceDenied   = errors.New("FastCAS service identity is not allowed")
)

func (s *Service) fastCASServiceClient() (*fastcas.Client, error) {
	if !s.config.FastCASServiceEnabled || s.config.FastCASServiceIssuer == "" || s.config.FastCASServiceClientID == "" {
		return nil, ErrFastCASServiceDisabled
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.casServiceClient != nil {
		return s.casServiceClient, nil
	}
	client, err := fastcas.New(fastcas.Config{
		Issuer:            s.config.FastCASServiceIssuer,
		ClientID:          s.config.FastCASServiceClientID,
		RedirectURI:       strings.TrimRight(s.config.FastCASServiceIssuer, "/") + "/console/",
		AllowLoopbackHTTP: s.config.FastCASServiceLoopbackHTTP,
	}, FastCASTransactions{s.store})
	if err == nil {
		s.casServiceClient = client
	}
	return client, err
}

func (s *Service) VerifyFastCASServiceToken(ctx context.Context, raw, scope string) (*fastcas.AccessClaims, error) {
	client, err := s.fastCASServiceClient()
	if err != nil {
		return nil, err
	}
	claims, err := client.VerifyAccessToken(ctx, raw, s.config.FastCASServiceAudience, scope)
	if err != nil {
		return nil, err
	}
	if err = validateFastCASServiceClaims(claims, s.config.FastCASServiceAllowedClientIDs); err != nil {
		return nil, err
	}
	return claims, nil
}

func validateFastCASServiceClaims(claims *fastcas.AccessClaims, allowedClientIDs []string) error {
	if claims == nil || !claims.Service || claims.Subject == "" || claims.ClientID == "" || claims.Subject != claims.ClientID || !slices.Contains(allowedClientIDs, claims.ClientID) {
		return ErrFastCASServiceDenied
	}
	return nil
}
