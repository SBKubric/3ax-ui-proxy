# 3AX-UI proxy fork

Project guide for AI assistants: `.ai/PROJECT.md` (structure, versioning, conventions). Read it first.

## Agent skills

### Issue tracker

Issues live in GitHub Issues of `SBKubric/3ax-ui-proxy`, driven via the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

Default vocabulary: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `CONTEXT.md` and `docs/adr/` at the repo root (created lazily by `/domain-modeling`). See `docs/agents/domain.md`.

### Testing

Legacy code stays untested; a fix in it ships with a unit test reproducing the bug; new functionality ships with unit tests and Playwright e2e tests against the repo's Docker image. See `docs/agents/testing.md` before writing or reviewing tests.
