# Cloudflare Tunnel for Webhook Ingestion

Routes external webhook traffic (GitHub, Forgejo, GitLab) into the Bureau
service mesh via a Cloudflare Tunnel.

## Architecture

```
GitHub ──webhook──▶ Cloudflare Edge ──tunnel──▶ cloudflared
                                                    │
                                     127.0.0.1:8642 (TCP)
                                                    │
                                              bureau-bridge
                                                    │
                                     /run/bureau/service/github-http.sock (Unix)
                                                    │
                                          bureau-github-service
```

The webhook-tunnel template runs cloudflared inside a Bureau sandbox with:
- **bureau-bridge** converting cloudflared's TCP origin to the forge
  connector's Unix webhook socket
- **RequiredServices** mounting the forge connector's HTTP endpoint into
  the sandbox (supports cross-machine routing via transport tunnels)

## Prerequisites

- A Cloudflare account with a domain
- `cloudflared` binary available in the sandbox (via Nix or system PATH)
- A running `github-service` principal in the fleet

## Setup

### 1. Create a Cloudflare Tunnel

Via the Cloudflare Zero Trust dashboard:
1. Navigate to Networks > Tunnels
2. Create a tunnel (type: Cloudflared)
3. Copy the tunnel token

Or via CLI:
```bash
cloudflared tunnel create bureau-webhooks
cloudflared tunnel token bureau-webhooks
```

### 2. Configure Ingress Rules

In the Cloudflare dashboard, add a public hostname entry:
- **Subdomain**: `webhooks` (or your preference)
- **Domain**: your domain
- **Service**: `http://localhost:8642`

For named tunnel configs, see `config.yml.example` in this directory.

### 3. Deploy the Webhook-Tunnel Principal

Assign the webhook-tunnel template to a machine. The github-service can
be on the same machine or a different one — RequiredServices handles
cross-machine routing automatically.

The PrincipalAssignment needs the tunnel token in ExtraEnvironmentVariables:

```json
{
    "type": "m.bureau.principal_assignment",
    "state_key": "webhook-tunnel",
    "content": {
        "template": "bureau/template:webhook-tunnel",
        "extra_environment_variables": {
            "TUNNEL_TOKEN": "<your-tunnel-token>"
        }
    }
}
```

For production deployments, deliver the tunnel token via the credential
bundle rather than ExtraEnvironmentVariables.

### 4. Configure GitHub Webhook

In your GitHub repository or organization settings:
1. Add a webhook
2. **Payload URL**: `https://webhooks.yourdomain.com/`
3. **Content type**: `application/json`
4. **Secret**: the value in your `BUREAU_GITHUB_WEBHOOK_SECRET_FILE`

### 5. Verify

Check the github-service logs for incoming webhook events:
```bash
bureau observe <github-service-principal>
```

The service logs each webhook delivery with event type and delivery ID.

## Multi-Forge Routing

When multiple forge connectors are deployed (github-service, forgejo-service,
gitlab-service), a single tunnel can route to all of them using path-based
ingress rules and multiple bridge instances. Override the webhook-tunnel
template's Command via PrincipalAssignment's CommandOverride, or create
separate tunnel principals per forge connector.
