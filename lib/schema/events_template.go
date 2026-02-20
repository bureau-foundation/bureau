// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import "github.com/bureau-foundation/bureau/lib/ref"

// PrincipalAssignment defines a single principal that should run on a machine.
type PrincipalAssignment struct {
	// Principal identifies this principal as a fleet-scoped entity reference.
	// Serialized as the full Matrix user ID (e.g.,
	// "@bureau/fleet/prod/agent/frontend:bureau.local"). Deserialized via
	// ref.Entity.UnmarshalText which validates the user ID and populates all
	// derived fields (Localpart, Fleet, Name, EntityType, etc.).
	Principal ref.Entity `json:"principal"`

	// Template is a template reference identifying which template to
	// instantiate for this principal. The format is:
	//
	//   <room-alias-localpart>:<template-name>
	//
	// Examples:
	//   - "bureau/template:base" — built-in base template
	//   - "bureau/template:llm-agent" — built-in agent template
	//   - "iree/template:amdgpu-developer" — project-specific template
	//
	// For federated deployments, an optional @server suffix on the room
	// reference specifies the homeserver:
	//
	//   "iree/template@other.example:amdgpu-developer"
	//
	// The daemon resolves this reference to a room alias and state event,
	// walks the inheritance chain, and produces a fully-resolved SandboxSpec.
	// See ParseTemplateRef for parsing semantics.
	Template string `json:"template"`

	// AutoStart controls whether the daemon should start this principal's
	// sandbox immediately at boot, or wait for a wake event (message,
	// service request, etc.).
	AutoStart bool `json:"auto_start"`

	// Labels are free-form key-value metadata for organizational purposes:
	// observation layout filtering, dashboard grouping, UI categorization.
	// Labels do not affect access control — that's handled by the
	// authorization policy and localpart glob patterns. Changing labels does not
	// require a sandbox restart; the daemon picks up label changes on the
	// next config sync.
	//
	// Common conventions:
	//   - "role": "agent", "service", "coordinator" — principal function
	//   - "team": "iree", "infra" — organizational grouping
	//   - "tier": "gpu", "cpu" — resource classification
	Labels map[string]string `json:"labels,omitempty"`

	// MatrixPolicy controls which self-service Matrix operations this
	// principal's proxy allows. The homeserver enforces room membership
	// and power levels; this policy controls whether the agent can change
	// its own membership (join rooms, invite others, create rooms).
	// When nil or zero-valued, all self-service membership operations are
	// blocked — the agent can only interact with rooms the admin placed
	// it in.
	MatrixPolicy *MatrixPolicy `json:"matrix_policy,omitempty"`

	// ServiceVisibility is a list of glob patterns that control which
	// services this principal can discover via GET /v1/services. Patterns
	// match against service localparts using Bureau's hierarchical glob
	// syntax:
	//   - "service/stt/*" — all STT services
	//   - "service/**" — all services under service/
	//   - "**" — all services (use with caution)
	//
	// An empty or nil list means the principal cannot see any services
	// (default-deny). The daemon pushes these patterns to the principal's
	// proxy, which filters the directory before returning results.
	ServiceVisibility []string `json:"service_visibility,omitempty"`

	// Authorization is the full authorization policy for this principal.
	// Defines what this principal can do (grants, denials) and what
	// others can do to it (allowances, allowance denials).
	//
	// This is the advanced alternative to the shorthand fields above
	// (MatrixPolicy, ServiceVisibility). When set, the daemon uses
	// it for all authorization decisions and ignores the shorthand
	// fields. When absent, the daemon synthesizes grants from the
	// shorthand fields.
	Authorization *AuthorizationPolicy `json:"authorization,omitempty"`

	// CommandOverride replaces the template's Command for this specific
	// principal instance. Use this when the same template should run a
	// different entrypoint on different machines or for different
	// principals (e.g., a template defines the environment but each
	// principal runs a different model). When nil, the template's Command
	// is used unchanged.
	CommandOverride []string `json:"command_override,omitempty"`

	// EnvironmentOverride replaces the template's Environment (Nix store
	// path) for this principal. When empty, the template's Environment is
	// used. This enables pinning a specific principal to a different Nix
	// closure than the template default — useful for canary deployments
	// or per-principal version pinning.
	EnvironmentOverride string `json:"environment_override,omitempty"`

	// ExtraEnvironmentVariables are merged into the template's
	// EnvironmentVariables map for this principal instance. These are
	// applied after template inheritance resolution, so they override
	// any variable set by the template or its parents. Use this for
	// per-instance configuration that doesn't warrant a separate
	// template (e.g., MODEL_NAME, BATCH_SIZE).
	ExtraEnvironmentVariables map[string]string `json:"extra_environment_variables,omitempty"`

	// Payload is instance-specific data passed to the agent at startup
	// and available for hot-reload via SIGHUP. The daemon merges this
	// over the template's DefaultPayload (Payload values win on
	// conflict) and writes the result to /run/bureau/payload.json
	// inside the sandbox. Use this for per-instance agent configuration:
	// project context, task assignments, model parameters, etc.
	Payload map[string]any `json:"payload,omitempty"`

	// StartCondition gates this principal's launch on the existence of a
	// specific state event in a specific room. When set, the daemon's
	// reconciliation loop checks whether the referenced event exists
	// before creating the sandbox. If the event is absent, the principal
	// is deferred until a subsequent /sync delivers it.
	//
	// The primary use case is workspace lifecycle sequencing: agent
	// principals gate on EventTypeWorkspace (with ContentMatch
	// {"status": "active"}) so they don't start until the setup
	// principal has finished creating the workspace. Other uses
	// include service dependencies, staged rollouts, and manual
	// approval gates.
	//
	// When nil, the principal starts normally (subject to AutoStart).
	StartCondition *StartCondition `json:"start_condition,omitempty"`
}

