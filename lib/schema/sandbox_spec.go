// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

// SandboxSpec is a fully-resolved sandbox configuration produced by the
// daemon's template resolution pipeline. The daemon fetches templates from
// Matrix, walks the inheritance chain, applies PrincipalAssignment instance
// overrides, and emits a SandboxSpec. The launcher receives this as a
// complete, self-contained specification — no template knowledge, no Matrix
// access, no inheritance resolution needed.
//
// SandboxSpec is transmitted as JSON over the daemon→launcher IPC socket
// as part of the create-sandbox request. It reuses the Template* types from
// this package for filesystem, namespace, resource, and security config.
//
// The launcher converts SandboxSpec fields into sandbox.Profile fields and
// bwrap command-line arguments. The mapping is straightforward:
//
//   - Filesystem → sandbox.Mount slice (same structure, different tags)
//   - Namespaces → sandbox.NamespaceConfig
//   - Resources → sandbox.ResourceConfig
//   - Security → sandbox.SecurityConfig
//   - EnvironmentVariables → sandbox.Profile.Environment
//   - Command → argv for bwrap's -- separator
//   - EnvironmentPath → bind-mount to /usr/local, prepend /usr/local/bin to PATH
//   - Payload → JSON written to /run/bureau/payload.json inside sandbox
//   - Roles → passed to tmux session creation for layout resolution
type SandboxSpec struct {
	// Command is the entrypoint command and arguments to exec inside the
	// sandbox. The first element is the executable path; subsequent
	// elements are arguments. This is the final resolved command after
	// template inheritance and any CommandOverride from the
	// PrincipalAssignment. Must not be empty.
	Command []string `json:"command"`

	// Filesystem is the fully-merged list of mount points. Template
	// inheritance appends parent mounts before child mounts; duplicates
	// by Dest have been resolved (child wins). The launcher converts
	// these to sandbox.Mount and passes them to bwrap.
	Filesystem []TemplateMount `json:"filesystem,omitempty"`

	// Namespaces specifies which Linux namespaces to unshare. The
	// launcher maps these to bwrap --unshare-* flags.
	Namespaces *TemplateNamespaces `json:"namespaces,omitempty"`

	// Resources specifies cgroup resource limits. The launcher applies
	// these via systemd-run --user --scope when systemd user scopes are
	// available, or ignores them with a warning when they aren't.
	Resources *TemplateResources `json:"resources,omitempty"`

	// Security specifies bwrap security options (setsid, die-with-parent,
	// no-new-privs).
	Security *TemplateSecurity `json:"security,omitempty"`

	// EnvironmentVariables is the complete set of environment variables
	// for the sandbox process, after merging template variables with
	// PrincipalAssignment.ExtraEnvironmentVariables. Variable references
	// (${WORKSPACE_ROOT}, ${PROJECT}, ${TERM}, etc.) have NOT been
	// expanded — the launcher expands them at sandbox creation time when
	// concrete values are known. Workspace variables (PROJECT,
	// WORKTREE_PATH) are extracted from SandboxSpec.Payload.
	EnvironmentVariables map[string]string `json:"environment_variables,omitempty"`

	// EnvironmentPath is the Nix store path providing the sandbox's
	// toolchain (e.g., "/nix/store/abc123-bureau-agent-env"). When set,
	// the launcher bind-mounts it read-only and prepends its bin/ to
	// PATH. The daemon prefetches this path from the Attic binary cache
	// before sending the SandboxSpec. Empty means no Nix environment.
	EnvironmentPath string `json:"environment_path,omitempty"`

	// Payload is the merged agent payload (template DefaultPayload
	// overridden by PrincipalAssignment.Payload). The launcher writes
	// this as JSON to /run/bureau/payload.json inside the sandbox. The
	// agent reads it at startup and can watch for SIGHUP to reload when
	// the daemon hot-updates it.
	Payload map[string]any `json:"payload,omitempty"`

	// Roles maps role names to their commands (e.g., "agent" →
	// ["/usr/local/bin/claude", "--agent"], "shell" → ["/bin/bash"]).
	// The launcher uses this when creating the tmux session: the layout
	// specifies panes by role name, and the launcher resolves each role
	// to its concrete command from this map.
	Roles map[string][]string `json:"roles,omitempty"`

	// CreateDirs lists directories to create inside the sandbox before
	// executing the command. The launcher creates these with mode 0755
	// after applying filesystem mounts but before exec. Duplicates have
	// been removed during template resolution.
	CreateDirs []string `json:"create_dirs,omitempty"`

	// RequiredServices lists service roles (e.g., "ticket", "rag")
	// that must be resolved to concrete service sockets before sandbox
	// creation. The daemon reads this after template resolution,
	// resolves each role via m.bureau.service_binding state events in the
	// principal's rooms, and passes the resolved socket paths to the
	// launcher as ServiceMounts. If any role cannot be resolved,
	// sandbox creation fails.
	RequiredServices []string `json:"required_services,omitempty"`

	// ProxyServices declares external HTTP API upstreams that the proxy
	// should forward with credential injection. The daemon registers
	// these on the principal's proxy after sandbox creation. The
	// launcher does not use this field — it exists in SandboxSpec so
	// that structurallyChanged() (which uses JSON serialization)
	// detects proxy service changes and triggers sandbox restarts.
	ProxyServices map[string]TemplateProxyService `json:"proxy_services,omitempty"`
}
