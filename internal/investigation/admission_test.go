package investigation

import "testing"

func TestAdmissionPolicyRateLimitBySource(t *testing.T) {
	policy := AdmissionPolicy{AskPerMinute: 6, AlertPerMinute: 30, DetectorPerMinute: 20}
	for source, want := range map[string]int{
		SourceHTTPAsk:            6,
		SourceCLI:                6,
		SourceHTTPAlert:          30,
		SourceKubernetesDetector: 20,
		"unknown":                30,
	} {
		if got := policy.RateLimit(source); got != want {
			t.Errorf("RateLimit(%q) = %d, want %d", source, got, want)
		}
	}
}
