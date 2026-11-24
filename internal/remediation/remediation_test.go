package remediation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeStore struct {
	created   Proposal
	ordinals  []int
	decided   Proposal
	status    string
	subject   string
	proposal  Proposal
	completed SubmissionResult
	failed    string
}

func (f *fakeStore) CreateProposal(_ context.Context, proposal Proposal, ordinals []int) (Proposal, error) {
	f.created = proposal
	f.ordinals = append([]int(nil), ordinals...)
	proposal.ID = "proposal-1"
	return proposal, nil
}
func (f *fakeStore) ListProposals(context.Context, string, int) ([]Proposal, error) { return nil, nil }
func (f *fakeStore) GetProposal(context.Context, string) (Proposal, error)          { return f.proposal, nil }
func (f *fakeStore) DecideProposal(_ context.Context, _ string, status, actor, subject, comment string) (Proposal, error) {
	f.status = status
	f.subject = subject
	f.decided = Proposal{Status: status, DecisionActor: actor, DecisionAuthSubject: subject, DecisionComment: comment}
	return f.decided, nil
}
func (f *fakeStore) BeginProposalSubmission(context.Context, string) (Proposal, error) {
	f.proposal.SubmissionStatus = SubmissionSubmitting
	return f.proposal, nil
}
func (f *fakeStore) CompleteProposalSubmission(_ context.Context, _ string, result SubmissionResult) (Proposal, error) {
	f.completed = result
	f.proposal.SubmissionStatus = SubmissionSubmitted
	f.proposal.PullRequestURL = result.PullRequestURL
	return f.proposal, nil
}
func (f *fakeStore) FailProposalSubmission(_ context.Context, _ string, message string) error {
	f.failed = message
	return nil
}

type fakeSubmitter struct {
	request SubmissionRequest
	result  SubmissionResult
	err     error
}

func (f *fakeSubmitter) Submit(_ context.Context, request SubmissionRequest) (SubmissionResult, error) {
	f.request = request
	return f.result, f.err
}

func testConfig() Config {
	return Config{
		Repository: "mvaldes14/k8s-apps",
		Targets: []Target{{
			ID:   "checkout",
			Path: "apps/checkout/deployment.yaml",
			Resource: Resource{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Namespace:  "apps",
				Name:       "checkout",
			},
		}},
	}
}

func intPointer(value int) *int { return &value }

func validRequest() CreateRequest {
	return CreateRequest{
		ActionKind:       ActionSetWorkloadReplicas,
		TargetID:         "checkout",
		Expected:         ReplicaInput{Replicas: intPointer(2)},
		Desired:          ReplicaInput{Replicas: intPointer(4)},
		Summary:          "Scale checkout",
		Rationale:        "Current replicas are saturated.",
		Risk:             RiskInput{Level: "medium", Detail: "Higher resource use."},
		EvidenceOrdinals: []int{1, 3},
		TTLSeconds:       600,
	}
}

func TestCreateBuildsTrustedDeterministicProjection(t *testing.T) {
	store := &fakeStore{}
	service, err := NewService(testConfig(), store)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := service.Create(context.Background(), "incident-1", validRequest())
	if err != nil {
		t.Fatal(err)
	}
	if proposal.TargetRepository != "mvaldes14/k8s-apps" || proposal.TargetPath != "apps/checkout/deployment.yaml" {
		t.Fatalf("trusted target = %s %s", proposal.TargetRepository, proposal.TargetPath)
	}
	wantBefore := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: checkout\n  namespace: apps\nspec:\n  replicas: 2\n"
	if proposal.RenderedBefore != wantBefore {
		t.Fatalf("rendered before:\n%s", proposal.RenderedBefore)
	}
	for _, want := range []string{"--- a/apps/checkout/deployment.yaml", "+++ b/apps/checkout/deployment.yaml", "-  replicas: 2", "+  replicas: 4"} {
		if !strings.Contains(proposal.RenderedDiff, want) {
			t.Errorf("diff missing %q:\n%s", want, proposal.RenderedDiff)
		}
	}
	if proposal.RenderFormat != RenderManifestProjection || proposal.Status != StatusPending {
		t.Fatalf("proposal format/status = %q/%q", proposal.RenderFormat, proposal.Status)
	}
	if got := proposal.ExpiresAt.Sub(time.Now().UTC()); got < 9*time.Minute || got > 11*time.Minute {
		t.Fatalf("expiry delta = %s", got)
	}
}

