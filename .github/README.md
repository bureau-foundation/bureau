# GitHub Configuration

GitHub-specific configuration for the Bureau repository.

## Structure

### `workflows/`

GitHub Actions workflows:

- `ci.yaml` — Primary CI (build, test, lint)
- `precommit.yaml` — Pre-commit hook validation
- `release.yaml` — Release automation

### `ISSUE_TEMPLATE/`

Issue templates:

- `bug_report.md` — Bug report template
- `feature_request.md` — Feature request template
- `agent_proposal.md` — New agent proposal

### `CODEOWNERS`

Code ownership for review requirements:

- Core infrastructure requires maintainer approval
- Domain-specific code can have domain owners
- Agent definitions require security review

### `PULL_REQUEST_TEMPLATE.md`

Standard PR template with checklist.

## Branch Protection

The `main` branch is protected:

- Require PR reviews
- Require CI passing
- No force push
- No deletion

See repository settings for current configuration.
