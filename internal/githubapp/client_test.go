package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mvaldes14/nerv/internal/remediation"
)

const testManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: nerv
  namespace: nerv
spec:
  replicas: 1 # retained
  selector:
    matchLabels:
      app: nerv
`

func TestSubmitCreatesBranchCommitAndPullRequest(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var committed string
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
			parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
			if len(parts) != 3 {
				t.Errorf("invalid JWT")
			}
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"token":"installation-token","permissions":{"contents":"write","pull_requests":"write","metadata":"read"}}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/contents/"):
			json.NewEncoder(w).Encode(map[string]string{"sha": "file-sha", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(testManifest))})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/git/ref/heads/main"):
			io.WriteString(w, `{"object":{"sha":"base-sha"}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/refs"):
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/contents/"):
			var body struct {
				Content string `json:"content"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			decoded, _ := base64.StdEncoding.DecodeString(body.Content)
			committed = string(decoded)
			io.WriteString(w, `{"commit":{"sha":"commit-sha"}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"number":108,"html_url":"https://github.test/pr/108"}`)
		default:
			http.Error(w, `{"message":"unexpected"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()

	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	client, err := New(Config{AppID: "5905005", InstallationID: "42", PrivateKey: string(privatePEM), APIURL: server.URL, BaseBranch: "main", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Submit(context.Background(), remediation.SubmissionRequest{
		ProposalID: "c49faad9-dd33-4ce0-8dc3-c5cdd45f32cf", Repository: "mvaldes14/k8s-apps", Path: "overlays/nerv/deployment.yaml",
		Resource:         remediation.Resource{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "nerv", Name: "nerv"},
		ExpectedReplicas: 1, DesiredReplicas: 2, Summary: "scale nerv", Rationale: "validated",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.PullRequestNumber != 108 || result.Branch != "nerv/remediation-c49faad9dd33" || result.CommitSHA != "commit-sha" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if !strings.Contains(committed, "  replicas: 2 # retained") {
		t.Fatalf("replicas not safely updated:\n%s", committed)
	}
	if strings.Count(committed, "replicas:") != 1 {
		t.Fatalf("unexpected manifest change: %s", committed)
	}
	if len(calls) != 6 {
		t.Fatalf("unexpected calls: %v", calls)
	}
}

func TestUpdateReplicasRejectsStaleExpectedState(t *testing.T) {
	updated, current, err := updateReplicas([]byte(testManifest), remediation.Resource{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "nerv", Name: "nerv"}, 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	if current != 1 || string(updated) != testManifest {
		t.Fatalf("unexpected stale result current=%d", current)
	}
}
