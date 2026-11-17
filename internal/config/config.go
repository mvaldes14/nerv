package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config contains process-level settings. Secrets come from environment
// variables locally and Vault/ESO in cluster deployments.
type Config struct {
	Addr                           string
	AuthToken                      string
	DatabaseURL                    string
	Kubeconfig                     string
	LLMProvider                    string
	LLMModel                       string
	AgentgatewayURL                string
	AgentgatewayToken              string
	DecisionGatewayURL             string
	DecisionGatewayToken           string
	DecisionName                   string
	DecisionVersion                string
	GotifyURL                      string
	GotifyToken                    string
	HTTPTimeout                    time.Duration
	DetectionEnabled               bool
	DetectionPersistence           time.Duration
	DetectionCooldown              time.Duration
	DetectionResync                time.Duration
	DetectionWorkers               int
	DetectionQueueCapacity         int
	DetectionRestartThreshold      int
	DetectionRestartWindow         time.Duration
	DetectionNamespaces            []string
	DetectionMaintenanceAnnotation string
	OTLPEndpoint                   string
	OTLPInsecure                   bool
}

func Load() Config {
	return Config{
		Addr:                           envOr("NERV_ADDR", "127.0.0.1:8080"),
		AuthToken:                      os.Getenv("NERV_AUTH_TOKEN"),
		DatabaseURL:                    os.Getenv("DATABASE_URL"),
		Kubeconfig:                     os.Getenv("KUBECONFIG"),
		LLMProvider:                    envOr("NERV_LLM_PROVIDER", "agentgateway"),
		LLMModel:                       envOr("NERV_LLM_MODEL", "openai/gpt-5-mini"),
		AgentgatewayURL:                os.Getenv("NERV_AGENTGATEWAY_URL"),
		AgentgatewayToken:              os.Getenv("NERV_AGENTGATEWAY_TOKEN"),
		DecisionGatewayURL:             os.Getenv("NERV_DECISION_GATEWAY_URL"),
		DecisionGatewayToken:           os.Getenv("NERV_DECISION_GATEWAY_TOKEN"),
		DecisionName:                   envOr("NERV_DECISION_NAME", "alert-disposition"),
		DecisionVersion:                envOr("NERV_DECISION_VERSION", "v1"),
		GotifyURL:                      os.Getenv("NERV_GOTIFY_URL"),
		GotifyToken:                    os.Getenv("NERV_GOTIFY_TOKEN"),
		HTTPTimeout:                    durationOr("NERV_HTTP_TIMEOUT", 30*time.Second),
		DetectionEnabled:               boolOr("NERV_DETECTION_ENABLED", false),
		DetectionPersistence:           durationOr("NERV_DETECTION_PERSISTENCE", 2*time.Minute),
		DetectionCooldown:              durationOr("NERV_DETECTION_COOLDOWN", 30*time.Minute),
		DetectionResync:                durationOr("NERV_DETECTION_RESYNC", 30*time.Second),
		DetectionWorkers:               intOr("NERV_DETECTION_WORKERS", 2),
		DetectionQueueCapacity:         intOr("NERV_DETECTION_QUEUE_CAPACITY", 512),
		DetectionRestartThreshold:      intOr("NERV_DETECTION_RESTART_THRESHOLD", 3),
		DetectionRestartWindow:         durationOr("NERV_DETECTION_RESTART_WINDOW", 10*time.Minute),
		DetectionNamespaces:            listOr("NERV_DETECTION_NAMESPACES", nil),
		DetectionMaintenanceAnnotation: envOr("NERV_DETECTION_MAINTENANCE_ANNOTATION", "nerv.io/maintenance"),
		OTLPEndpoint:                   os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		OTLPInsecure:                   boolOr("OTEL_EXPORTER_OTLP_INSECURE", true),
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func durationOr(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func intOr(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return fallback
	}
	return parsed
}

func listOr(key string, fallback []string) []string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func boolOr(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}
