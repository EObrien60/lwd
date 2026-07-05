package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// AgentNode is a Node implementation that talks to a remote lwd-agent server
// (internal/agent.Server) over HTTP, using bearer-token auth. It is the
// mirror image of that server: every method here sends the request the server
// expects and decodes the response it sends back, using the wire contract
// (Path* constants and *Request/*Response DTOs) defined in wire.go. Because
// AgentNode lives in package node — the same package as the wire contract and
// as the Node primitives (RunSpec/Container/HealthSpec) those DTOs wrap — the
// client and the agent server (which imports package node) share the exact
// same types, so drift between the two is structurally impossible.
//
// AgentNode is constructed once per remote node and reused; it holds no
// per-request state beyond its http.Client.
type AgentNode struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewAgentNode returns an AgentNode that dials baseURL (trailing slash
// trimmed) using token for bearer authentication.
func NewAgentNode(baseURL, token string) *AgentNode {
	return &AgentNode{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

var _ Node = (*AgentNode)(nil)

// agentError is returned when the agent responds with a non-2xx status. It
// carries the status code so callers/logs can distinguish e.g. 401 from 500.
type agentError struct {
	status int
	msg    string
}

func (e *agentError) Error() string {
	return fmt.Sprintf("agent: %s (status %d)", e.msg, e.status)
}

// newRequest builds an HTTP request against a.baseURL+path with the bearer
// auth header set. body may be nil.
func (a *AgentNode) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	return req, nil
}

// doJSON POSTs reqBody (JSON-encoded) to path and decodes a JSON response
// into respBody (which may be nil if the caller doesn't need the body). On a
// non-2xx response it decodes a ErrorResponse and returns it wrapped in
// an *agentError.
func (a *AgentNode) doJSON(ctx context.Context, path string, reqBody, respBody any) error {
	b, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	req, err := a.newRequest(ctx, http.MethodPost, path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeAgentError(resp)
	}
	if respBody == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(respBody)
}

// decodeAgentError decodes resp's body as a ErrorResponse and returns it
// as an *agentError carrying resp's status code.
func decodeAgentError(resp *http.Response) error {
	var er ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return &agentError{status: resp.StatusCode, msg: fmt.Sprintf("unreadable error body: %v", err)}
	}
	return &agentError{status: resp.StatusCode, msg: er.Error}
}

// Ping hits GET /healthz. No auth is required by the server, but sending the
// bearer header is harmless since the server ignores auth on this path.
func (a *AgentNode) Ping(ctx context.Context) error {
	req, err := a.newRequest(ctx, http.MethodGet, PathHealthz, nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return decodeAgentError(resp)
	}
	return nil
}

func (a *AgentNode) EnsureImage(ctx context.Context, imageRef string) error {
	return a.doJSON(ctx, PathEnsureImage, EnsureImageRequest{Ref: imageRef}, nil)
}

func (a *AgentNode) EnsureNetwork(ctx context.Context, name string) error {
	return a.doJSON(ctx, PathEnsureNetwork, EnsureNetworkRequest{Name: name}, nil)
}

func (a *AgentNode) RunContainer(ctx context.Context, spec RunSpec) (Container, error) {
	var resp RunResponse
	if err := a.doJSON(ctx, PathRun, RunRequest{Spec: spec}, &resp); err != nil {
		return Container{}, err
	}
	return resp.Container, nil
}

func (a *AgentNode) RemoveContainer(ctx context.Context, id string) error {
	return a.doJSON(ctx, PathRemove, RemoveRequest{ID: id}, nil)
}

func (a *AgentNode) ListContainers(ctx context.Context, labels map[string]string) ([]Container, error) {
	var resp ListResponse
	if err := a.doJSON(ctx, PathList, ListRequest{Labels: labels}, &resp); err != nil {
		return nil, err
	}
	return resp.Containers, nil
}

func (a *AgentNode) Health(ctx context.Context, c Container, h HealthSpec) error {
	return a.doJSON(ctx, PathHealth, HealthCheckRequest{Container: c, Health: h}, nil)
}

func (a *AgentNode) ContainerHealth(ctx context.Context, id string) (state string, dockerHealth string, err error) {
	var resp ContainerHealthResponse
	if err := a.doJSON(ctx, PathContainerHealth, ContainerHealthRequest{ID: id}, &resp); err != nil {
		return "", "", err
	}
	return resp.State, resp.DockerHealth, nil
}

func (a *AgentNode) ConnectContainerToNetwork(ctx context.Context, containerID, network string) error {
	return a.doJSON(ctx, PathConnectNetwork, ConnectNetworkRequest{ContainerID: containerID, Network: network}, nil)
}

func (a *AgentNode) ImagePresent(ctx context.Context, ref string) (bool, error) {
	var resp ImagePresentResponse
	if err := a.doJSON(ctx, PathImagePresent, ImagePresentRequest{Ref: ref}, &resp); err != nil {
		return false, err
	}
	return resp.Present, nil
}

// ContainerLogs GETs /logs?id=..&follow=.. and returns the response body as
// the log stream. The caller owns the returned io.ReadCloser and must Close
// it. On a non-2xx response the body is closed here and an error returned.
func (a *AgentNode) ContainerLogs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	q := url.Values{}
	q.Set("id", id)
	q.Set("follow", strconv.FormatBool(follow))

	req, err := a.newRequest(ctx, http.MethodGet, PathLogs+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, decodeAgentError(resp)
	}
	return resp.Body, nil
}

// SaveImage GETs /save?ref=.. and returns the response body as a tar stream.
// The caller owns the returned io.ReadCloser and must Close it. On a non-2xx
// response the body is closed here and an error returned.
func (a *AgentNode) SaveImage(ctx context.Context, ref string) (io.ReadCloser, error) {
	q := url.Values{}
	q.Set("ref", ref)

	req, err := a.newRequest(ctx, http.MethodGet, PathSave+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, decodeAgentError(resp)
	}
	return resp.Body, nil
}

// LoadImage POSTs the raw tar stream r to /load.
func (a *AgentNode) LoadImage(ctx context.Context, r io.Reader) error {
	req, err := a.newRequest(ctx, http.MethodPost, PathLoad, r)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return decodeAgentError(resp)
	}
	return nil
}
