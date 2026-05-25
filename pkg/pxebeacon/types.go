// Package pxebeacon holds the public wire types for the pxe-beacon
// JSON API (everything under /api/v1/*). It is a dependency-free leaf
// package: SDK authors, Terraform providers, and UI builders import
// these structs and the ErrCode constants instead of re-declaring the
// wire shapes, which would silently drift from the server.
//
// The types are plain data with JSON tags — no methods that touch
// server state, no imports of internal/*. The server (internal/httpd)
// builds and returns these; clients unmarshal into them.
//
// Stability: these types carry the same compatibility contract as the
// JSON API itself (see the versioning policy in the README). Fields
// are added, never renamed or removed within a major API version.
package pxebeacon

import "time"

// Desired is the operator's queued boot intent for a machine's next
// PXE boot. Action is "install", "rescue", or "" (none queued).
type Desired struct {
	Action      string    `json:"action,omitempty"`
	RequestedAt time.Time `json:"requested_at,omitempty"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
}

// Observed is the most recent install-progress state the tracker saw
// for a machine. Phase is a lifecycle event name (e.g. "ipxe-dhcp",
// "installer-done", "installer-failed").
type Observed struct {
	Phase    string    `json:"phase,omitempty"`
	LastSeen time.Time `json:"last_seen,omitempty"`
}

// Machine is the full per-machine view: fleet config plus desired
// intent and observed state. Returned by GET /api/v1/machines/{mac}
// and (per entry) GET /api/v1/machines.
type Machine struct {
	MAC      string   `json:"mac"`
	Name     string   `json:"name,omitempty"`
	Boot     string   `json:"boot,omitempty"`
	Desired  Desired  `json:"desired"`
	Observed Observed `json:"observed"`
}

// Intent is the standalone desired+observed view returned by
// GET /api/v1/machines/{mac}/intent (the K8s subresource shape).
type Intent struct {
	MAC      string   `json:"mac"`
	Desired  Desired  `json:"desired"`
	Observed Observed `json:"observed"`
}

// MachineConfig is the JSON request body for POST /api/v1/machines
// (create) and PUT /api/v1/machines/{mac} (update). On create, MAC is
// taken from the body; on update it comes from the URL path and any
// body MAC is ignored.
type MachineConfig struct {
	MAC        string            `json:"mac,omitempty"`
	Name       string            `json:"name"`
	Boot       string            `json:"boot"`
	Preseed    string            `json:"preseed,omitempty"`
	Kickstart  string            `json:"kickstart,omitempty"`
	CloudInit  string            `json:"cloud_init,omitempty"`
	Rescue     string            `json:"rescue,omitempty"`
	IPXEScript string            `json:"ipxe_script,omitempty"`
	Params     map[string]string `json:"params,omitempty"`
}

// ListResponse is the body of GET /api/v1/machines. Total is the full
// fleet size before paging; Limit/Offset echo the applied window.
type ListResponse struct {
	PendingTTLs int       `json:"pending_ttl_s"`
	Total       int       `json:"total"`
	Limit       int       `json:"limit"`
	Offset      int       `json:"offset"`
	Machines    []Machine `json:"machines"`
}

// ServerInfo is the server-level block in StatusResponse.
type ServerInfo struct {
	AdvertisedIP string `json:"advertised_ip"`
	HTTPPort     int    `json:"http_port"`
	UptimeS      int    `json:"uptime_s"`
	StartedAt    string `json:"started_at"`
	PendingTTLs  int    `json:"pending_ttl_s"`
}

// StatusResponse is the body of the (deprecated) GET /status.json. The
// shape matches GET /api/v1/machines per-entry so UI consumers parse
// one Machine type everywhere.
type StatusResponse struct {
	Server   ServerInfo `json:"server"`
	Machines []Machine  `json:"machines"`
}

// HealthzResponse is the body of GET /healthz (liveness).
type HealthzResponse struct {
	Status    string `json:"status"`
	UptimeS   int    `json:"uptime_s"`
	StartedAt string `json:"started_at"`
}

// ReadyzResponse is the body of GET /readyz (readiness). Status is
// "ok" or "not_ready". Components maps each subsystem to "ok" or a
// not-ready reason. The counts are present only when ready (pointers
// so a legitimate zero is still reported, but a not-ready response
// omits them).
type ReadyzResponse struct {
	Status       string            `json:"status"`
	Components   map[string]string `json:"components"`
	PendingCount *int              `json:"pending_count,omitempty"`
	TrackerCount *int              `json:"tracker_count,omitempty"`
}

// ErrCode is the stable, machine-readable identifier in an APIError.
// Clients should branch on this (a fixed enum), never on the prose
// Message — Message text may be reworded between releases, ErrCode
// values never change meaning. New codes are additive.
type ErrCode string

const (
	ErrCodeFleetNotLoaded       ErrCode = "fleet_not_loaded"
	ErrCodePendingNotConfigured ErrCode = "pending_not_configured"
	ErrCodeMACMissing           ErrCode = "mac_missing"
	ErrCodeMACInvalid           ErrCode = "mac_invalid"
	ErrCodeMACNotInFleet        ErrCode = "mac_not_in_fleet"
	ErrCodeBodyInvalid          ErrCode = "body_invalid"
	ErrCodeActionMissing        ErrCode = "action_missing"
	ErrCodeActionInvalid        ErrCode = "action_invalid"
	ErrCodeCallbackToken        ErrCode = "callback_token_invalid"
	ErrCodePendingFailed        ErrCode = "pending_failed"
	ErrCodePagingInvalid        ErrCode = "paging_invalid"
	ErrCodeContentType          ErrCode = "content_type_unsupported"
	ErrCodeMACExists            ErrCode = "mac_exists"
	ErrCodeBootInvalid          ErrCode = "boot_invalid"
	ErrCodeValidationFailed     ErrCode = "validation_failed"
	ErrCodePreconditionRequired ErrCode = "precondition_required"
	ErrCodePreconditionFailed   ErrCode = "precondition_failed"
	ErrCodeSaveFailed           ErrCode = "save_failed"
)

// APIError is the structured error envelope returned by every
// /api/v1/* error response. Branch on Code; Details is optional
// heterogeneous context (accepted values, the offending field, etc.).
type APIError struct {
	Code    ErrCode        `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}