func TestCreateDefaultsTTLToTwentyFourHours(t *testing.T) {
	service, err := NewService(testConfig(), &fakeStore{})
	if err != nil {
		t.Fatal(err)
	}
	req := validRequest()
	req.TTLSeconds = 0
	proposal, err := service.Create(context.Background(), "incident-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if got := proposal.ExpiresAt.Sub(time.Now().UTC()); got < 23*time.Hour+59*time.Minute || got > 24*time.Hour+time.Minute {
		t.Fatalf("expiry delta = %s", got)
	}
}

func TestCreateValidationRejectsMutationSurfaceAndUnsafeValues(t *testing.T) {
	tests := []struct {
		name   string
		change func(*CreateRequest)
	}{
		{name: "unknown action", change: func(r *CreateRequest) { r.ActionKind = "kubectl/v1" }},
		{name: "unknown target", change: func(r *CreateRequest) { r.TargetID = "other" }},
		{name: "missing expected", change: func(r *CreateRequest) { r.Expected.Replicas = nil }},
		{name: "replicas too high", change: func(r *CreateRequest) { r.Desired.Replicas = intPointer(1001) }},
		{name: "unchanged", change: func(r *CreateRequest) { r.Desired.Replicas = intPointer(2) }},
		{name: "bad risk", change: func(r *CreateRequest) { r.Risk.Level = "critical" }},
		{name: "no evidence", change: func(r *CreateRequest) { r.EvidenceOrdinals = nil }},
		{name: "duplicate evidence", change: func(r *CreateRequest) { r.EvidenceOrdinals = []int{1, 1} }},
		{name: "short ttl", change: func(r *CreateRequest) { r.TTLSeconds = 299 }},
		{name: "long ttl", change: func(r *CreateRequest) { r.TTLSeconds = 604801 }},
	}
	service, err := NewService(testConfig(), &fakeStore{})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := validRequest()
			test.change(&req)
			_, err := service.Create(context.Background(), "incident-1", req)
			var validation *ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("error = %v, want validation", err)
			}
		})
	}
}

