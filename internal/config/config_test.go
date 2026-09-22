package config

import (
	"strings"
	"testing"
)

// setEnv gives a test a clean, valid base configuration and then applies overrides. Every variable Load
// reads is set explicitly, so a developer's own environment cannot change what a test asserts.
func setEnv(t *testing.T, overrides map[string]string) {
	t.Helper()
	base := map[string]string{
		"FASTTASK_ENV":                     "development",
		"FASTTASK_JWT_SECRET":              strings.Repeat("jwt-secret-", 4),
		"FASTTASK_ADMIN_PASSWORD":          "not-the-default-password",
		"FASTTASK_PROVIDER_ENCRYPTION_KEY": strings.Repeat("provider-key-", 3),
		"FASTTASK_SIDECAR_ENABLED":         "false",
		"FASTTASK_SIDECAR_SPAWN":           "true",
		"FASTTASK_SIDECAR_SECRET":          "",
		"FASTTASK_SIDECAR_START_TIMEOUT":   "15s",
		"FASTTASK_AGENT_REASONING_PERSIST": "true",
		"FASTTASK_INTEGRATION_TIMEOUT_MS":  "2000",
		"FASTTASK_PORT":                    "10000",
	}
	for key, value := range base {
		t.Setenv(key, value)
	}
	for key, value := range overrides {
		t.Setenv(key, value)
	}
}

// TestSidecarSecretIsRequiredWithoutSpawning is the §21.4 deployment rule: a sidecar this process does not
// start cannot be handed a secret invented at boot, so the deployment must supply the one both sides read.
// Failing at Load is the point — a server that starts and then cannot authenticate to its own fallback host
// would refuse every sidecar run with no explanation.
func TestSidecarSecretIsRequiredWithoutSpawning(t *testing.T) {
	setEnv(t, map[string]string{"FASTTASK_SIDECAR_ENABLED": "true", "FASTTASK_SIDECAR_SPAWN": "false"})
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted an externally managed sidecar with no shared secret")
	} else if !strings.Contains(err.Error(), "FASTTASK_SIDECAR_SECRET") {
		t.Fatalf("error=%v, want one naming FASTTASK_SIDECAR_SECRET", err)
	}

	setEnv(t, map[string]string{
		"FASTTASK_SIDECAR_ENABLED": "true", "FASTTASK_SIDECAR_SPAWN": "false",
		"FASTTASK_SIDECAR_SECRET": strings.Repeat("shared-", 6),
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SidecarSpawn {
		t.Fatal("FASTTASK_SIDECAR_SPAWN=false was not read")
	}
	if cfg.SidecarSecret == "" {
		t.Fatal("the configured secret was dropped")
	}
}

// TestSidecarDefaultsAreOff keeps the ADR-0005 promise that the fallback is optional: a deployment that
// says nothing about the sidecar gets no Node dependency at all.
func TestSidecarDefaultsAreOff(t *testing.T) {
	setEnv(t, map[string]string{"FASTTASK_SIDECAR_ENABLED": "", "FASTTASK_SIDECAR_SPAWN": "", "FASTTASK_SIDECAR_SECRET": ""})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SidecarEnabled {
		t.Fatal("the sidecar defaulted to enabled")
	}
	if !cfg.SidecarSpawn {
		t.Fatal("spawn defaulted to false; §8.4 wants this process to start the host it supervises")
	}
	if cfg.SidecarStartTimeout.String() != "15s" {
		t.Fatalf("start timeout=%s, want 15s", cfg.SidecarStartTimeout)
	}
}

// TestSidecarStartTimeoutHasAFloor is §8.4's "readiness must fail startup, not run half-working": a timeout
// too short to reach a first health check would make every boot fail for the wrong reason.
func TestSidecarStartTimeoutHasAFloor(t *testing.T) {
	setEnv(t, map[string]string{"FASTTASK_SIDECAR_ENABLED": "true", "FASTTASK_SIDECAR_START_TIMEOUT": "100ms"})
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a sidecar start timeout below 1s")
	}
}
