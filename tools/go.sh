#!/usr/bin/env bash
# Runs the Go toolchain inside the pinned golang image. Nothing is installed on
# the host. Module and build caches live in .cache/.
#
# usage: tools/go.sh <command>
#   check     gofmt, go vet and go test -race over every package
#   build     the binary for darwin/arm64, linux/amd64 and linux/arm64 into bin/
#   vuln      govulncheck over every package
#   sec       gosec as the sast stage of Pipesmith CI runs it, then the gate
#             at level error (tools/sarif_gate.py); SARIF in work/gosec/
#   all       check, build, vuln and sec, in this order (the gate of every build)
#   tidy      go mod tidy and go mod verify
#   run ...   any command in the container, from the repository root
#
# DOCKER_CONFIG must point at a Docker configuration without credential prompts.
# The module and build caches are bind mounts in .cache/ of the repository (git
# ignores it), so they stay on the disk of the repository and not in the Docker
# VM. PSR_GO_CACHE names another cache directory. PSR_GO_NETWORK puts the
# container on a Docker network, for tests against a server in another
# container. In a git worktree the common git directory is mounted read only at
# its own path, so the build can still read the commit.
set -euo pipefail

IMAGE="golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9"
GOVULNCHECK="golang.org/x/vuln/cmd/govulncheck@v1.9.0"
# The gosec image and arguments of kit/scripts/gosec.sh in Pipesmith CI.
GOSEC_IMAGE="ghcr.io/securego/gosec:2.30.0@sha256:7bcf1edbab3e80d8006a28d53e7031a9c9775be8eec30679e4adc1f51d272121"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CACHE="${PSR_GO_CACHE:-$ROOT/.cache}"
mkdir -p "$CACHE/go-mod" "$CACHE/go-build"
VERSION="${PSR_VERSION:-$(cat "$ROOT/VERSION")}"

if [ -z "${DOCKER_CONFIG:-}" ]; then
  echo "tools/go.sh: set DOCKER_CONFIG to a configuration without credential prompts" >&2
  exit 2
fi

GIT_MOUNT=()
if [ -f "$ROOT/.git" ]; then
  COMMON="$(git -C "$ROOT" rev-parse --path-format=absolute --git-common-dir)"
  GIT_MOUNT=(-v "$COMMON:$COMMON:ro")
fi
NETWORK=()
if [ -n "${PSR_GO_NETWORK:-}" ]; then NETWORK=(--network "$PSR_GO_NETWORK"); fi

in_go() {
  docker run --rm \
    ${GIT_MOUNT[@]+"${GIT_MOUNT[@]}"} ${NETWORK[@]+"${NETWORK[@]}"} \
    -v "$ROOT:/src" \
    -v "$CACHE/go-mod:/go/pkg/mod" \
    -v "$CACHE/go-build:/root/.cache/go-build" \
    -e PSR_VERSION="$VERSION" \
    -w /src \
    "$IMAGE" sh -euc "$*"
}

check() {
  in_go '
    unformatted="$(gofmt -l cmd internal tools)"
    if [ -n "$unformatted" ]; then echo "gofmt: these files need formatting:"; echo "$unformatted"; exit 1; fi
    go vet ./...
    go test -race -count=1 ./...
  '
}

build() {
  in_go '
    for target in darwin/arm64 linux/amd64 linux/arm64; do
      os="${target%/*}"; arch="${target#*/}"
      CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
        -ldflags "-s -w -X github.com/mirosso25/pipesmith-registry/internal/version.Version=$PSR_VERSION" \
        -o "bin/pipesmith-registry-$os-$arch" ./cmd/pipesmith-registry
    done
    ls -l bin
  '
}

vuln() {
  in_go "go run $GOVULNCHECK ./..."
}

sec() {
  local out="$ROOT/work/gosec"
  mkdir -p "$out"
  # The packages of this module as go list names them; the gate fails on a
  # result outside them and when gosec checked fewer files than packages.
  in_go 'go list -f "{{.Dir}}" ./...' | sed -e 's#^/src/##' -e 's#^/src$#.#' > "$out/packages.txt"
  # Every top level folder git ignores (.cache/ with the module cache by
  # default, work/ with worktrees and scratch data, .tmp/, .venv/, bin/) holds no
  # product code. An empty tmpfs covers it, so gosec neither scans nor walks it;
  # walking a large cache took longer than 20 minutes.
  local masks=() dir
  while IFS= read -r dir; do
    case "$dir" in */) dir="${dir%/}" ;; *) continue ;; esac
    case "$dir" in */*) continue ;; esac
    masks+=(--mount "type=tmpfs,destination=/src/$dir")
  done < <(git -C "$ROOT" ls-files --others --ignored --exclude-standard --directory)
  # The image pins GOTOOLCHAIN=local with an older Go than go.mod asks for;
  # gosec then loads no package, so the go command may fetch the toolchain.
  # The text report on stdout carries the file count the gate checks.
  docker run --rm \
    -v "$ROOT:/src" \
    ${masks[@]+"${masks[@]}"} \
    -v "$out:/out" \
    -v "$CACHE/go-mod:/go/pkg/mod" \
    -e GOTOOLCHAIN=auto \
    -w /src \
    "$GOSEC_IMAGE" -no-fail -fmt sarif -out /out/gosec.sarif -stdout -verbose=text ./... > "$out/gosec.txt" 2>&1
  python3 "$ROOT/tools/sarif_gate.py" "$out/gosec.sarif" --packages "$out/packages.txt" --summary "$out/gosec.txt"
}

case "${1:-}" in
  check) check ;;
  build) build ;;
  vuln) vuln ;;
  sec) sec ;;
  all) check && build && vuln && sec ;;
  tidy) in_go 'go mod tidy && go mod verify' ;;
  run) shift; in_go "$*" ;;
  *) sed -n '2,18p' "$0" >&2; exit 2 ;;
esac
