# How `install.sh` resolves a release for `XUI_REPO`, and what the fork's release workflow needs

Resolves: https://github.com/SBKubric/3ax-ui-proxy/issues/5 (part of #2).
Purpose: facts for the "Cut a release tag on the fork" ticket.

Sources are the repo at commit `b1e67026` (`install.sh`, `update.sh`, `x-ui.sh`,
`.github/workflows/release.yml`, `config/version`, `config/config.go`,
`web/service/panel.go`), the live GitHub state of `SBKubric/3ax-ui-proxy` and
`coinman-dev/3ax-ui` (queried via `gh api` on 2026-09-11), GitHub docs, and the
source of `svenstaro/upload-release-action@v2`.

## TL;DR

| Question | Answer |
|---|---|
| Which release does `install.sh` / `update.sh` pick? | No argument: `GET /repos/$XUI_REPO/releases/latest` = newest **non-prerelease, non-draft** release. `--beta`/`--pre`: first entry of `GET /repos/$XUI_REPO/releases` (newest release, pre-release or not). Explicit `vX.Y.Z` argument (`install.sh` only): that tag verbatim. |
| Tag name to push | Must match `v*` (`release.yml:7`). Convention in upstream: `vMAJOR.MINOR.PATCH`. `config/version` does **not** need to match — the binary's version comes from the tag name via `-ldflags`. |
| Does `-proxy.1` make it a pre-release? | **Yes.** `prerelease: ${{ contains(github.ref_name, '-') }}` (`release.yml:216`, `:351`) marks any tag containing `-` as a pre-release, and `releases/latest` skips pre-releases. So `v1.8.1-proxy.1` is installable only with `--beta`/`--pre` or by passing the tag explicitly. |
| What the fork's workflow needs | Nothing extra: `GITHUB_TOKEN` with job-level `permissions: contents: write` (already in `release.yml:51-52`, `:225-226`), no custom secrets, Actions enabled on the fork (already enabled; CI runs succeed). |
| Asset names | `install.sh`/`update.sh` request `x-ui-linux-{amd64,386,arm64,armv7,armv6,armv5,s390x}.tar.gz`; the workflow uploads exactly those seven names plus `x-ui-windows-amd64.zip`. They match. |

Current fork state: **zero releases** (`GET /repos/SBKubric/3ax-ui-proxy/releases/latest` returns 404), only one tag `1.3.3a` (inherited, no `v` prefix, so it never triggered `release.yml`). Until a release exists, `bash <(curl … install.sh)` with the default `XUI_REPO=SBKubric/3ax-ui-proxy` fails with "Failed to fetch x-ui version" (`install.sh:2458-2461`).

---

## 1. How `install.sh` / `update.sh` choose the release

### Repo and branch selection

- `install.sh:49` — `XUI_REPO="${XUI_REPO:-SBKubric/3ax-ui-proxy}"`; same in `update.sh:43` and `x-ui.sh:11`. Every API and download URL below is built from `${XUI_REPO}`.
- `install.sh:41-45` / `update.sh:35-39` — `REPO_BRANCH` is `dev` when the first argument is `--beta`/`--pre`, else `main`. This only affects the auxiliary files fetched from `raw.githubusercontent.com/${XUI_REPO}/${REPO_BRANCH}/…` (`x-ui.sh`, `x-ui.rc`, `x-ui.service.*` — `install.sh:2335, 2383-2389, 2507`; `update.sh:1636-1639, 1722-1773`), not the release binary.

### Local-source short-circuit (no GitHub at all)

`install.sh:26-38` / `update.sh:20-31` — when the script is run from a real checkout (`main.go`, `go.mod`, `web/`, `.git` present next to it) it builds from source and never contacts the releases API (`install.sh:2443-2450`, `update.sh:1431`). `update.sh:1373-1377` stamps the binary with `git describe --tags --always --dirty`, falling back to `v$(cat config/version)`.

### Release-tarball path — three modes (`install_x-ui`, `install.sh:2453-2505`)

