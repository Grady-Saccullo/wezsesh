#!/bin/sh
# wezsesh curl-installer (T-1002).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/Grady-Saccullo/wezsesh/main/install.sh | sh
#
# Env overrides:
#   WEZSESH_VERSION     pin a specific tag (e.g. v0.1.0, v0.1.0-rc1).
#                       leading 'v' is auto-prepended if missing. defaults to
#                       the GitHub "latest" release (which excludes pre-releases).
#   WEZSESH_INSTALL_DIR install destination. defaults to $HOME/.local/bin.
#
# The script downloads the matching release tarball, verifies its sha256
# against the published SHA256SUMS, extracts it, and installs the binary.
# It does NOT modify shell init files; if the install dir is not on $PATH
# the script prints a hint and exits successfully.

set -eu
# pipefail is bash/ksh/zsh, not POSIX. Probe and enable when supported so we
# still catch failures inside `cmd1 | cmd2` on shells that have it (e.g. bash
# invoked as /bin/sh on Linux distros that symlink /bin/sh -> /bin/bash).
# shellcheck disable=SC3040
(set -o pipefail 2>/dev/null) && set -o pipefail || true

REPO="Grady-Saccullo/wezsesh"
GITHUB_API="https://api.github.com/repos/${REPO}/releases/latest"
RELEASE_BASE="https://github.com/${REPO}/releases/download"

# Step name for the ERR trap. Each major phase reassigns this so a failure
# inside that phase prints something more useful than "line N exited 1".
WEZSESH_STEP="initialising"

err() {
	printf 'wezsesh-install: error: %s\n' "$1" >&2
}

# Initialised once mktemp succeeds; cleanup tolerates the unset case.
TMPDIR_W=""

cleanup() {
	[ -n "${TMPDIR_W}" ] && [ -d "${TMPDIR_W}" ] && rm -rf "${TMPDIR_W}"
}

on_exit() {
	rc=$?
	cleanup
	if [ "${rc}" -ne 0 ]; then
		# $LINENO is bash-only under POSIX `sh`; the trap reports the
		# named step instead so the user can locate the failing phase.
		err "step '${WEZSESH_STEP}' failed (exit ${rc})"
	fi
	exit "${rc}"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

need() {
	command -v "$1" >/dev/null 2>&1 || {
		err "required tool '$1' not found in PATH"
		exit 1
	}
}

# --- detect platform ---------------------------------------------------------
WEZSESH_STEP="detecting platform"

uname_s=$(uname -s)
case "${uname_s}" in
	Linux)  OS=linux ;;
	Darwin) OS=darwin ;;
	*)
		err "unsupported OS: ${uname_s} (only Linux and Darwin are supported)"
		exit 1
		;;
esac

uname_m=$(uname -m)
case "${uname_m}" in
	x86_64|amd64)        ARCH=amd64 ;;
	aarch64|arm64)       ARCH=arm64 ;;
	*)
		err "unsupported architecture: ${uname_m} (only amd64 and arm64 are supported)"
		exit 1
		;;
esac

# --- detect tools ------------------------------------------------------------
WEZSESH_STEP="detecting required tools"

need uname
need tar
need mktemp
need install
need grep
need sed

# A downloader: prefer curl (we know it exists when the user piped us through
# `curl | sh`, but the script must also work from a saved file under wget).
if command -v curl >/dev/null 2>&1; then
	DL=curl
elif command -v wget >/dev/null 2>&1; then
	DL=wget
else
	err "neither curl nor wget is installed; cannot download release artefacts"
	exit 1
fi

# A SHA256 verifier. macOS ships shasum but not sha256sum by default; Linux is
# the inverse on most distros. Either is acceptable.
if command -v sha256sum >/dev/null 2>&1; then
	SHA=sha256sum
elif command -v shasum >/dev/null 2>&1; then
	SHA="shasum -a 256"
else
	err "neither sha256sum nor shasum is installed; cannot verify release"
	exit 1
fi

# fetch_to <url> <dest-file>
fetch_to() {
	if [ "${DL}" = curl ]; then
		curl -fsSL -o "$2" "$1"
	else
		wget -q -O "$2" "$1"
	fi
}

# fetch_stdout <url>  (used for the GitHub API JSON; small payload, single line
# match is enough).
fetch_stdout() {
	if [ "${DL}" = curl ]; then
		curl -fsSL "$1"
	else
		wget -q -O - "$1"
	fi
}