// StartCondition specifies a state event that must exist (and optionally
// match specific content) before a principal can launch. The daemon checks
// this during reconciliation: resolve the room alias, fetch the state event,
// and proceed only if the event exists and all ContentMatch entries are
// satisfied.
type StartCondition struct {
	// EventType is the Matrix state event type to check for (e.g.,
	// "m.bureau.workspace"). Must be a valid Matrix event type.
	EventType string `json:"event_type"`

	// StateKey is the state key to look up. Empty string is valid and
	// common — it means the singleton instance of that event type.
	StateKey string `json:"state_key"`

	// RoomAlias is the full Matrix room alias where the event should
	// exist (e.g., "#iree/amdgpu/inference:bureau.local"). The daemon
	// resolves this to a room ID (cached) before fetching the event.
	// When empty, the daemon checks the principal's own config room.
	RoomAlias string `json:"room_alias,omitempty"`

	// ContentMatch specifies criteria that must all be satisfied in the
	// event content for the condition to pass. When nil or empty, only
	// event existence is checked.
	//
	// Each key names a field in the event content. Each value is either
	// a bare scalar (shorthand for equality) or a $-prefixed operator
	// object supporting comparisons and set membership. Multiple
	// operators on the same field AND together (enabling ranges).
	//
	// Bare values (backward compatible):
	//   {"status": "active"}            — string equality
	//   {"priority": 2}                 — numeric equality
	//
	// Operators:
	//   {"priority": {"$lte": 2}}       — comparison
	//   {"priority": {"$gte": 1, "$lte": 3}} — range
	//   {"hour": {"$in": [9, 10, 11]}}  — set membership
	//
	// For array targets, all operators evaluate per-element: a match
	// succeeds if any element in the array satisfies the criterion.
	ContentMatch ContentMatch `json:"content_match,omitempty"`
}

// MatrixPolicy controls which self-service Matrix operations an agent can
// perform through its proxy. The homeserver enforces room membership and
// power levels; this is a defense-in-depth layer that prevents agents from
// expanding their own access.
//
// Default (all false): the agent can only interact with rooms it was
// explicitly placed in by the admin. It cannot join new rooms, accept
// invitations, invite others, or create rooms.
type MatrixPolicy struct {
	// AllowJoin permits the agent to join public rooms and accept room
	// invitations from other agents. When false, the proxy blocks all
	// POST /join requests. Enables general-purpose agents that discover
	// rooms via space membership and accept invitations from coordinator
	// agents.
	AllowJoin bool `json:"allow_join,omitempty"`

	// AllowInvite permits the agent to invite other agents to rooms it
	// is a member of. Enables coordinator/secretary agents that assemble
	// teams and page in other agents.
	AllowInvite bool `json:"allow_invite,omitempty"`

	// AllowRoomCreate permits the agent to create new rooms (e.g.,
	// backchannel rooms for ad-hoc collaboration between agents).
	AllowRoomCreate bool `json:"allow_room_create,omitempty"`
}