1. **No argument** (`install.sh:2453-2467`; `update.sh:1510-1516`):
   ```sh
   tag_version=$(curl -Ls "https://api.github.com/repos/${XUI_REPO}/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
   ```
   then downloads `https://github.com/${XUI_REPO}/releases/download/${tag_version}/x-ui-linux-$(arch).tar.gz` (`install.sh:2464`, `update.sh:1521`). Retries once with `-4`; exits if empty.
   GitHub REST docs, "Get the latest release": *"The latest release is the most recent non-prerelease, non-draft release, sorted by the `created_at` attribute. The `created_at` attribute is the date of the commit used for the release, and not the date when the release was drafted or published."* — https://docs.github.com/en/rest/releases/releases?apiVersion=2022-11-28#get-the-latest-release
   So the default mode is **latest-non-prerelease**, never a pre-release.

2. **`--beta` / `--pre`** (`install.sh:2469-2485`; `update.sh:1503-1509`):
   ```sh
   tag_version=$(curl -Ls "https://api.github.com/repos/${XUI_REPO}/releases" | grep '"tag_name":' | … | head -1)
   ```
   Takes the **first** entry of the list endpoint — the most recently created published release regardless of the `prerelease` flag (drafts are only listed for users with push access, and the anonymous call has none: https://docs.github.com/en/rest/releases/releases?apiVersion=2022-11-28#list-releases). Note this is "newest release", not "newest pre-release": if a stable release is created after a pre-release, `--beta` returns the stable one.

3. **Explicit tag argument** (`install.sh:2486-2505` only; `update.sh` has no such mode):
   `tag_version=$1`; the `v` is stripped only for the floor check `sort -V` against `min_version="1.0.0"` (`install.sh:2488-2496`), but the **download URL uses the tag verbatim** (`install.sh:2499`). Therefore the argument must be the exact tag name, e.g. `v1.8.1-proxy.1`; passing `1.8.1-proxy.1` would 404. `x-ui.sh:199-212` (`x-ui legacy`) wraps this: it asks for `2.4.0`-style input and prepends `v` itself, fetching `install.sh` from the same tag ref.
   `sort -V` orders `1.8.1-proxy.1` after `1.0.0`, so a suffixed tag passes the floor.

### What happens with a `-proxy.1` suffix

- The scripts do no parsing of the tag beyond the above; a suffix matters only through what GitHub says about the release.
- The workflow decides the pre-release flag purely from the tag string: `prerelease: ${{ contains(github.ref_name, '-') }}` (`release.yml:216` Linux, `:351` Windows). `contains()` is a case-insensitive substring test (https://docs.github.com/en/actions/reference/workflows-and-actions/expressions#contains); `github.ref_name` is the short tag name (https://docs.github.com/en/actions/reference/workflows-and-actions/contexts#github-context). `upload-release-action` parses it as `core.getInput('prerelease') == 'true'` (src/main.ts).
- Consequently: `v1.8.1-proxy.1` → pre-release → **skipped by `releases/latest`** → default `install.sh`/`update.sh` will not see it; `--beta`/`--pre` will (while it is the newest); `install.sh v1.8.1-proxy.1` always will.
- A tag without a hyphen (e.g. `v1.8.2`, or `v1.8.1.1` — dots are fine) → full release → picked by the default mode.
- Caveat on "latest" ordering: `/releases/latest` sorts by `created_at`, which is the **commit date**, not the publish date. Since the fork has no releases yet this cannot bite now, but if a later release is cut from an older commit than a previous release, `latest` may not be the one you expect. The Create Release API's `make_latest` (the action sends `make_latest: 'true'` by default — `action.yml` default `"true"`; docs: *"Drafts and prereleases cannot be set as latest. Defaults to true for newly published releases."* — https://docs.github.com/en/rest/releases/releases?apiVersion=2022-11-28#create-a-release) explicitly pins the newly created full release as latest, which sidesteps the ordering issue for full releases.

### The panel's own update check is separate and hard-wired to upstream

`web/service/panel.go:137` calls `https://api.github.com/repos/coinman-dev/3ax-ui/releases/latest` (not `XUI_REPO`) and compares to `config.GetVersion()` (`panel.go:53`). `web/html/index.html:348` links to upstream's releases page. Once the fork ships, say, `v1.8.1-proxy.1`, the dashboard will compare `1.8.1-proxy.1` against upstream `1.8.1` — out of scope here, but worth a follow-up ticket.

## 2. What the "Release 3AX-UI" workflow needs on the fork

### Trigger and tag format

- `release.yml:3-8`: `on: workflow_dispatch`, `push: tags: ["v*"]`, `pull_request`. The `build` and `build-windows` jobs additionally require `if: startsWith(github.ref, 'refs/tags/')` (`release.yml:50`, `:222`).
- So: **the tag must start with `v`** (`1.8.1-proxy.1` without `v` triggers nothing — this is exactly why the inherited `1.3.3a` tag never produced a release). `workflow_dispatch` from a branch runs only `analyze` (the fork's Jun 14 dispatch on `main` did just that); `gh workflow run release.yml --ref v1.8.1-proxy.1` on an existing tag would run the build jobs, since `github.ref` is then `refs/tags/…`.
- `docker.yml:9-11` triggers on `v*.*.*`; `v1.8.1-proxy.1` matches that glob too, so a Docker image is also built and pushed to GHCR (`packages: write`).
- Push-tag docs: https://docs.github.com/en/actions/writing-workflows/choosing-when-your-workflow-runs/events-that-trigger-workflows#push

### `config/version` vs. tag

- `release.yml:116-118` (Linux) and `:288-290` (Windows):
  ```sh
  BUILD_VERSION="${{ github.ref_name }}"
  [ -z "$BUILD_VERSION" ] && BUILD_VERSION=$(cat config/version)
  go build -ldflags "… -X 'github.com/coinman-dev/3ax-ui/v2/config.version=${BUILD_VERSION}'" …
  ```
  On a tag push `github.ref_name` is always set, so **`config/version` is never read in CI**. `config/config.go:44-50` (`GetVersion`) returns the ldflags value with a leading `v` stripped; the embedded `config/version` (`1.8.1`) is only the fallback for `go run` / builds without ldflags.
- `config/version` therefore does **not** have to equal the tag for the release to work. It is good hygiene to bump it (and it is what `update.sh` local-source builds fall back to when `git describe` fails, `update.sh:1376`), but not a requirement.
- Upstream convention: every upstream release tag is `vX.Y.Z` with `prerelease=false` (`v1.0.0` … `v1.8.1`, checked via `gh api repos/coinman-dev/3ax-ui/releases`); upstream has never published a hyphenated tag.

### Permissions — `GITHUB_TOKEN`

- The fork's repository default is restricted: `GET /repos/SBKubric/3ax-ui-proxy/actions/permissions/workflow` → `default_workflow_permissions: "read"`. That is GitHub's default for personal repos: *"By default, when you create a new repository in your personal account, `GITHUB_TOKEN` only has read access for the `contents` and `packages` scopes."* — https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/enabling-features-for-your-repository/managing-github-actions-settings-for-a-repository#setting-the-default-github_token-permissions
- The workflow raises it per job: `permissions: contents: write` on `build` (`release.yml:51-52`) and `build-windows` (`:225-226`); `analyze` keeps `contents: read` (`:16-17`). This is allowed regardless of the restricted default: *"Anyone with write access to a repository can modify the permissions granted to the `GITHUB_TOKEN`, adding or removing access as required, by editing the `permissions` key in the workflow file."* (same page). `contents: write` is the scope that creates releases and uploads assets (https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#permissions). The upload action's README states the same requirement: *"you will need to add the `contents: write` permission to the token using granular permissions."*
- The only case where write is forced down to read is a `pull_request` from a fork: *"if the workflow was triggered by a pull request event other than `pull_request_target` from a forked repository … the permissions are adjusted to change any write permissions to read only."* Irrelevant for a tag push to the fork itself (the fork is the workflow's own repository; nothing here is a PR from a fork), and moot anyway because the build jobs are gated on `refs/tags/`.
- No change to repo Settings → Actions → Workflow permissions is needed.

### Secrets

- The only secret used is `${{ secrets.GITHUB_TOKEN }}` (`release.yml:211`, `:346`), which GitHub provides automatically to every run (https://docs.github.com/en/actions/security-for-github-actions/security-guides/automatic-token-authentication). No repository/organisation secrets are needed for `release.yml`. (`docker.yml` likewise uses `GITHUB_TOKEN` for GHCR.)
- Side effect to know: releases created with `GITHUB_TOKEN` do not fire further workflows — *"With the exception of `workflow_dispatch` and `repository_dispatch`, other `GITHUB_TOKEN`-triggered events do not create workflow runs at all."* (events-that-trigger-workflows page). Nothing in this repo depends on a `release:` event, so no impact.

### Actions on the fork

- Forks of public repos get Actions but with the workflows initially disabled until the owner enables them in the Actions tab; scheduled workflows stay disabled (*"When a public repository is forked, scheduled workflows are disabled by default."* — https://docs.github.com/en/actions/managing-workflow-runs-and-deployments/managing-workflow-runs/disabling-and-enabling-a-workflow).
- Empirically this is already done on the fork: `GET /repos/SBKubric/3ax-ui-proxy/actions/permissions` → `enabled: true, allowed_actions: all`; `gh workflow list` shows `Release 3AX-UI` (id 295942922) **active**; `gh run list` shows recent successful `CI` runs on `main` and a successful `Release 3AX-UI` run (analyze-only) on Jun 14.
- Third-party actions in use (`actions/checkout@v6`, `actions/setup-go@v6`, `actions/upload-artifact@v6`, `msys2/setup-msys2@v2`, `svenstaro/upload-release-action@v2`) are allowed because `allowed_actions: all`.

### How the release object gets created

`release.yml` never calls "create release" explicitly. `svenstaro/upload-release-action` does it: it `GET`s the release by tag, and on 404 logs *"Release for tag ${tag} doesn't exist - creating it"* and calls `createRelease` with `tag_name`, `prerelease`, `draft`, `make_latest: 'true'` (src/main.ts). With eight matrix jobs racing, the action catches the 422 `already_exists` from the loser and re-fetches the release (*"…probably due to race condition between matrix jobs."*). `overwrite: true` (`release.yml:215`, `:350`) deletes a same-named asset before re-uploading, so re-running a failed matrix leg is safe. Once a release exists, the `prerelease` flag is **not** updated on later runs (only `promote: true` would flip it to false), so the flag is fixed by the first job that creates the release — which for a given tag is always the same expression, so no inconsistency.

## 3. Asset names: what `install.sh` asks for vs. what the workflow uploads

`install.sh:67-77` / `update.sh:88-98` map `uname -m` to exactly seven labels: `amd64`, `386`, `arm64`, `armv7`, `armv6`, `armv5`, `s390x`, and request

```
https://github.com/${XUI_REPO}/releases/download/${tag_version}/x-ui-linux-$(arch).tar.gz
```
(`install.sh:2464, 2481, 2499`; `update.sh:1521, 1524`).

`release.yml:57-65` builds the same seven platforms and uploads `asset_name: x-ui-linux-${{ matrix.platform }}.tar.gz` (`release.yml:205-216`), plus `x-ui-windows-amd64.zip` (`:343-351`), which no script downloads. Upstream's `v1.8.1` release carries exactly these eight assets (`gh api repos/coinman-dev/3ax-ui/releases/latest`).

Archive layout also matches: the workflow packs a top-level `x-ui/` directory containing the `x-ui` binary (`release.yml:121-126, 203`), and the scripts refuse a tarball that lacks `x-ui/x-ui` (`install.sh:2516-2522`; `update.sh:1535-1540`).

**Result: names match 1:1; nothing needs renaming.**

## Concrete checklist for "Cut a release tag on the fork"

1. Decide stable vs pre-release by the tag string alone:
   - `v1.8.1-proxy.1` (any `-`) → pre-release; users need `install.sh --beta` or `install.sh v1.8.1-proxy.1`; the default one-liner keeps failing with 404 on `releases/latest` until a non-hyphen tag exists.
   - `v1.8.2` (or e.g. `v1.8.1.1`) → full release → the default one-liner works.
2. Tag the desired commit on `main` and push: `git tag v1.8.2 && git push origin v1.8.2` (`v` prefix mandatory). Optionally bump `config/version` first; not required.
3. Wait for `Release 3AX-UI` (≈ 4-5 min per leg, 7 Linux + 1 Windows legs) and `Release 3X-UI for Docker`.
4. Verify: `gh api repos/SBKubric/3ax-ui-proxy/releases/latest -q '{tag_name,prerelease,assets:[.assets[].name]}'` shows the tag and eight assets; then `bash <(curl -Ls https://raw.githubusercontent.com/SBKubric/3ax-ui-proxy/main/install.sh)` resolves it.
5. Follow-up (separate ticket): `web/service/panel.go:137` still checks upstream `coinman-dev/3ax-ui` for updates.
