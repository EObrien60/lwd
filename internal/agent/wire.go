// Package agent defines the wire contract shared by the lwd-agent HTTP
// server and the agentNode HTTP client: request/response DTOs that wrap
// node.Node's primitives, and the HTTP path constants both sides dial
// against. Keeping these in one file/package means the server and client
// can't drift on field names or paths.
package agent

import "lwd/internal/node"

// HTTP path constants shared by the agent server and client.
const (
	PathHealthz         = "/healthz"
	PathRun             = "/run"
	PathRemove          = "/remove"
	PathList            = "/list"
	PathEnsureImage     = "/ensure-image"
	PathImagePresent    = "/image-present"
	PathLoad            = "/load" // tar stream body, not a JSON DTO
	PathSave            = "/save" // tar stream body, not a JSON DTO
	PathLogs            = "/logs" // streamed body, not a JSON DTO
	PathEnsureNetwork   = "/ensure-network"
	PathConnectNetwork  = "/connect-network"
	PathContainerHealth = "/container-health"
	PathHealth          = "/health"
)

// RunRequest is the body of a POST PathRun request.
type RunRequest struct {
	Spec node.RunSpec `json:"spec"`
}

// RunResponse is the body of a PathRun response.
type RunResponse struct {
	Container node.Container `json:"container"`
}

// RemoveRequest is the body of a POST PathRemove request.
type RemoveRequest struct {
	ID string `json:"id"`
}

// ListRequest is the body of a POST PathList request.
type ListRequest struct {
	Labels map[string]string `json:"labels"`
}

// ListResponse is the body of a PathList response.
type ListResponse struct {
	Containers []node.Container `json:"containers"`
}

// EnsureImageRequest is the body of a POST PathEnsureImage request.
type EnsureImageRequest struct {
	Ref string `json:"ref"`
}

// ImagePresentRequest is the body of a POST PathImagePresent request.
type ImagePresentRequest struct {
	Ref string `json:"ref"`
}

// ImagePresentResponse is the body of a PathImagePresent response.
type ImagePresentResponse struct {
	Present bool `json:"present"`
}

// EnsureNetworkRequest is the body of a POST PathEnsureNetwork request.
type EnsureNetworkRequest struct {
	Name string `json:"name"`
}

// ConnectNetworkRequest is the body of a POST PathConnectNetwork request.
type ConnectNetworkRequest struct {
	ContainerID string `json:"containerId"`
	Network     string `json:"network"`
}

// ContainerHealthRequest is the body of a POST PathContainerHealth request.
type ContainerHealthRequest struct {
	ID string `json:"id"`
}

// ContainerHealthResponse is the body of a PathContainerHealth response.
type ContainerHealthResponse struct {
	State        string `json:"state"`
	DockerHealth string `json:"dockerHealth"`
}

// HealthCheckRequest is the body of a POST PathHealth request. Health.Timeout
// is a time.Duration, which encoding/json marshals as an int64 count of
// nanoseconds; that round-trips losslessly, it's just carried as-is (not
// re-encoded as a duration string).
type HealthCheckRequest struct {
	Container node.Container  `json:"container"`
	Health    node.HealthSpec `json:"health"`
}

// ErrorResponse is the JSON body returned for non-2xx responses.
type ErrorResponse struct {
	Error string `json:"error"`
}
