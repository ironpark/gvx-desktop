# Development Guide

This document covers building GVX Desktop from source, running tests and quality checks, regenerating visual and localization assets, understanding the codebase architecture, and cutting reproducible releases.

For installation and end-user documentation, see [README.md](README.md) ([한국어](README.ko.md)).

## Prerequisites

- **Go 1.27+** (release builds pin `go1.27.4` in [release/toolchain.env](release/toolchain.env))
- **`ggui` CLI `v0.9.2`** (for bundling, `ggui dev`, `ggui i18n check`, and `ggui record`). Install the pinned version with:

  ```sh
  ./release/repro-build.sh install-ggui
  ```

- **`rungv`** (optional for unit tests and `-render-dir` previews; required for running sandboxes and live integration tests)

## Build and run

Run these commands from the repository root:

```sh
go build -o bin/gvx-desktop .
./bin/gvx-desktop
```

### Live development with `ggui dev`

Use `ggui dev` for automatic rebuilds and the dev-only tags configured in [ggui.toml](ggui.toml):

```sh
ggui dev
```

To develop against a local `ggui` checkout, create a Git-ignored `go.work` file (or run `go work use . ../ggui ../ggui/icons/lucide` if you already have one):

```sh
go work init . ../ggui ../ggui/icons/lucide
```

### Bundling & platform installers

GVX Desktop configures the Zig-backed native GPU renderer (`renderer = "zig"`, powered by `ggfx`) in [ggui.toml](ggui.toml).

- **macOS `.app` bundle and DMG** (output in `build/`):

  ```sh
  ggui build --dmg
  ```

- **Windows executable and Setup wizard** (`cmd/installer` is auto-discovered by `ggui build --target windows --installer` and embeds `GVX Desktop.exe`, `ggfx_driver.dll`, and any `--installer-extra-dir` files such as the bundled Docker CLI):

  ```sh
  ggui build --target windows --installer
  ```

- **Full reproducible release packages** (including bundled Docker CLI binaries fetched and verified via [release/fetch-docker-cli.sh](release/fetch-docker-cli.sh)):

  ```sh
  ./release/repro-build.sh package macos arm64 0.9.0 ./out
  ./release/repro-build.sh package windows amd64 0.9.0 ./out
  ./release/repro-build.sh package linux amd64 0.9.0 ./out
  ```

## Testing and code quality

### Formatting and cross-OS static checks

```sh
gofmt -l .
GOOS=darwin  go vet ./...
GOOS=linux   go vet ./...
GOOS=windows go vet ./...
```

### Unit, race, and UI goroutine-safety tests

```sh
# Fast unit tests (no rungv required)
go test -short ./...

# Race detector
go test -race -count=1 ./...

# UI goroutine-safety check (panics on off-UI-goroutine state writes)
go test -tags ggui_debug -short ./...
```

### Localization check

Verify that every translated string in `internal/ui` and `main.go` exists in all language catalogs:

```sh
ggui i18n check
```

### Live integration tests

With a working `rungv` binary on `PATH` (or `$GVX_RUNGV`), run the sandbox and Docker Engine integration tests:

```sh
go test ./internal/sandbox/
GVXD_DOCKER_TEST=1 go test ./internal/recipes/
```

## Preview screenshots, demo GIF, icons, and localization

### Preview screenshots

Render application screens to PNG using demo data with no running engine:

```sh
./bin/gvx-desktop -render-dir ./shots
./bin/gvx-desktop -render-dir ./shots-ko -render-lang ko
```

### Demo GIF