// TemplateContent is the content of an EventTypeTemplate state event. It
// defines the complete sandbox specification for a class of principals:
// filesystem mounts, namespace isolation, resource limits, security settings,
// entrypoint command, environment variables, and agent roles.
//
// This is the Matrix wire-format representation. The sandbox package defines
// parallel runtime types (sandbox.Profile, sandbox.Mount, etc.) with YAML
// tags for local file authoring. The daemon converts between the two when
// resolving templates. The same pattern is used for LayoutContent vs
// observe.Layout.
//
// Templates can inherit from multiple parents via the Inherits field. The
// daemon resolves each parent independently and merges them left-to-right:
// later parents override earlier parents for conflicting scalars and map
// keys, slices are concatenated, and the child template is applied last
// (overrides everything). Slices (Filesystem, CreateDirs) are appended;
// maps (EnvironmentVariables, Roles, DefaultPayload) are merged with child
// values winning; scalars (Command, Environment) are replaced if non-zero
// in the child.
type TemplateContent struct {
	// Description is a human-readable summary of what this template
	// provides (e.g., "GPU-accelerated LLM agent with IREE runtime").
	Description string `json:"description,omitempty"`

	// Inherits is an ordered list of parent template references. The
	// daemon resolves each parent independently and merges left-to-right:
	// later parents override earlier parents for conflicting scalar
	// fields and map keys. Slices are concatenated. The child template
	// is applied last (overrides everything).
	//
	// Single-parent inheritance is a one-element list. Empty means no
	// inheritance (self-contained template). Format for each entry is
	// the same as PrincipalAssignment.Template:
	// "room-alias-localpart:template-name". Cycles are detected and
	// rejected.
	Inherits []string `json:"inherits,omitempty"`

	// Command is the entrypoint command and arguments to run inside the
	// sandbox. The first element is the executable path; subsequent
	// elements are arguments. Empty inherits from the parent template.
	Command []string `json:"command,omitempty"`

	// Environment is the Nix store path providing the sandbox's /usr/local
	// toolchain (e.g., "/nix/store/abc123-bureau-agent-env"). Resolved at
	// template publish time, not at runtime. The daemon prefetches missing
	// store paths from the Attic binary cache before creating the sandbox.
	// Empty means no Nix environment (use host /usr/bin via bind mounts).
	Environment string `json:"environment,omitempty"`

	// EnvironmentVariables are environment variables set inside the sandbox.
	// During inheritance, child values override parent values for the same
	// key. Values may contain ${VARIABLE} references expanded at sandbox
	// creation time. Available variables include ${WORKSPACE_ROOT},
	// ${PROJECT}, ${WORKTREE_PATH}, ${CACHE_ROOT}, ${PROXY_SOCKET},
	// ${TERM}, ${MACHINE_NAME}, and ${SERVER_NAME}. Workspace variables
	// (PROJECT, WORKTREE_PATH) are populated from the principal's payload.
	EnvironmentVariables map[string]string `json:"environment_variables,omitempty"`

	// Filesystem is the list of mount points for the sandbox. During
	// inheritance, child mounts are appended after parent mounts.
	// The sandbox package's MergeProfiles handles deduplication by Dest.
	Filesystem []TemplateMount `json:"filesystem,omitempty"`

	// Namespaces controls which Linux namespaces are unshared for the
	// sandbox. When nil, inherits from the parent template.
	Namespaces *TemplateNamespaces `json:"namespaces,omitempty"`

	// Resources sets resource limits (cgroup constraints) for the sandbox.
	// When nil, inherits from the parent template.
	Resources *TemplateResources `json:"resources,omitempty"`

	// Security configures sandbox security options (setsid, PR_SET_NO_NEW_PRIVS,
	// etc.). When nil, inherits from the parent template.
	Security *TemplateSecurity `json:"security,omitempty"`

	// CreateDirs lists directories to create inside the sandbox before
	// starting the command. During inheritance, child dirs are appended
	// after parent dirs (duplicates are removed).
	CreateDirs []string `json:"create_dirs,omitempty"`

	// Roles maps role names to their commands. The observation layout
	// system uses roles to determine what each pane shows (e.g., "agent"
	// runs the main agent process, "shell" runs a debug shell). Each
	// role's value is a command array (same format as Command).
	Roles map[string][]string `json:"roles,omitempty"`

	// RequiredCredentials lists the credential names that must be present
	// in the principal's encrypted credential bundle before the sandbox
	// can start. The daemon checks this against the Credentials state
	// event and refuses to start the sandbox if any are missing.
	RequiredCredentials []string `json:"required_credentials,omitempty"`

	// RequiredServices lists service roles (e.g., "ticket", "rag")
	// that must be available when a sandbox is created from this
	// template. The daemon resolves each role to a concrete service
	// principal by reading m.bureau.room_service state events in the
	// principal's rooms, then bind-mounts the service's Unix socket
	// into the sandbox at /run/bureau/service/<role>.sock. If any
	// required service cannot be resolved, sandbox creation fails.
	//
	// During template inheritance, child RequiredServices are appended
	// to parent RequiredServices (duplicates removed).
	RequiredServices []string `json:"required_services,omitempty"`

	// DefaultPayload is the default agent payload merged under
	// PrincipalAssignment.Payload at resolution time (instance values
	// win on conflict). Written to /run/bureau/payload.json inside the
	// sandbox. Use this for template-level defaults that individual
	// instances can override.
	DefaultPayload map[string]any `json:"default_payload,omitempty"`

	// HealthCheck configures automated health monitoring for principals
	// created from this template. When set, the daemon polls the
	// configured HTTP endpoint through the proxy admin socket and
	// triggers rollback on sustained failure. During template
	// inheritance, child HealthCheck replaces parent HealthCheck
	// entirely (no field-level merge — the parameters form a coherent
	// unit). When nil, no health monitoring is performed.
	HealthCheck *HealthCheck `json:"health_check,omitempty"`

	// ProxyServices declares external HTTP API upstreams that the proxy
	// should be configured to forward. The daemon registers these on
	// the principal's proxy after sandbox creation, enabling credential
	// injection without exposing API keys to the sandboxed agent.
	//
	// During template inheritance, ProxyServices maps are merged with
	// child values winning on conflict (same semantics as
	// EnvironmentVariables and Roles).
	//
	// Example:
	//
	//   "proxy_services": {
	//     "anthropic": {
	//       "upstream": "https://api.anthropic.com",
	//       "inject_headers": {"x-api-key": "ANTHROPIC_API_KEY"},
	//       "strip_headers": ["x-api-key", "authorization"]
	//     }
	//   }
	//
	// Inside the sandbox, agents reach the service at
	// /http/<service-name>/... on the proxy socket (directly via Unix
	// socket clients, or via the bridge's TCP endpoint for HTTP clients
	// that require a URL).
	ProxyServices map[string]TemplateProxyService `json:"proxy_services,omitempty"`
}

