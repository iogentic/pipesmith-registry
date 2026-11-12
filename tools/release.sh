#!/usr/bin/env bash
# Writes the release files of the commit at HEAD into <out>/<version>/, with
# the names and content of docs/RELEASE.md in the ci repository:
#   pipesmith-registry-<os>-<arch>  the binaries of tools/go.sh build
#   sbom.spdx.json, sbom.cdx.json   Syft over the repository at the commit,
#                                   as the release job of the pipeline runs it
#   image.tar                       docker save of the image from Dockerfile
#   SHA256SUMS                      per file: SHA-256 in hex, two spaces, name
#   SHA256SUMS.sigstore.json        cosign bundle over SHA256SUMS, signed with
#                                   a key, without a transparency log entry
#   cosign.pub                      the public key of that key
# Local only: nothing is uploaded, and cosign and Syft run without network.
#
# usage: tools/release.sh [--out DIR] [--key-dir DIR | --no-sign]
#   --out DIR      receives <version>/; dist/ by default, else a folder below work/
#   --no-sign      no bundle and no cosign.pub: the release carries SHA256SUMS only
#   --key-dir DIR  a folder below work/ with cosign.key and cosign.pub; when both
#                  are missing a test key pair with an empty password is made
#                  there; work/release-key/ by default; never committed
#
# <version> is the content of VERSION, which the binaries carry. The tree must
# be clean, so binaries, bills of materials and image all come from HEAD. An
# existing <out>/<version>/ is refused: a release does not change. The files
# are written to a folder next to it, cosign.pub last, and the folder is
# renamed at the end. DOCKER_CONFIG must point at a configuration without
# credential prompts. The Go cache of tools/go.sh applies to the build.
# COSIGN_PASSWORD opens a key that has a password.
set -euo pipefail

COSIGN_IMAGE="ghcr.io/sigstore/cosign/cosign:v3.1.4@sha256:0f06154836d2de34f887a5c273506d52ca43edaa1d9e53e6510fa78a48f69c2d"
SYFT_IMAGE="ghcr.io/anchore/syft:v1.57.1@sha256:03215d0e98b661d0ecbc8575dbc14d65be1743b5de5df6410aecb7e80f95b060"
# The signing configuration of the kit (config/cosign/signing-config.json):
# no transparency log and no timestamp authority.
SIGNING_CONFIG='{"mediaType":"application/vnd.dev.sigstore.signingconfig.v0.2+json","rekorTlogConfig":{},"tsaConfig":{}}'
# The Syft configuration that the plan of the pipeline writes for a repository
# without exclude entries in .pipesmith-ci.toml.
SYFT_CONFIG='{
  "exclude": []
}'

ROOT="$(cd "$(dirname "$0")/.." && pwd -P)"

usage() { sed -n '2,27p' "$0" >&2; exit 2; }
fail() { echo "tools/release.sh: $*" >&2; exit 1; }
realpath_of() { python3 -c 'import os, sys; print(os.path.realpath(sys.argv[1]))' "$1"; }

OUT="$ROOT/dist"
KEY_DIR="$ROOT/work/release-key"
SIGN=1
while [ "$#" -gt 0 ]; do
  case "$1" in
    --no-sign) SIGN=0; shift ;;
    --out) [ "$#" -ge 2 ] || usage; OUT="$2"; shift 2 ;;
    --key-dir) [ "$#" -ge 2 ] || usage; KEY_DIR="$2"; shift 2 ;;
    *) usage ;;
  esac
done

[ -n "${DOCKER_CONFIG:-}" ] || fail "set DOCKER_CONFIG to a configuration without credential prompts"
VERSION="$(cat "$ROOT/VERSION")"
[[ "$VERSION" =~ ^[0-9A-Za-z][0-9A-Za-z._+-]*$ ]] || fail "VERSION is not a plain version: $VERSION"

OUT="$(realpath_of "$OUT")"
KEY_DIR="$(realpath_of "$KEY_DIR")"
case "$OUT/" in
  "$ROOT/dist/" | "$ROOT/work/"?*) ;;
  *) fail "--out must be dist/ or a folder below work/: $OUT" ;;
esac
case "$KEY_DIR/" in
  "$ROOT/work/"?*) ;;
  *) fail "--key-dir must be a folder below work/: $KEY_DIR" ;;
esac

[ -z "$(git -C "$ROOT" status --porcelain)" ] || fail "the tree is not clean; commit first"
HEAD="$(git -C "$ROOT" rev-parse HEAD)"
DEST="$OUT/$VERSION"
[ ! -e "$DEST" ] || fail "$DEST exists; a release does not change"

# The platforms of tools/go.sh build, so a new platform needs no change here.
TARGETS="$(sed -n 's/^ *for target in \(.*\); do$/\1/p' "$ROOT/tools/go.sh")"
[ -n "$TARGETS" ] || fail "found no build targets in tools/go.sh"
BINARIES=()
for target in $TARGETS; do BINARIES+=("pipesmith-registry-${target%/*}-${target#*/}"); done

if [ "$SIGN" -eq 0 ]; then
  echo "== no signature (--no-sign)"
