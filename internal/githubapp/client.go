package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mvaldes14/nerv/internal/remediation"
	"go.yaml.in/yaml/v3"
)

const maxResponseBytes = 2 << 20

type Config struct {
	AppID          string
	InstallationID string
	PrivateKey     string
	BaseBranch     string
	APIURL         string
	Timeout        time.Duration
}

type Client struct {
	appID          string
	installationID string
	privateKey     *rsa.PrivateKey
	baseBranch     string
	apiURL         string
	http           *http.Client
}

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("GitHub API returned HTTP %d: %s", e.Status, e.Message)
}

func New(config Config) (*Client, error) {
	if strings.TrimSpace(config.AppID) == "" || strings.TrimSpace(config.InstallationID) == "" || strings.TrimSpace(config.PrivateKey) == "" {
		return nil, errors.New("GitHub App ID, installation ID, and private key must be configured together")
	}
	block, _ := pem.Decode([]byte(config.PrivateKey))
	if block == nil {
		return nil, errors.New("decode GitHub App private key PEM")
	}
	var key *rsa.PrivateKey
	parsedPKCS1, errPKCS1 := x509.ParsePKCS1PrivateKey(block.Bytes)
	if errPKCS1 == nil {
		key = parsedPKCS1
	} else {
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse GitHub App private key as PKCS#1 (%v) or PKCS#8: %w", errPKCS1, err)
		}
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("GitHub App private key must be RSA")
		}
	}
	baseBranch := strings.TrimSpace(config.BaseBranch)
	if baseBranch == "" {
		baseBranch = "main"
	}
	apiURL := strings.TrimRight(strings.TrimSpace(config.APIURL), "/")
	if apiURL == "" {
		apiURL = "https://api.github.com"
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		appID: strings.TrimSpace(config.AppID), installationID: strings.TrimSpace(config.InstallationID),
		privateKey: key, baseBranch: baseBranch, apiURL: apiURL, http: &http.Client{Timeout: timeout},
	}, nil
}

func (c *Client) PullRequestStatus(ctx context.Context, repository string, number int) (remediation.PullRequestStatus, error) {
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") || number <= 0 {
		return remediation.PullRequestStatus{}, errors.New("invalid GitHub pull request identity")
	}
	token, err := c.installationToken(ctx)
	if err != nil {
		return remediation.PullRequestStatus{}, err
	}
	var response struct {
		State    string     `json:"state"`
		Merged   bool       `json:"merged"`
		ClosedAt *time.Time `json:"closed_at"`
		MergedAt *time.Time `json:"merged_at"`
	}
	endpoint := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/pulls/" + strconv.Itoa(number)
	if err := c.request(ctx, http.MethodGet, endpoint, token, nil, &response); err != nil {
		return remediation.PullRequestStatus{}, err
	}
	if response.State != "open" && response.State != "closed" {
		return remediation.PullRequestStatus{}, fmt.Errorf("unexpected GitHub pull request state %q", response.State)
	}
	return remediation.PullRequestStatus{State: response.State, Merged: response.Merged, ClosedAt: response.ClosedAt, MergedAt: response.MergedAt}, nil
}

