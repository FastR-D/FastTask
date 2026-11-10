package auth

import (
	"errors"
	"testing"

	fastcas "github.com/FastR-D/FastCAS/sdk/go"
)

func TestValidateFastCASServiceClaims(t *testing.T) {
	allowed := []string{"fastresearch"}
	if err := validateFastCASServiceClaims(&fastcas.AccessClaims{Subject: "fastresearch", ClientID: "fastresearch", Service: true}, allowed); err != nil {
		t.Fatalf("valid service claims rejected: %v", err)
	}
	for name, claims := range map[string]*fastcas.AccessClaims{
		"user token":       {Subject: "fastresearch", ClientID: "fastresearch"},
		"subject mismatch": {Subject: "another-subject", ClientID: "fastresearch", Service: true},
		"unlisted client":  {Subject: "other", ClientID: "other", Service: true},
		"missing client":   {Subject: "fastresearch", Service: true},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateFastCASServiceClaims(claims, allowed); !errors.Is(err, ErrFastCASServiceDenied) {
				t.Fatalf("claims accepted or wrong error: %v", err)
			}
		})
	}
}
