package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen                string
	Port                  int
	PublicURL             string
	DatabasePath          string
	JWTSecret             string
	AccessTTL             time.Duration
	RefreshTTL            time.Duration
	AdminIdentifier       string
	AdminPassword         string
	AdminName             string
	WorkerInterval        time.Duration
	WebDist               string
	Environment           string
	OpenAIBaseURL         string
	OpenAIModel           string
	OpenAIAPIKey          string
	TranscriptionModel    string
	AudioDir              string
	PanelJWTSecret        string
	ProviderEncryptionKey string
	TrustedProxies        []string
	FastReadURL           string
	FastWriteURL          string
	IntegrationTimeout    time.Duration
}

func Load() (Config, error) {
	_ = loadDotEnv(".env")
	port, err := envInt("FASTTASK_PORT", 10000)
	if err != nil {
		return Config{}, err
	}
	integrationTimeoutMS, err := envInt("FASTTASK_INTEGRATION_TIMEOUT_MS", 2000)
	if err != nil {
		return Config{}, err
	}
	c := Config{
		Listen:                env("FASTTASK_LISTEN", "127.0.0.1"),
		Port:                  port,
		PublicURL:             env("FASTTASK_PUBLIC_URL", fmt.Sprintf("http://127.0.0.1:%d", port)),
		DatabasePath:          env("FASTTASK_DATABASE", "data/fasttask.db"),
		JWTSecret:             env("FASTTASK_JWT_SECRET", "local-development-secret-change-me"),
		AccessTTL:             time.Hour,
		RefreshTTL:            30 * 24 * time.Hour,
		AdminIdentifier:       env("FASTTASK_ADMIN_USER", "admin"),
		AdminPassword:         env("FASTTASK_ADMIN_PASSWORD", "fasttask-admin"),
		AdminName:             env("FASTTASK_ADMIN_NAME", "FastTask Admin"),
		WorkerInterval:        300 * time.Millisecond,
		WebDist:               env("FASTTASK_WEB_DIST", "web/dist"),
		Environment:           env("FASTTASK_ENV", "development"),
		OpenAIBaseURL:         strings.TrimRight(env("OPENAI_API_BASE_URL", ""), "/"),
		OpenAIModel:           env("OPENAI_MODEL", ""),
		OpenAIAPIKey:          env("OPENAI_API_KEY", ""),
		TranscriptionModel:    env("OPENAI_TRANSCRIPTION_MODEL", ""),
		AudioDir:              env("FASTTASK_AUDIO_DIR", "data/audio"),
		PanelJWTSecret:        env("FASTTASK_PANEL_JWT_SECRET", env("FASTTASK_JWT_SECRET", "local-development-secret-change-me")),
		ProviderEncryptionKey: strings.TrimSpace(env("FASTTASK_PROVIDER_ENCRYPTION_KEY", "")),
		TrustedProxies:        envList("FASTTASK_TRUSTED_PROXIES", "10.22.33.0/24"),
		FastReadURL:           strings.TrimRight(env("FASTTASK_FASTREAD_URL", ""), "/"),
		FastWriteURL:          strings.TrimRight(env("FASTTASK_FASTWRITE_URL", ""), "/"),
		IntegrationTimeout:    time.Duration(integrationTimeoutMS) * time.Millisecond,
	}
	if c.Port < 1 || c.Port > 65535 {
		return Config{}, errors.New("FASTTASK_PORT must be between 1 and 65535")
	}
	if len(c.JWTSecret) < 24 {
		return Config{}, errors.New("FASTTASK_JWT_SECRET must contain at least 24 characters")
	}
	if c.IntegrationTimeout < 100*time.Millisecond || c.IntegrationTimeout > 10*time.Second {
		return Config{}, errors.New("FASTTASK_INTEGRATION_TIMEOUT_MS must be between 100 and 10000")
	}
	if c.Environment == "production" && (strings.Contains(c.JWTSecret, "development") || c.AdminPassword == "fasttask-admin") {
		return Config{}, errors.New("production requires non-default FASTTASK_JWT_SECRET and FASTTASK_ADMIN_PASSWORD")
	}
	if c.Environment == "production" && (len(c.ProviderEncryptionKey) < 32 || strings.Contains(c.ProviderEncryptionKey, "development") || c.ProviderEncryptionKey == c.JWTSecret) {
		return Config{}, errors.New("production requires a unique FASTTASK_PROVIDER_ENCRYPTION_KEY with at least 32 characters")
	}
	if c.ProviderEncryptionKey == "" {
		c.ProviderEncryptionKey = c.JWTSecret
	}
	return c, nil
}

func (c Config) HasLLM() bool {
	return c.OpenAIBaseURL != "" && c.OpenAIModel != "" && c.OpenAIAPIKey != ""
}

func (c Config) Address() string { return fmt.Sprintf("%s:%d", c.Listen, c.Port) }

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return parsed, nil
}

func envList(key, fallback string) []string {
	value := env(key, fallback)
	result := make([]string, 0)
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), "\"'")
		if key != "" {
			if _, exists := os.LookupEnv(key); !exists {
				_ = os.Setenv(key, value)
			}
		}
	}
	return scanner.Err()
}
