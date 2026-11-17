package config

import "testing"

func TestLoadAgentDefaultsAndFallbackDisabled(t *testing.T) {
	t.Setenv("NERV_LLM_FALLBACK_MODEL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentMaxRounds != 4 || cfg.AgentMaxToolCalls != 6 || cfg.AgentMaxTokens != 12000 || cfg.AgentToolConcurrency != 2 {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
	if cfg.LLMFallbackModel != "" {
		t.Fatalf("fallback model = %q, want disabled", cfg.LLMFallbackModel)
	}
}

func TestLoadRejectsUnsafeAgentConfiguration(t *testing.T) {
	tests := []struct {
		key   string
		value string
	}{
		{key: "NERV_AGENT_MAX_ROUNDS", value: "0"},
		{key: "NERV_AGENT_TIMEOUT", value: "0s"},
		{key: "NERV_AGENT_TIMEOUT", value: "16m"},
		{key: "NERV_AGENT_TOOL_CONCURRENCY", value: "99"},
		{key: "NERV_AGENT_MAX_TOOL_OUTPUT_BYTES", value: "128"},
		{key: "NERV_AGENT_MAX_COST_USD", value: "1"},
		{key: "NERV_AGENT_INPUT_PRICE_PER_MILLION_TOKENS_USD", value: "NaN"},
	}
	for _, test := range tests {
		t.Run(test.key+"="+test.value, func(t *testing.T) {
			t.Setenv(test.key, test.value)
			if _, err := Load(); err == nil {
				t.Fatalf("%s=%s was accepted", test.key, test.value)
			}
		})
	}
}

func TestLoadRejectsFallbackEqualToPrimary(t *testing.T) {
	t.Setenv("NERV_LLM_MODEL", "same")
	t.Setenv("NERV_LLM_FALLBACK_MODEL", "same")
	if _, err := Load(); err == nil {
		t.Fatal("fallback equal to primary was accepted")
	}
}
