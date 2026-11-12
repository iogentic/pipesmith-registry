#!/usr/bin/env bash
# Runs the export program tools/h2export/H2Export.java in the pinned Java
# image with the H2 driver 2.2.224. Nothing is installed on the host.
#
# usage: tools/h2export.sh <command>
#   export --database <file> --out <dir> [--set content|identity|all] [--with-password-hashes]
#             export one database below work/ or .tmp/ of this repository
#   export --database-copy <file> --out <dir> [...]
#             the same for a database anywhere else, named on purpose
#   selftest  check the driver and the image, print the Java and driver versions
#   fixture <file> [--missing-column TABLE.COLUMN]
#             build the fixture database of tests/import (FixtureDb.java)
#
# <file> is the .mv.db file of the database. --out must not exist or be
# empty, and lies below work/ or .tmp/ of this repository, or below
# testdata/export/ for a database that is not a --database-copy. The export
# directory gets mode 0700 and its files 0600.
#
# The driver is fetched once from Maven Central into $PSR_GO_CACHE/h2/
# (default .cache/h2/ of this repository), checked against the SHA-256 below
# and against the .sha1 Maven Central publishes, and checked again at every
# run. A jar that fails the check is never replaced by this script.
# Licence of the driver: MPL 2.0 or EPL 1.0 (its pom on Maven Central).
#
# DOCKER_CONFIG must point at a Docker configuration without credential
# prompts. The image is used as it is on this machine; it is never pulled.
# Exit codes: 0 done, 1 failed, 2 usage, 3 refused path, and the codes of
# the program (4 schema mismatch, 5 database cannot be opened).
set -euo pipefail

IMAGE="maven:3.9-eclipse-temurin-21@sha256:99e61abcff91a9b1333463bd8451fb18495d6eba9250ac66a338b518f8278320"
H2_VERSION="2.2.224"
H2_SHA256="b9d8f19358ada82a4f6eb5b174c6cfe320a375b5a9cb5a4fe456d623e6e55497"
H2_SHA1="7bdade27d8cd197d9b5ce9dc251f41d2edc5f7ad"
H2_URL="https://repo1.maven.org/maven2/com/h2database/h2/$H2_VERSION/h2-$H2_VERSION.jar"

ROOT="$(cd "$(dirname "$0")/.." && pwd -P)"
CACHE="${PSR_GO_CACHE:-$ROOT/.cache}"
JAR_DIR="$CACHE/h2"
JAR="$JAR_DIR/h2-$H2_VERSION.jar"

usage() { sed -n '2,13p' "$0" >&2; exit 2; }
fail() { echo "tools/h2export.sh: $*" >&2; exit 1; }
refuse() { echo "tools/h2export.sh: $*" >&2; exit 3; }

if [ -z "${DOCKER_CONFIG:-}" ]; then
  echo "tools/h2export.sh: set DOCKER_CONFIG to a configuration without credential prompts" >&2
  exit 2
fi

# resolved prints the absolute path with every symbolic link resolved, also
# for a path that does not exist yet.
resolved() { python3 -I -c 'import os, sys; print(os.path.realpath(sys.argv[1]))' "$1"; }

