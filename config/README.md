# Configuration

This directory contains configuration templates and schemas for Bureau deployments.

**Note**: This is the *source repository*. Actual deployment configurations live outside this tree (typically `/opt/bureau/config/` or similar). These files are templates and examples.

## Structure

### `agents/`

Agent definition templates:

- `_templates/` — Starter templates for new agents
- Example agent manifests and AGENT.md files

### `policies/`

OPA (Open Policy Agent) policies:

- Capability checking rules
- Approval workflow policies
- Cost limit enforcement

### `environments/`

Environment-specific configuration:

- `sandbox.yaml` — Local development/testing
- `staging.yaml` — Pre-production validation
- `production.yaml` — Production settings

## Configuration Files

Bureau uses several configuration files:

| File | Purpose |
|------|---------|
| `bureau.yaml` | Core service configuration |
| `budget.yaml` | Cost limits and alerts |
| `alerts.yaml` | Monitoring alert definitions |
| `services.yaml` | External service configuration |

## Schema

Configuration schemas are defined in `pkg/protocol/` and validated on load.

## Hot Reload

Most configuration changes can be applied without restart:

- Agent manifests and prompts
- Policies
- Budget limits
- Knowledge files

Code changes require a full restart.