func (c *Client) Submit(ctx context.Context, req remediation.SubmissionRequest) (remediation.SubmissionResult, error) {
	owner, repository, ok := strings.Cut(req.Repository, "/")
	if !ok || owner == "" || repository == "" || strings.Contains(repository, "/") {
		return remediation.SubmissionResult{}, errors.New("invalid GitHub repository")
	}
	if strings.TrimSpace(req.Path) == "" || req.Resource.APIVersion == "" || req.Resource.Kind == "" || req.Resource.Namespace == "" || req.Resource.Name == "" {
		return remediation.SubmissionResult{}, errors.New("target path and resource identity are required")
	}
	if req.ExpectedReplicas < remediation.MinReplicas || req.ExpectedReplicas > remediation.MaxReplicas || req.DesiredReplicas < remediation.MinReplicas || req.DesiredReplicas > remediation.MaxReplicas || req.ExpectedReplicas == req.DesiredReplicas {
		return remediation.SubmissionResult{}, errors.New("invalid replica change")
	}
	token, err := c.installationToken(ctx)
	if err != nil {
		return remediation.SubmissionResult{}, err
	}
	baseContent, err := c.getContent(ctx, token, owner, repository, req.Path, c.baseBranch)
	if err != nil {
		return remediation.SubmissionResult{}, err
	}
	updated, current, err := updateReplicas(baseContent.Content, req.Resource, req.ExpectedReplicas, req.DesiredReplicas)
	if err != nil {
		return remediation.SubmissionResult{}, err
	}
	if current != req.ExpectedReplicas {
		return remediation.SubmissionResult{}, fmt.Errorf("%w: repository has replicas %d, expected %d", remediation.ErrConflict, current, req.ExpectedReplicas)
	}
	baseSHA, err := c.getRef(ctx, token, owner, repository, c.baseBranch)
	if err != nil {
		return remediation.SubmissionResult{}, err
	}
	branch := "nerv/remediation-" + compactID(req.ProposalID)
	if err := c.createRef(ctx, token, owner, repository, branch, baseSHA); err != nil {
		var apiErr *apiError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnprocessableEntity {
			return remediation.SubmissionResult{}, err
		}
		branchContent, getErr := c.getContent(ctx, token, owner, repository, req.Path, branch)
		if getErr != nil {
			return remediation.SubmissionResult{}, fmt.Errorf("resume existing remediation branch: %w", getErr)
		}
		updated, current, err = updateReplicas(branchContent.Content, req.Resource, req.ExpectedReplicas, req.DesiredReplicas)
		if err != nil {
			return remediation.SubmissionResult{}, err
		}
		baseContent = branchContent
		if current != req.ExpectedReplicas && current != req.DesiredReplicas {
			return remediation.SubmissionResult{}, fmt.Errorf("%w: remediation branch has replicas %d", remediation.ErrConflict, current)
		}
	}
	commitSHA := ""
	if current != req.DesiredReplicas {
		commitSHA, err = c.updateContent(ctx, token, owner, repository, req.Path, branch, baseContent.SHA, updated, req)
	} else {
		commitSHA, err = c.getRef(ctx, token, owner, repository, branch)
	}
	if err != nil {
		return remediation.SubmissionResult{}, err
	}
	pull, err := c.createPullRequest(ctx, token, owner, repository, branch, req)
	if err != nil {
		var apiErr *apiError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnprocessableEntity {
			return remediation.SubmissionResult{}, err
		}
		pull, err = c.findOpenPullRequest(ctx, token, owner, repository, branch)
		if err != nil {
			return remediation.SubmissionResult{}, err
		}
	}
	return remediation.SubmissionResult{Branch: branch, CommitSHA: commitSHA, PullRequestNumber: pull.Number, PullRequestURL: pull.HTMLURL}, nil
}

type contentResponse struct {
	SHA      string `json:"sha"`
	Content  []byte
	Encoding string `json:"encoding"`
}

func (c *contentResponse) UnmarshalJSON(data []byte) error {
	var raw struct{ SHA, Content, Encoding string }
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.Encoding != "base64" {
		return fmt.Errorf("unsupported GitHub content encoding %q", raw.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(raw.Content, "\n", ""))
	if err != nil {
		return fmt.Errorf("decode GitHub file content: %w", err)
	}
	c.SHA, c.Encoding, c.Content = raw.SHA, raw.Encoding, decoded
	return nil
}

func (c *Client) installationToken(ctx context.Context) (string, error) {
	jwt, err := c.jwt()
	if err != nil {
		return "", err
	}
	var response struct {
		Token       string            `json:"token"`
		Permissions map[string]string `json:"permissions"`
	}
	if err := c.request(ctx, http.MethodPost, "/app/installations/"+url.PathEscape(c.installationID)+"/access_tokens", jwt, nil, &response); err != nil {
		return "", err
	}
	if response.Token == "" {
		return "", errors.New("GitHub installation token response was empty")
	}
	if response.Permissions["contents"] != "write" || response.Permissions["pull_requests"] != "write" {
		return "", errors.New("GitHub App installation requires contents:write and pull_requests:write")
	}
	return response.Token, nil
}

func (c *Client) jwt() (string, error) {
	now := time.Now().Unix()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payloadJSON, _ := json.Marshal(map[string]any{"iat": now - 60, "exp": now + 540, "iss": c.appID})
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	unsigned := header + "." + payload
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, c.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign GitHub App JWT: %w", err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (c *Client) getContent(ctx context.Context, token, owner, repository, filePath, ref string) (contentResponse, error) {
	var response contentResponse
	endpoint := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repository) + "/contents/" + escapePath(filePath) + "?ref=" + url.QueryEscape(ref)
	err := c.request(ctx, http.MethodGet, endpoint, token, nil, &response)
	return response, err
}

func (c *Client) getRef(ctx context.Context, token, owner, repository, branch string) (string, error) {
	var response struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := c.request(ctx, http.MethodGet, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repository)+"/git/ref/heads/"+escapePath(branch), token, nil, &response); err != nil {
		return "", err
	}
	if response.Object.SHA == "" {
		return "", errors.New("GitHub branch ref SHA was empty")
	}
	return response.Object.SHA, nil
}

func (c *Client) createRef(ctx context.Context, token, owner, repository, branch, sha string) error {
	return c.request(ctx, http.MethodPost, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repository)+"/git/refs", token, map[string]string{"ref": "refs/heads/" + branch, "sha": sha}, nil)
}

