# Ly Route Orchestrator — build repository

This repository holds the **Orchestrator** release tree of the Ly Route codebase.
The Gateway and Orchestrator products share the Go backend and the React UI
source; only the product profile, the packaged UI bundle and the runtime
packages differ.

## Branches

| branch | purpose |
|---|---|
| `main` | Orchestrator release candidate (what CI builds) |
| `ui-source` | React/TypeScript UI source used to produce the pinned bundle |

`packaging/build-profiles/orchestrator.json` pins the shipped UI:

```json
{
  "frontend_app_sha256": "2eb71550…",
  "frontend_bundle_source": "2b1f651…"
}
```

`frontend_bundle_source` is a commit in this repository (the `ui-source`
branch). The CI `ui` job checks that commit out into a worktree, builds the
bundle with `LY_ROUTE_PRODUCT=orchestrator`, and fails if the produced
`app.js` differs from `frontend_app_sha256` or from the bundle committed under
`packaging/orchestrator-frontend/bundle/`.

## CI

`.github/workflows/orchestrator-build.yml`

| job | what it does |
|---|---|
| `verify` | line-ending normalisation, toolchain check, `scripts/ci-release-verify.sh` |
| `backend` | `go build`, `go vet`, `gofmt`, full `go test`, builds and uploads `orchestrator-control` |
| `ui` | reproducible orchestrator bundle from the pinned UI commit, hash check |
| `image` | pinned VPP packages (cached) + `ly-route-vpp-apply`/`ly-route-vpp-orchestrator` + rootfs + upgrade + 4G disk image, hash-verified, uploaded |

The `image` job runs on pushes to `main`, tags and manual dispatch (not on pull
requests). It needs no secrets: VPP is built from the pinned upstream commit and
cached; the Debian rootfs uses the configured mirrors.

### Running the same checks locally

```sh
bash scripts/ci-release-verify.sh

cd backend
go build ./... && go vet ./... && go test -p 1 -count=1 ./...

CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o /tmp/orchestrator-control ./cmd/orchestrator-control
/tmp/orchestrator-control --version
```

### Building the release image locally

```sh
export LY_ROUTE_EXTRA_DEBS_DIR=/path/to/runtime-debs   # vpp base + vpp-apply + vpp-orchestrator
bash scripts/build-rootfs.sh --product orchestrator --arch amd64 --out dist/rootfs
bash scripts/build-upgrade-package.sh --product orchestrator --arch amd64 --out dist/upgrade
bash scripts/build-disk-image.sh --product orchestrator \
  --rootfs dist/rootfs/ly-route-rootfs-orchestrator-bookworm-amd64.tar.zst \
  --out dist/disk --size 4G
```

The disk manifest records `product=orchestrator`, `source_commit` and
`frontend_app_sha256`; acceptance pins both separately.

## Note on `.github/workflows/gateway-release.yml`

The shared rootfs scaffold validation asserts the gateway release topology is
present. This repository therefore keeps that workflow as **manual dispatch
only** (reference for the gateway product); it never runs on push or tag here.
Gateway images are built in the `ly-route` repository.