The walkthrough GIF in the README is scripted in [tour.go](tour.go). Regenerate it from the repository root using [`ggui record`](https://github.com/ironpark/ggui/blob/main/docs/recording.md) (`ggui` CLI v0.6.0+):

```sh
ggui record -o assets/tour.gif --width 1040 --frame mac --lossy 30 --fps 15
```

### App icon assets

To regenerate the PNG icon assets (`assets/icon.png` and sidebar mark) from `assets/gvx-desktop-icon.svg` on macOS:

```sh
swift assets/svg2png.swift
```

### Localization

Each supported language has a catalog in [internal/ui](internal/ui) (`i18n_ko.go`, `i18n_ja.go`, `i18n_zh_hans.go`, `i18n_zh_hant.go`, `i18n_de.go`, `i18n_hi.go`) keyed by the English source strings. Functions tracked by `ggui i18n check` are configured under `[i18n]` in [ggui.toml](ggui.toml) (`tr`, `ui.T`, `trn`). Languages other than Korean were machine-translated and have not been reviewed by native speakers; improvements are welcome.

## Architecture

Because the gvx Go SDK constrains the calling process, GVX Desktop runs one `rungv` child process per sandbox and communicates with management commands using `--format json` and per-sandbox control sockets. When the app quits (or receives `SIGINT` / `SIGTERM`), it stops the sandboxes it launched.

### Packages

| Package | Responsibility |
| --- | --- |
| [internal/rungv](internal/rungv) | Locate, install, update, codesign (macOS hypervisor entitlement), and invoke the `rungv` CLI and v0.20/v0.21 control-socket commands. |
| [internal/sandbox](internal/sandbox) | Translate sandbox specs into `rungv` CLI flags and supervise child processes, logs, live port/mount/QoS updates, readiness, and snapshots. |
| [internal/engine](internal/engine) | Native **Sandbox Engine** Docker API server: serves Docker CLI and Compose requests directly by running each container as an unprivileged gvx sandbox with BuildKit, Healthcheck, volumes, and networks. |
| [internal/overlay](internal/overlay) | Userspace overlay networking, DNS resolution, and `netinit` plumbing for inter-container communication across sandboxes. |
| [internal/nodenet](internal/nodenet) | Multi-node Kubernetes networking and image import (`ctr -n k8s.io images import`) joining k3s server and agent sandboxes. |
| [internal/kubeapi](internal/kubeapi) | Lightweight Kubernetes API client (over plain `net/http` using kubeconfig credentials) for pods, metrics, workloads, services, logs, and port forwarding. |
| [internal/dockerapi](internal/dockerapi) | Docker Engine HTTP API client used by the UI and compatibility `docker:dind` mode. |
| [internal/dockercli](internal/dockercli) | Locate and expose bundled `docker`, `docker-buildx`, and `docker-compose` binaries and manage `~/.gvx/bin` terminal `PATH` registration. |
| [internal/dockerproxy](internal/dockerproxy) | Host-facing Docker Engine API proxy forwarding requests to a `docker:dind` sandbox Unix socket and translating shared host bind mounts. |
| [internal/localreg](internal/localreg) | Embedded loopback-only OCI Distribution v1.1 / Registry V2 server (`127.0.0.1:5001`) with disk persistence, upload staging, and garbage collection. |
| [internal/localproxy](internal/localproxy) | Embedded loopback HTTP/HTTPS reverse proxy (`:18080` / `:18443`) for `*.localhost` domains with persistent local dev CA and dynamic TLS leaf certificates. |
| [internal/mcpserver](internal/mcpserver) | Embedded loopback Streamable HTTP MCP server (`http://127.0.0.1:18787/mcp`, powered by `modelcontextprotocol/go-sdk`) exposing 14 tools for AI coding agents. |
| [internal/registry](internal/registry) | Search Docker Hub and Quay and list remote image tags. |
| [internal/imagefs](internal/imagefs) | Browse files and layers inside cached container images. |
| [internal/ext4lite](internal/ext4lite) | Pure-Go read-only reader for `rungv`'s `ext4lite` `.ext4` volume files (including in-memory journal replay). |
| [internal/appupdate](internal/appupdate) | Self-updater for GVX Desktop supporting Ed25519-signed Sparkle 2 `appcast.xml` feeds with GitHub Releases fallback and install-method detection. |
| [internal/ghrelease](internal/ghrelease) | GitHub Releases API client and SHA-256-verified artifact downloader shared by `rungv` and app updaters. |
| [internal/procstat](internal/procstat) | Cross-platform process CPU, memory, and I/O statistics collection for running sandboxes. |
| [internal/term](internal/term) | Interactive PTY/terminal session bridge for container and pod shells. |
| [internal/recipes](internal/recipes) | Built-in Docker Engine (`Sandbox Engine` and `docker:dind`), Kubernetes (`k3s` server/agent), and helper presets. |
| [internal/store](internal/store) | Persist `sandboxes.json` and `settings.json` in the user configuration directory. |
| [internal/ui](internal/ui) | All `ggui` application screens, dialogs, sidebar navigation, tray integration, Command Palette, and i18n catalogs. |
| [cmd/installer](cmd/installer) | Self-contained Windows GUI setup and uninstaller wizard (`gvx-desktop-<version>-windows-<arch>-setup.exe`) built with `ggui/installer`. |
| [release/](release/) | Deterministic build driver (`repro-build.sh`), toolchain & Docker CLI pins (`toolchain.env`, `docker-cli.env`), bundle/archive tooling (`bundletool/`, `repro-pack.go`, `fetch-docker-cli.sh`, `check-bundle.sh`), Homebrew cask (`cask.sh`), and Chocolatey (`choco.sh`). |

### Design documents

Detailed design notes live under [docs/design/](docs/design/):

- [docs/design/sandbox-engine.md](docs/design/sandbox-engine.md) — Native Docker Sandbox Engine architecture
- [docs/design/amd64-builder.md](docs/design/amd64-builder.md) — Rosetta-backed `linux/amd64` builder VM for Apple silicon macOS
- [docs/design/gvx-v020.md](docs/design/gvx-v020.md) — `rungv` v0.20+ control socket, live updates, and runtime integration
- [docs/design/k3s-clusters.md](docs/design/k3s-clusters.md) — Multi-node k3s cluster topology and image loading

## Release and reproducible builds

### Cutting a release

1. Update `version` under `[app]` in [ggui.toml](ggui.toml) (and [CHANGELOG.md](CHANGELOG.md)).
2. Commit the release preparation changes, tag the commit `vX.Y.Z`, and push the tag.

The [release workflow](.github/workflows/release.yml) runs entirely on GitHub-hosted Linux runners (except optional Chocolatey verification on `windows-2025`):

- **Pinned toolchains**: Go and `ggui` CLI versions and per-OS `GOAMD64`/`GOARM64` microarchitecture levels are pinned in [release/toolchain.env](release/toolchain.env). Official Docker CLI, Buildx, and Compose binaries and their SHA-256 digests are pinned in [release/docker-cli.env](release/docker-cli.env) and fetched by [release/fetch-docker-cli.sh](release/fetch-docker-cli.sh).
- **Artifacts produced**:
  - macOS Apple silicon: `gvx-desktop-<version>-macos-arm64.dmg` (bundling `Contents/Helpers/docker` and CLI plugins)
  - Windows (`amd64` and `arm64`): `gvx-desktop-<version>-windows-<arch>-setup.exe` and `gvx-desktop-<version>-windows-<arch>.zip` (bundling `ggfx_driver.dll` and `bin/docker.exe` + CLI plugins)
  - Linux (`amd64` and `arm64`): `gvx-desktop-<version>-linux-<arch>.tar.gz`, `gvx-desktop_<version>_<arch>.deb`, and `gvx-desktop-<version>-1.<x86_64|aarch64>.rpm` (bundling `libggfx_driver.so` and Docker CLI under `/usr/lib/gvx-desktop/`)
  - Metadata & trust: `SHA256SUMS`, per-file `.sha256`, `BUILD_INFO.txt`, Ed25519-signed Sparkle 2 `appcast.xml` (when `GGUI_UPDATE_PRIVATE_KEY` is configured), and the offline SLSA v1.0 provenance bundle `gvx-desktop-v<version>.sigstore.json`.
- **Reproducibility checks**: CI rebuilds Windows (`amd64` portable ZIP and `-setup.exe`) and Linux (`amd64` `.tar.gz` and `.deb`) in clean build caches and asserts bit-for-bit SHA-256 equality before signing attestations or publishing.
- **Downstream package managers**:
  - Updates `ironpark/homebrew-tap` via [release/cask.sh](release/cask.sh) using `TAP_GITHUB_TOKEN`.
  - Optionally packages and publishes to Chocolatey via [release/choco.sh](release/choco.sh) when the repository variable `CHOCO_PUBLISH` is set to `true` (see the workflow header for optional signing, notarization, Sparkle `GGUI_UPDATE_PRIVATE_KEY`, and `CHOCO_API_KEY` secrets).

### Reproducing a release locally

Check out tag `vX.Y.Z` and run [release/repro-build.sh](release/repro-build.sh) using the `SOURCE_DATE_EPOCH` recorded in the release's `BUILD_INFO.txt`:

```sh
V=0.9.0  # release version, in a checkout of tag v0.9.0
./release/repro-build.sh install-ggui
./release/repro-build.sh package windows amd64 "$V" ./out
./release/repro-build.sh package linux amd64 "$V" ./out
```

Or install a reproducible standalone binary directly from `proxy.golang.org` (example for Linux `amd64`; check `BUILD_INFO.txt` for the exact values of a specific release):

```sh
V=0.9.0
GO111MODULE=on GOPROXY=https://proxy.golang.org/ GOSUMDB=sum.golang.org \
  GOENV=off GOWORK=off GOTELEMETRY=off GOFIPS140=off GOEXPERIMENT= GODEBUG= GOFLAGS= GOBIN= \
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v2 GOTOOLCHAIN=go1.27.4 \
  go install -trimpath -pgo=off -compiler=gc -tags=ggfxzig \
  -ldflags="-s -w -buildid= -X main.version=$V -X github.com/ironpark/ggui.buildID=io.github.ironpark.gvx-desktop" \
  -- "github.com/ironpark/gvx-desktop@v$V"
```

### Verifying SLSA build provenance

Releases after `v0.7.0` include SLSA v1.0 build provenance signed via Sigstore (`actions/attest`):

```sh
V=0.9.0
gh attestation verify "gvx-desktop-$V-windows-amd64-setup.exe" \
  --repo ironpark/gvx-desktop \
  --signer-workflow ironpark/gvx-desktop/.github/workflows/release.yml \
  --source-ref "refs/tags/v$V" \
  --deny-self-hosted-runners
```