# --- resolve tag -------------------------------------------------------------
WEZSESH_STEP="resolving release tag"

if [ "${WEZSESH_VERSION-}" != "" ]; then
	TAG="${WEZSESH_VERSION}"
	case "${TAG}" in
		v*) ;;
		*) TAG="v${TAG}" ;;
	esac
else
	# tag_name is a stable, single-line JSON field. Avoid jq so the installer
	# has no extra dependencies; the field shape is "tag_name": "vX.Y.Z".
	api_json=$(fetch_stdout "${GITHUB_API}")
	TAG=$(printf '%s\n' "${api_json}" \
		| grep -o '"tag_name"[[:space:]]*:[[:space:]]*"[^"]*"' \
		| head -n 1 \
		| sed -e 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/')
	if [ -z "${TAG}" ]; then
		err "could not parse tag_name from ${GITHUB_API}; set WEZSESH_VERSION to override"
		exit 1
	fi
fi

ASSET="wezsesh_${TAG}_${OS}_${ARCH}.tar.gz"
ASSET_URL="${RELEASE_BASE}/${TAG}/${ASSET}"
SUMS_URL="${RELEASE_BASE}/${TAG}/SHA256SUMS"

# --- download ----------------------------------------------------------------
WEZSESH_STEP="creating temp workspace"

TMPDIR_W=$(mktemp -d 2>/dev/null || mktemp -d -t wezsesh-install)

WEZSESH_STEP="downloading ${ASSET}"
fetch_to "${ASSET_URL}" "${TMPDIR_W}/${ASSET}"

WEZSESH_STEP="downloading SHA256SUMS"
fetch_to "${SUMS_URL}" "${TMPDIR_W}/SHA256SUMS"

# --- verify checksum ---------------------------------------------------------
WEZSESH_STEP="verifying checksum"

# Extract just the line we care about. Running `--check` against the full
# SHA256SUMS would also try to verify the other three tarballs (which we did
# not download) and trip "FAILED open or read" on each. Filtering to one line
# also future-proofs against new assets being added to the SHA256SUMS file.
expected_line=$(grep "  ${ASSET}\$" "${TMPDIR_W}/SHA256SUMS" || true)
if [ -z "${expected_line}" ]; then
	err "no SHA256SUMS entry for ${ASSET}; release may be incomplete"
	exit 1
fi
printf '%s\n' "${expected_line}" > "${TMPDIR_W}/SHA256SUMS.one"

(
	cd "${TMPDIR_W}"
	# shellcheck disable=SC2086
	${SHA} -c SHA256SUMS.one >/dev/null
)

# --- extract -----------------------------------------------------------------
WEZSESH_STEP="extracting tarball"

tar -xzf "${TMPDIR_W}/${ASSET}" -C "${TMPDIR_W}"
SRC_BIN="${TMPDIR_W}/wezsesh_${TAG}_${OS}_${ARCH}/wezsesh"
if [ ! -f "${SRC_BIN}" ]; then
	err "binary missing from tarball at expected path wezsesh_${TAG}_${OS}_${ARCH}/wezsesh"
	exit 1
fi

# --- install -----------------------------------------------------------------
WEZSESH_STEP="installing binary"

INSTALL_DIR="${WEZSESH_INSTALL_DIR:-${HOME}/.local/bin}"
mkdir -p "${INSTALL_DIR}"
# `install -m 0755` overwrites atomically (rename-on-write on most platforms)
# and works on both GNU coreutils (Linux) and BSD install (macOS).
install -m 0755 "${SRC_BIN}" "${INSTALL_DIR}/wezsesh"

# --- post-install hint -------------------------------------------------------
WEZSESH_STEP="post-install"

case ":${PATH}:" in
	*":${INSTALL_DIR}:"*) ;;
	*)
		printf 'wezsesh-install: warning: %s is not on $PATH.\n' "${INSTALL_DIR}" >&2
		printf 'wezsesh-install: add it with, e.g.:\n' >&2
		printf '    echo '\''export PATH="%s:$PATH"'\'' >> ~/.profile\n' "${INSTALL_DIR}" >&2
		;;
esac

printf 'wezsesh %s installed to %s/wezsesh\n' "${TAG}" "${INSTALL_DIR}"