elif [ ! -e "$KEY_DIR/cosign.key" ] && [ ! -e "$KEY_DIR/cosign.pub" ]; then
  echo "== key in ${KEY_DIR#"$ROOT/"}"
  echo "making a test key pair"
  mkdir -p "$KEY_DIR"
  chmod 0700 "$KEY_DIR"
  COSIGN_PASSWORD="" docker run --rm --network none -e COSIGN_PASSWORD -v "$KEY_DIR:/k" \
    "$COSIGN_IMAGE" generate-key-pair --output-key-prefix /k/cosign
elif [ ! -f "$KEY_DIR/cosign.key" ] || [ ! -f "$KEY_DIR/cosign.pub" ]; then
  fail "$KEY_DIR holds only one of cosign.key and cosign.pub"
else
  echo "== key in ${KEY_DIR#"$ROOT/"}"
fi

STAGE="$OUT/.$VERSION.partial.$$"
TMP="$ROOT/work/.release-tmp.$$"
TAG="pipesmith-registry-release:$$-$(date +%s)"
cleanup() {
  rm -rf "$TMP" "$STAGE"
  docker image rm "$TAG" > /dev/null 2>&1 || true
}
trap cleanup EXIT
mkdir -p "$OUT" "$STAGE" "$TMP/src" "$TMP/out"

echo "== binaries of $HEAD, version $VERSION"
# The binaries carry VERSION, as docs/RELEASE.md requires.
env -u PSR_VERSION "$ROOT/tools/go.sh" build
for name in "${BINARIES[@]}"; do
  cp "$ROOT/bin/$name" "$STAGE/$name"
  chmod 0755 "$STAGE/$name"
done
rel="${STAGE#"$ROOT/"}"
info="$("$ROOT/tools/go.sh" run "for f in ${BINARIES[*]}; do go version -m \"$rel/\$f\"; done")"
[ "$(grep -c "vcs.revision=$HEAD\$" <<< "$info")" -eq "${#BINARIES[@]}" ] || fail "a binary does not carry the commit $HEAD"
[ "$(grep -c "vcs.modified=false\$" <<< "$info")" -eq "${#BINARIES[@]}" ] || fail "a binary was built from a changed tree"

echo "== bills of materials of the repository at $HEAD"
git -C "$ROOT" archive --format=tar HEAD | tar -x -C "$TMP/src"
printf '%s\n' "$SYFT_CONFIG" > "$TMP/syft.yaml"
docker run --rm --network none -e SYFT_CHECK_FOR_APP_UPDATE=false \
  -v "$TMP:/w" -w /w/src "$SYFT_IMAGE" \
  dir:. --config /w/syft.yaml -o spdx-json=/w/out/sbom.spdx.json -o cyclonedx-json=/w/out/sbom.cdx.json
cp "$TMP/out/sbom.spdx.json" "$TMP/out/sbom.cdx.json" "$STAGE/"

echo "== image from Dockerfile"
docker build --file "$TMP/src/Dockerfile" --tag "$TAG" \
  --label "org.opencontainers.image.source=https://github.com/mirosso25/pipesmith-registry" \
  --label "org.opencontainers.image.version=$VERSION" --label "org.opencontainers.image.licenses=Apache-2.0" "$TMP/src"
docker save --output "$STAGE/image.tar" "$TAG"
docker image rm "$TAG" > /dev/null

echo "== SHA256SUMS"
if command -v sha256sum > /dev/null; then SUM=(sha256sum); else SUM=(shasum -a 256); fi
(cd "$STAGE" && "${SUM[@]}" sbom.spdx.json sbom.cdx.json image.tar "${BINARIES[@]}" > SHA256SUMS)
cat "$STAGE/SHA256SUMS"

chmod 0644 "$STAGE"/sbom.spdx.json "$STAGE"/sbom.cdx.json "$STAGE"/image.tar "$STAGE"/SHA256SUMS
if [ "$SIGN" -eq 1 ]; then
echo "== signature"
printf '%s\n' "$SIGNING_CONFIG" > "$TMP/signing-config.json"
COSIGN_PASSWORD="${COSIGN_PASSWORD:-}" docker run --rm --network none -e COSIGN_PASSWORD \
  -v "$KEY_DIR:/k:ro" -v "$TMP:/c:ro" -v "$STAGE:/r" "$COSIGN_IMAGE" \
  sign-blob --yes --key /k/cosign.key --signing-config /c/signing-config.json \
  --bundle /r/SHA256SUMS.sigstore.json /r/SHA256SUMS
docker run --rm --network none -v "$KEY_DIR:/k:ro" -v "$STAGE:/r:ro" "$COSIGN_IMAGE" \
  verify-blob --key /k/cosign.pub --insecure-ignore-tlog \
  --bundle /r/SHA256SUMS.sigstore.json /r/SHA256SUMS
cp "$KEY_DIR/cosign.pub" "$STAGE/cosign.pub"
chmod 0644 "$STAGE"/SHA256SUMS.sigstore.json "$STAGE"/cosign.pub
fi

mv "$STAGE" "$DEST"
echo "== release ${DEST#"$ROOT/"}"
ls -l "$DEST"