below() { # below <path> <dir>...: the resolved path lies inside one of the dirs
  local path="$1" dir
  shift
  for dir in "$@"; do
    case "$path" in "$dir"/*) return 0 ;; esac
  done
  return 1
}

sha256_of() { shasum -a 256 "$1" | cut -d ' ' -f 1; }
sha1_of() { shasum -a 1 "$1" | cut -d ' ' -f 1; }

driver() {
  if [ ! -f "$JAR" ]; then
    mkdir -p "$JAR_DIR"
    local temp="$JAR_DIR/.h2-$$.jar" published
    curl -fsS -o "$temp" "$H2_URL" || { rm -f "$temp"; fail "cannot fetch $H2_URL"; }
    published="$(curl -fsS "$H2_URL.sha1" | tr -d '[:space:]')" || { rm -f "$temp"; fail "cannot fetch $H2_URL.sha1"; }
    if [ "$(sha256_of "$temp")" != "$H2_SHA256" ]; then rm -f "$temp"; fail "the fetched driver does not have the pinned SHA-256"; fi
    if [ "$(sha1_of "$temp")" != "$published" ] || [ "$published" != "$H2_SHA1" ]; then
      rm -f "$temp"; fail "the fetched driver does not match the SHA-1 Maven Central publishes"
    fi
    mv "$temp" "$JAR"
  fi
  if [ "$(sha256_of "$JAR")" != "$H2_SHA256" ]; then
    fail "$JAR does not have the pinned SHA-256 $H2_SHA256; remove it to fetch it again"
  fi
}

image() {
  docker image inspect "$IMAGE" >/dev/null 2>&1 ||
    fail "the image $IMAGE is not on this machine; check df -h first (8 GiB free at least), then: docker pull $IMAGE"
}

# java <mounts...> -- <java arguments...>: the JVM in the image, no network,
# as the calling user, with the driver and this repository's tools read only.
java_run() {
  local mounts=()
  while [ "$1" != "--" ]; do mounts+=("$1"); shift; done
  shift
  docker run --rm --network none --user "$(id -u):$(id -g)" --entrypoint java \
    -v "$JAR_DIR:/h2:ro" -v "$ROOT/tools/h2export:/tool:ro" \
    ${mounts[@]+"${mounts[@]}"} \
    "$IMAGE" -XX:-UsePerfData -Xmx512m -cp "/h2/h2-$H2_VERSION.jar" "$@"
}

# database_file <arg>: the .mv.db file of a database argument.
database_file() {
  case "$1" in
    *.mv.db) printf '%s\n' "$1" ;;
    *) usage ;;
  esac
}

cmd_export() {
  local database="" copy="" out="" set="content" hashes=()
  while [ $# -gt 0 ]; do
    case "$1" in
      --database) [ $# -ge 2 ] || usage; database="$2"; shift 2 ;;
      --database-copy) [ $# -ge 2 ] || usage; copy="$2"; shift 2 ;;
      --out) [ $# -ge 2 ] || usage; out="$2"; shift 2 ;;
      --set) [ $# -ge 2 ] || usage; set="$2"; shift 2 ;;
      --with-password-hashes) hashes=(--with-password-hashes); shift ;;
      *) usage ;;
    esac
  done
  [ -n "$out" ] || usage
  if [ -n "$database" ] && [ -n "$copy" ]; then usage; fi
  [ -n "$database$copy" ] || usage
  case "$set" in content|identity|all) ;; *) usage ;; esac

  local file
  file="$(resolved "$(database_file "${database:-$copy}")")"
  if [ -n "$database" ] && ! below "$file" "$ROOT/work" "$ROOT/.tmp"; then
    refuse "the database lies outside work/ and .tmp/ of this repository; name a copy on purpose with --database-copy"
  fi
  local target
  target="$(resolved "$out")"
  if [ -n "$copy" ]; then
    below "$target" "$ROOT/work" "$ROOT/.tmp" ||
      refuse "the export of a --database-copy goes below work/ or .tmp/ of this repository only"
  else
    below "$target" "$ROOT/work" "$ROOT/.tmp" "$ROOT/testdata/export" ||
      refuse "--out lies outside work/, .tmp/ and testdata/export/ of this repository"
  fi
  if [ -e "$target" ]; then
    [ -d "$target" ] || refuse "--out exists and is not a directory"
    [ -z "$(ls -A "$target")" ] || refuse "--out is not empty"
  fi
  [ -f "$file" ] || { echo "tools/h2export.sh: the database file does not exist" >&2; exit 5; }

  driver
  image
  local created=0
  if [ ! -e "$target" ]; then mkdir -p "$(dirname "$target")"; mkdir -m 0700 "$target"; created=1; fi
  chmod 0700 "$target"
  local dir base code=0
  dir="$(dirname "$file")"
  base="$(basename "$file" .mv.db)"
  java_run -v "$dir:/db:ro" -v "$target:/out" -- \
    /tool/H2Export.java --database "/db/$base" --out /out --set "$set" ${hashes[@]+"${hashes[@]}"} || code=$?
  if [ "$code" -ne 0 ] && [ "$created" -eq 1 ] && [ -z "$(ls -A "$target")" ]; then rmdir "$target"; fi
  return "$code"
}

cmd_fixture() {
  [ $# -ge 1 ] || usage
  local file extra=()
  file="$(resolved "$(database_file "$1")")"
  shift
  case "${1:-}" in
    "") ;;
    --missing-column) [ $# -eq 2 ] || usage; extra=(--missing-column "$2") ;;
    *) usage ;;
  esac
  below "$file" "$ROOT/work" "$ROOT/.tmp" || refuse "the fixture database goes below work/ or .tmp/ of this repository only"
  [ ! -e "$file" ] || refuse "the fixture database exists already"
  driver
  image
  mkdir -p "$(dirname "$file")"
  java_run -v "$(dirname "$file"):/db" -v "$ROOT/tests/import:/fixture:ro" -- \
    /fixture/FixtureDb.java "/db/$(basename "$file" .mv.db)" ${extra[@]+"${extra[@]}"}
}

cmd_selftest() {
  [ $# -eq 0 ] || usage
  driver
  image
  echo "driver $JAR: SHA-256 as pinned"
  java_run -- /tool/H2Export.java --selftest
}

case "${1:-}" in
  export) shift; cmd_export "$@" ;;
  fixture) shift; cmd_fixture "$@" ;;
  selftest) shift; cmd_selftest "$@" ;;
  *) usage ;;
esac