// TemplateProxyService declares an external HTTP API upstream that the
// proxy should forward requests to with credential injection. The daemon
// registers each service on the principal's proxy via the admin API
// after sandbox creation.
type TemplateProxyService struct {
	// Upstream is the base URL of the external API
	// (e.g., "https://api.anthropic.com"). Required.
	Upstream string `json:"upstream"`

	// InjectHeaders maps HTTP header names to credential names. When
	// forwarding a request, the proxy reads each credential from the
	// principal's credential bundle and sets it as the specified header.
	// Example: {"x-api-key": "ANTHROPIC_API_KEY"} injects the
	// ANTHROPIC_API_KEY credential as the x-api-key header.
	InjectHeaders map[string]string `json:"inject_headers,omitempty"`

	// StripHeaders lists HTTP headers to remove from incoming requests
	// before forwarding. Use this to prevent agents from injecting
	// their own authentication headers.
	StripHeaders []string `json:"strip_headers,omitempty"`
}

// HealthCheck configures automated health monitoring for principals
// created from a template. The daemon polls the health endpoint through
// the principal's proxy admin socket. If consecutive failures exceed the
// threshold, the daemon triggers a rollback to the previously working
// sandbox configuration.
//
// Health checks are optional. Templates for services (databases, API
// servers, webhook relays) should define health checks; templates for
// interactive agents typically should not.
type HealthCheck struct {
	// Endpoint is the HTTP path to poll (e.g., "/health", "/healthz",
	// "/api/v1/status"). The daemon sends HTTP GET requests to this
	// path through the proxy admin socket.
	Endpoint string `json:"endpoint"`

	// IntervalSeconds is the polling interval in seconds. The daemon
	// sends one health check request per interval. Must be positive.
	IntervalSeconds int `json:"interval_seconds"`

	// TimeoutSeconds is the per-request timeout in seconds. A request
	// that does not receive HTTP 200 within this duration counts as a
	// failure. Zero defaults to 5 seconds at runtime.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`

	// FailureThreshold is the number of consecutive failures before
	// the daemon considers the principal unhealthy and triggers a
	// rollback to the previous working sandbox configuration. Zero
	// defaults to 3 at runtime.
	FailureThreshold int `json:"failure_threshold,omitempty"`

	// GracePeriodSeconds is the delay after sandbox creation before
	// the first health check. Gives the service time to initialize
	// (load models, warm caches, bind ports). Zero defaults to 30
	// seconds at runtime.
	GracePeriodSeconds int `json:"grace_period_seconds,omitempty"`
}

