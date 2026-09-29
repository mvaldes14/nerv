package config

import "os"

// Config contains process-level settings. Secrets come from the environment in
// local development and will later come from Vault/ESO in cluster deployments.
type Config struct {
	Addr        string
	AuthToken   string
	LLMProvider string
	LLMModel    string
	DatabaseURL string
}

func Load() Config {
	return Config{
		Addr:        envOr("NERV_ADDR", "127.0.0.1:8080"),
		AuthToken:   os.Getenv("NERV_AUTH_TOKEN"),
		LLMProvider: envOr("NERV_LLM_PROVIDER", "echo"),
		LLMModel:    envOr("NERV_LLM_MODEL", "echo-v0"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
