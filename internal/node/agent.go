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
// NewAgentNode's client deliberately has no client-wide Timeout: that field
// bounds an ENTIRE exchange, including streaming bodies, so it would abort
// long-running SaveImage/LoadImage transfers (docker save|load'd
// lwd-build/<app>:<sha> images with no registry) partway through. Every
// method here instead governs its own lifetime via the caller's ctx
// (http.NewRequestWithContext), matching the unbounded-by-default,
// ctx-governed lifetime of the docker-over-ssh transport. The one exception
// is Ping, which imposes its own short bound (see Ping) since it exists
// purely as a fast reachability/readiness probe.
func NewAgentNode(baseURL, token string) *AgentNode {
	return &AgentNode{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{},
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

// pingTimeout bounds how long Ping waits for the agent's authenticated
// readiness probe (PathReady) to answer, regardless of ctx's own deadline —
// Ping is used by transport selection (RegistryResolver.buildTransport, via
// pingOK) as a fast "is this usable" check, not a long-running operation.
const pingTimeout = 5 * time.Second

// Ping hits GET /ready WITH the bearer token — unlike /healthz, /ready is not
// exempt from authMiddleware, so a 200 here means both "the agent is up" and
// "my token works". That is the contract node.Node.Ping needs for this
// transport: RegistryResolver.buildTransport treats any Ping error —
// including a 401 from a wrong/missing LWD_AGENT_TOKEN — as "agent
// unavailable" and falls back to docker-over-ssh. /healthz remains served by
// the agent, unauthenticated, purely for external liveness probes; AgentNode
// itself no longer uses it.
func (a *AgentNode) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	req, err := a.newRequest(ctx, http.MethodGet, PathReady, nil)
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

// Capacity is not yet implemented for AgentNode: Phase 11a Task 3 wires this
// up to a real agent-side endpoint. This stub exists only so AgentNode keeps
// satisfying the Node interface in the interim.
func (a *AgentNode) Capacity(ctx context.Context) (Capacity, error) {
	return Capacity{}, fmt.Errorf("AgentNode.Capacity not implemented (Task 3)")
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