func (c *Client) updateContent(ctx context.Context, token, owner, repository, filePath, branch, sha string, content []byte, req remediation.SubmissionRequest) (string, error) {
	body := map[string]string{"message": "fix(nerv): " + singleLine(req.Summary, 100), "content": base64.StdEncoding.EncodeToString(content), "sha": sha, "branch": branch}
	var response struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := c.request(ctx, http.MethodPut, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repository)+"/contents/"+escapePath(filePath), token, body, &response); err != nil {
		return "", err
	}
	return response.Commit.SHA, nil
}

type pullResponse struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

func (c *Client) createPullRequest(ctx context.Context, token, owner, repository, branch string, req remediation.SubmissionRequest) (pullResponse, error) {
	body := map[string]any{
		"title": "NERV: " + singleLine(req.Summary, 180), "head": branch, "base": c.baseBranch,
		"body": fmt.Sprintf("Automated GitOps change from approved NERV remediation proposal `%s`.\n\n%s\n\nExpected replicas: `%d`  \nDesired replicas: `%d`\n\nThis PR requires human review and merge.", req.ProposalID, req.Rationale, req.ExpectedReplicas, req.DesiredReplicas),
	}
	var response pullResponse
	err := c.request(ctx, http.MethodPost, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repository)+"/pulls", token, body, &response)
	return response, err
}

func (c *Client) findOpenPullRequest(ctx context.Context, token, owner, repository, branch string) (pullResponse, error) {
	var response []pullResponse
	endpoint := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repository) + "/pulls?state=open&head=" + url.QueryEscape(owner+":"+branch)
	if err := c.request(ctx, http.MethodGet, endpoint, token, nil, &response); err != nil {
		return pullResponse{}, err
	}
	if len(response) == 0 {
		return pullResponse{}, errors.New("existing remediation branch has no open pull request")
	}
	return response[0], nil
}

func (c *Client) request(ctx context.Context, method, endpoint, token string, body, response any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiURL+endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call GitHub API: %w", err)
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, maxResponseBytes)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiResponse struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(limited).Decode(&apiResponse)
		return &apiError{Status: resp.StatusCode, Message: apiResponse.Message}
	}
	if response != nil {
		if err := json.NewDecoder(limited).Decode(response); err != nil {
			return fmt.Errorf("decode GitHub API response: %w", err)
		}
	}
	return nil
}

func updateReplicas(content []byte, resource remediation.Resource, expected, desired int) ([]byte, int, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	for {
		var document yaml.Node
		if err := decoder.Decode(&document); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, 0, fmt.Errorf("parse target manifest: %w", err)
		}
		if len(document.Content) == 0 || len(document.Content[0].Content) == 0 {
			continue
		}
		root := document.Content[0]
		if scalar(root, "apiVersion") != resource.APIVersion || scalar(root, "kind") != resource.Kind {
			continue
		}
		metadata := mapping(root, "metadata")
		if metadata == nil || scalar(metadata, "name") != resource.Name {
			continue
		}
		namespace := scalar(metadata, "namespace")
		if namespace != "" && namespace != resource.Namespace {
			continue
		}
		spec := mapping(root, "spec")
		replicas := value(spec, "replicas")
		if replicas == nil {
			return nil, 0, errors.New("target manifest spec.replicas is required")
		}
		current, err := strconv.Atoi(replicas.Value)
		if err != nil {
			return nil, 0, errors.New("target manifest spec.replicas must be an integer")
		}
		if current != expected {
			return append([]byte(nil), content...), current, nil
		}
		lines := strings.Split(string(content), "\n")
		line := replicas.Line - 1
		if line < 0 || line >= len(lines) {
			return nil, 0, errors.New("target manifest replicas line is invalid")
		}
		colon := strings.Index(lines[line], ":")
		if colon < 0 || strings.TrimSpace(lines[line][:colon]) != "replicas" {
			return nil, 0, errors.New("target manifest replicas line cannot be safely edited")
		}
		rest := lines[line][colon+1:]
		comment := ""
		if index := strings.Index(rest, "#"); index >= 0 {
			comment = " " + strings.TrimSpace(rest[index:])
		}
		lines[line] = lines[line][:colon+1] + " " + strconv.Itoa(desired) + comment
		return []byte(strings.Join(lines, "\n")), current, nil
	}
	return nil, 0, errors.New("allowlisted resource was not found in target manifest")
}

func mapping(node *yaml.Node, key string) *yaml.Node {
	found := value(node, key)
	if found != nil && found.Kind == yaml.MappingNode {
		return found
	}
	return nil
}

func scalar(node *yaml.Node, key string) string {
	found := value(node, key)
	if found == nil || found.Kind != yaml.ScalarNode {
		return ""
	}
	return found.Value
}

func value(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func compactID(id string) string {
	id = strings.ToLower(strings.ReplaceAll(id, "-", ""))
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

func singleLine(value string, maximum int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > maximum {
		value = value[:maximum]
	}
	return value
}

func escapePath(value string) string {
	parts := strings.Split(value, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