// TemplateMount describes a filesystem mount point in a template. This is
// the JSON wire-format counterpart to sandbox.Mount (which uses YAML tags).
// The daemon converts between the two during template resolution.
type TemplateMount struct {
	// Source is the host path to mount. Supports ${VARIABLE} expansion
	// (e.g., "${WORKSPACE_ROOT}/${PROJECT}" for a project workspace directory).
	// Empty when Type is "tmpfs" or other virtual filesystem.
	Source string `json:"source,omitempty"`

	// Dest is the mount destination path inside the sandbox. Required.
	Dest string `json:"dest"`

	// Type is the filesystem type. Empty means bind mount (the common
	// case). Other values: "tmpfs" for a temporary filesystem.
	Type string `json:"type,omitempty"`

	// Mode is the access mode: "ro" for read-only, "rw" for read-write.
	// Empty defaults to "ro" in the sandbox package.
	Mode string `json:"mode,omitempty"`

	// Options are mount-specific options (e.g., "size=64M" for tmpfs).
	Options string `json:"options,omitempty"`

	// Optional marks this mount as non-fatal if the source does not
	// exist. When true, the sandbox skips this mount silently rather
	// than failing. Use for paths that exist on some machines but not
	// others (e.g., /lib64 on non-multilib systems, /nix on non-Nix
	// machines).
	Optional bool `json:"optional,omitempty"`
}

// TemplateNamespaces controls which Linux namespaces are unshared for the
// sandbox. Each field corresponds to a bwrap --unshare-* flag.
type TemplateNamespaces struct {
	// PID unshares the PID namespace (--unshare-pid). Processes inside
	// the sandbox see their own PID 1.
	PID bool `json:"pid,omitempty"`

	// Net unshares the network namespace (--unshare-net). The sandbox
	// gets an isolated network stack with only loopback. External
	// network access goes through the bridge/proxy.
	Net bool `json:"net,omitempty"`

	// IPC unshares the IPC namespace (--unshare-ipc). Isolates System V
	// IPC and POSIX message queues.
	IPC bool `json:"ipc,omitempty"`

	// UTS unshares the UTS namespace (--unshare-uts). The sandbox gets
	// its own hostname.
	UTS bool `json:"uts,omitempty"`
}

// TemplateResources defines resource limits for the sandbox, enforced via
// cgroup constraints when systemd user scopes are available.
type TemplateResources struct {
	// CPUShares is the relative CPU weight (cgroup cpu.shares). Higher
	// values get proportionally more CPU time under contention. Zero
	// means no limit (use cgroup default).
	CPUShares int `json:"cpu_shares,omitempty"`

	// MemoryLimitMB is the maximum memory in megabytes (cgroup
	// memory.max). Zero means no limit.
	MemoryLimitMB int `json:"memory_limit_mb,omitempty"`

	// PidsLimit is the maximum number of processes (cgroup pids.max).
	// Zero means no limit.
	PidsLimit int `json:"pids_limit,omitempty"`
}

// TemplateSecurity configures sandbox security options passed to bwrap.
type TemplateSecurity struct {
	// NewSession calls setsid(2) to create a new session for the sandbox
	// process, detaching it from the controlling terminal. This prevents
	// the sandboxed process from sending signals to the parent process
	// group.
	NewSession bool `json:"new_session,omitempty"`

	// DieWithParent sets PR_SET_PDEATHSIG so the sandbox process receives
	// SIGKILL when its parent dies. Prevents orphaned sandbox processes.
	DieWithParent bool `json:"die_with_parent,omitempty"`

	// NoNewPrivs sets PR_SET_NO_NEW_PRIVS, preventing the sandbox process
	// (and its children) from gaining privileges through execve of setuid
	// binaries.
	NoNewPrivs bool `json:"no_new_privs,omitempty"`
}