func TestParseConfigRejectsUntrustedTargets(t *testing.T) {
	tests := []struct {
		name       string
		repository string
		targets    string
	}{
		{name: "url repository", repository: "https://github.com/org/repo", targets: `[]`},
		{name: "unknown field", repository: "org/repo", targets: `[ {"id":"x","path":"x.yaml","resource":{"api_version":"apps/v1","kind":"Deployment","namespace":"apps","name":"x"},"url":"https://example"} ]`},
		{name: "absolute path", repository: "org/repo", targets: `[ {"id":"x","path":"/x.yaml","resource":{"api_version":"apps/v1","kind":"Deployment","namespace":"apps","name":"x"}} ]`},
		{name: "traversal", repository: "org/repo", targets: `[ {"id":"x","path":"apps/../x.yaml","resource":{"api_version":"apps/v1","kind":"Deployment","namespace":"apps","name":"x"}} ]`},
		{name: "backslash", repository: "org/repo", targets: `[ {"id":"x","path":"apps\\x.yaml","resource":{"api_version":"apps/v1","kind":"Deployment","namespace":"apps","name":"x"}} ]`},
		{name: "empty segment", repository: "org/repo", targets: `[ {"id":"x","path":"apps//x.yaml","resource":{"api_version":"apps/v1","kind":"Deployment","namespace":"apps","name":"x"}} ]`},
		{name: "wrong extension", repository: "org/repo", targets: `[ {"id":"x","path":"apps/x.json","resource":{"api_version":"apps/v1","kind":"Deployment","namespace":"apps","name":"x"}} ]`},
		{name: "wrong api", repository: "org/repo", targets: `[ {"id":"x","path":"apps/x.yaml","resource":{"api_version":"v1","kind":"Deployment","namespace":"apps","name":"x"}} ]`},
		{name: "wrong kind", repository: "org/repo", targets: `[ {"id":"x","path":"apps/x.yaml","resource":{"api_version":"apps/v1","kind":"DaemonSet","namespace":"apps","name":"x"}} ]`},
		{name: "duplicate id", repository: "org/repo", targets: `[ {"id":"x","path":"apps/x.yaml","resource":{"api_version":"apps/v1","kind":"Deployment","namespace":"apps","name":"x"}}, {"id":"x","path":"apps/y.yaml","resource":{"api_version":"apps/v1","kind":"StatefulSet","namespace":"apps","name":"y"}} ]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseConfig(test.repository, test.targets); err == nil {
				t.Fatal("unsafe config accepted")
			}
		})
	}
}

func TestDecisionPersistsSharedBearerSubject(t *testing.T) {
	store := &fakeStore{}
	service, err := NewService(testConfig(), store)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := service.Approve(context.Background(), "proposal-1", DecisionRequest{Actor: "operator", Comment: "reviewed"}, "shared-bearer")
	if err != nil {
		t.Fatal(err)
	}
	if store.status != StatusApproved || proposal.DecisionAuthSubject != "shared-bearer" {
		t.Fatalf("decision = %+v", proposal)
	}
}

func TestSubmitCreatesPullRequestForApprovedProposal(t *testing.T) {
	store := &fakeStore{proposal: Proposal{
		ID: "proposal-1", Status: StatusApproved, SubmissionStatus: SubmissionNotSubmitted,
		TargetRepository: "mvaldes14/k8s-apps", TargetPath: "overlays/nerv/deployment.yaml",
		TargetResource: Resource{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "nerv", Name: "nerv"},
		ExpectedState:  ReplicaState{Replicas: 1}, DesiredChange: ReplicaState{Replicas: 2},
		Summary: "scale nerv", Rationale: "validated",
	}}
	submitter := &fakeSubmitter{result: SubmissionResult{Branch: "nerv/remediation-proposal1", CommitSHA: "abc", PullRequestNumber: 108, PullRequestURL: "https://github.test/pr/108"}}
	service, err := NewService(testConfig(), store, submitter)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := service.Submit(context.Background(), "proposal-1")
	if err != nil {
		t.Fatal(err)
	}
	if submitter.request.ExpectedReplicas != 1 || submitter.request.DesiredReplicas != 2 {
		t.Fatalf("request = %+v", submitter.request)
	}
	if proposal.SubmissionStatus != SubmissionSubmitted || proposal.PullRequestURL == "" {
		t.Fatalf("proposal = %+v", proposal)
	}
}

func TestSubmitRecordsFailure(t *testing.T) {
	store := &fakeStore{proposal: Proposal{ID: "proposal-1", Status: StatusApproved, SubmissionStatus: SubmissionNotSubmitted}}
	submitter := &fakeSubmitter{err: errors.New("github unavailable")}
	service, err := NewService(testConfig(), store, submitter)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Submit(context.Background(), "proposal-1"); err == nil {
		t.Fatal("expected submission error")
	}
	if store.failed != "github unavailable" {
		t.Fatalf("failure = %q", store.failed)
	}
}

func TestRenderManifestRejectsInvalidReplicaCount(t *testing.T) {
	resource := Resource{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "apps", Name: "checkout"}
	for _, replicas := range []int{-1, MaxReplicas + 1} {
		if _, err := renderManifest(resource, replicas); err == nil {
			t.Fatalf("renderManifest accepted replicas=%d", replicas)
		}
	}
}
