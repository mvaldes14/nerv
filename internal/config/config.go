package config

import (
	"fmt"
	"math"
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
	LLMFallbackModel               string
	AgentgatewayURL                string
	AgentgatewayToken              string
	DecisionGatewayURL             string
	DecisionGatewayToken           string
	DecisionName                   string
	DecisionVersion                string
	GotifyURL                      string
	GotifyToken                    string
	NotificationDebounce           time.Duration
	HTTPTimeout                    time.Duration
	AgentMaxRounds                 int
	AgentMaxToolCalls              int
	AgentTimeout                   time.Duration
	AgentMaxTokens                 int
	AgentToolConcurrency           int
	AgentModelTimeout              time.Duration
	AgentToolTimeout               time.Duration
	AgentMaxEvidenceBytes          int
	AgentMaxToolOutputBytes        int
	AgentMaxCostUSD                float64
	AgentInputPricePerMTokens      float64
	AgentOutputPricePerMTokens     float64
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

func Load() (Config, error) {
	httpTimeout, err := durationOr("NERV_HTTP_TIMEOUT", 30*time.Second)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Addr:                           envOr("NERV_ADDR", "127.0.0.1:8080"),
		AuthToken:                      os.Getenv("NERV_AUTH_TOKEN"),
		DatabaseURL:                    os.Getenv("DATABASE_URL"),
		Kubeconfig:                     os.Getenv("KUBECONFIG"),
		LLMProvider:                    envOr("NERV_LLM_PROVIDER", "agentgateway"),
		LLMModel:                       envOr("NERV_LLM_MODEL", "openai/gpt-5-mini"),
		LLMFallbackModel:               strings.TrimSpace(os.Getenv("NERV_LLM_FALLBACK_MODEL")),
		AgentgatewayURL:                os.Getenv("NERV_AGENTGATEWAY_URL"),
		AgentgatewayToken:              os.Getenv("NERV_AGENTGATEWAY_TOKEN"),
		DecisionGatewayURL:             os.Getenv("NERV_DECISION_GATEWAY_URL"),
		DecisionGatewayToken:           os.Getenv("NERV_DECISION_GATEWAY_TOKEN"),
		DecisionName:                   envOr("NERV_DECISION_NAME", "alert-disposition"),
		DecisionVersion:                envOr("NERV_DECISION_VERSION", "v1"),
		GotifyURL:                      os.Getenv("NERV_GOTIFY_URL"),
		GotifyToken:                    os.Getenv("NERV_GOTIFY_TOKEN"),
		HTTPTimeout:                    httpTimeout,
		DetectionNamespaces:            listOr("NERV_DETECTION_NAMESPACES", nil),
		DetectionMaintenanceAnnotation: envOr("NERV_DETECTION_MAINTENANCE_ANNOTATION", "nerv.io/maintenance"),
		OTLPEndpoint:                   os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	}
	if cfg.NotificationDebounce, err = durationOr("NERV_NOTIFICATION_DEBOUNCE", 2*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.AgentMaxRounds, err = intOr("NERV_AGENT_MAX_ROUNDS", 4); err != nil {
		return Config{}, err
	}
	if cfg.AgentMaxToolCalls, err = intOr("NERV_AGENT_MAX_TOOL_CALLS", 6); err != nil {
		return Config{}, err
	}
	if cfg.AgentTimeout, err = durationOr("NERV_AGENT_TIMEOUT", 75*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.AgentMaxTokens, err = intOr("NERV_AGENT_MAX_TOKENS", 12000); err != nil {
		return Config{}, err
	}
	if cfg.AgentToolConcurrency, err = intOr("NERV_AGENT_TOOL_CONCURRENCY", 2); err != nil {
		return Config{}, err
	}
	if cfg.AgentModelTimeout, err = durationOr("NERV_AGENT_MODEL_TIMEOUT", httpTimeout); err != nil {
		return Config{}, err
	}
	if cfg.AgentToolTimeout, err = durationOr("NERV_AGENT_TOOL_TIMEOUT", httpTimeout); err != nil {
		return Config{}, err
	}
	if cfg.AgentMaxEvidenceBytes, err = intOr("NERV_AGENT_MAX_EVIDENCE_BYTES", 32*1024); err != nil {
		return Config{}, err
	}
	if cfg.AgentMaxToolOutputBytes, err = intOr("NERV_AGENT_MAX_TOOL_OUTPUT_BYTES", 32*1024); err != nil {
		return Config{}, err
	}
	if cfg.AgentMaxCostUSD, err = floatOr("NERV_AGENT_MAX_COST_USD", 0); err != nil {
		return Config{}, err
	}
	if cfg.AgentInputPricePerMTokens, err = floatOr("NERV_AGENT_INPUT_PRICE_PER_MILLION_TOKENS_USD", 0); err != nil {
		return Config{}, err
	}
	if cfg.AgentOutputPricePerMTokens, err = floatOr("NERV_AGENT_OUTPUT_PRICE_PER_MILLION_TOKENS_USD", 0); err != nil {
		return Config{}, err
	}
	if cfg.DetectionEnabled, err = boolOr("NERV_DETECTION_ENABLED", false); err != nil {
		return Config{}, err
	}
	if cfg.DetectionPersistence, err = durationOr("NERV_DETECTION_PERSISTENCE", 2*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.DetectionCooldown, err = durationOr("NERV_DETECTION_COOLDOWN", 30*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.DetectionResync, err = durationOr("NERV_DETECTION_RESYNC", 30*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.DetectionWorkers, err = intOr("NERV_DETECTION_WORKERS", 2); err != nil {
		return Config{}, err
	}
	if cfg.DetectionQueueCapacity, err = intOr("NERV_DETECTION_QUEUE_CAPACITY", 512); err != nil {
		return Config{}, err
	}
	if cfg.DetectionRestartThreshold, err = intOr("NERV_DETECTION_RESTART_THRESHOLD", 3); err != nil {
		return Config{}, err
	}
	if cfg.DetectionRestartWindow, err = durationOr("NERV_DETECTION_RESTART_WINDOW", 10*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.OTLPInsecure, err = boolOr("OTEL_EXPORTER_OTLP_INSECURE", true); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.HTTPTimeout <= 0 || c.AgentTimeout <= 0 || c.AgentModelTimeout <= 0 || c.AgentToolTimeout <= 0 {
		return fmt.Errorf("HTTP and agent timeouts must be positive")
	}
	if c.HTTPTimeout > 5*time.Minute || c.AgentTimeout > 15*time.Minute || c.AgentModelTimeout > 5*time.Minute || c.AgentToolTimeout > 5*time.Minute {
		return fmt.Errorf("HTTP and agent timeouts exceed safety caps")
	}
	if c.NotificationDebounce <= 0 || c.NotificationDebounce > 15*time.Minute {
		return fmt.Errorf("NERV_NOTIFICATION_DEBOUNCE must be positive and at most 15m")
	}
	if c.LLMFallbackModel != "" && c.LLMFallbackModel == c.LLMModel {
		return fmt.Errorf("NERV_LLM_FALLBACK_MODEL must differ from NERV_LLM_MODEL")
	}
	if c.AgentMaxRounds < 1 || c.AgentMaxRounds > 20 {
		return fmt.Errorf("NERV_AGENT_MAX_ROUNDS must be between 1 and 20")
	}
	if c.AgentMaxToolCalls < 1 || c.AgentMaxToolCalls > 50 {
		return fmt.Errorf("NERV_AGENT_MAX_TOOL_CALLS must be between 1 and 50")
	}
	if c.AgentToolConcurrency < 1 || c.AgentToolConcurrency > 16 || c.AgentToolConcurrency > c.AgentMaxToolCalls {
		return fmt.Errorf("NERV_AGENT_TOOL_CONCURRENCY must be positive, at most 16, and no greater than tool-call budget")
	}
	if c.AgentMaxTokens < 1 || c.AgentMaxTokens > 1_000_000 {
		return fmt.Errorf("NERV_AGENT_MAX_TOKENS must be between 1 and 1000000")
	}
	if c.AgentMaxEvidenceBytes < 256 || c.AgentMaxEvidenceBytes > 1<<20 || c.AgentMaxToolOutputBytes < 256 || c.AgentMaxToolOutputBytes > 1<<20 {
		return fmt.Errorf("agent evidence and tool-output byte limits must be between 256 and 1048576")
	}
	if c.AgentMaxCostUSD < 0 || c.AgentInputPricePerMTokens < 0 || c.AgentOutputPricePerMTokens < 0 {
		return fmt.Errorf("agent cost and price values cannot be negative")
	}
	if c.AgentMaxCostUSD > 10_000 || c.AgentInputPricePerMTokens > 1_000_000 || c.AgentOutputPricePerMTokens > 1_000_000 {
		return fmt.Errorf("agent cost and price values exceed safety caps")
	}
	pricingSet := c.AgentInputPricePerMTokens > 0 && c.AgentOutputPricePerMTokens > 0
	pricingPartial := (c.AgentInputPricePerMTokens > 0) != (c.AgentOutputPricePerMTokens > 0)
	if pricingPartial {
		return fmt.Errorf("both agent input and output prices must be configured together")
	}
	if c.AgentMaxCostUSD > 0 && !pricingSet {
		return fmt.Errorf("agent cost ceiling requires input and output price configuration")
	}
	return nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func durationOr(key string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return parsed, nil
}

func intOr(key string, fallback int) (int, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return parsed, nil
}

func floatOr(key string, fallback float64) (float64, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed < 0 || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, fmt.Errorf("%s must be a non-negative number", key)
	}
	return parsed, nil
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

func boolOr(key string, fallback bool) (bool, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", key)
	}
	return parsed, nil
}
