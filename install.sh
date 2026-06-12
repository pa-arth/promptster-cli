#!/usr/bin/env bash
# Promptster installer
# Usage: curl -fsSL https://get.promptster.ai | sh
# Or:    PROMPTSTER_VERSION=0.2.9 curl -fsSL https://get.promptster.ai | sh
set -euo pipefail

REPO="pa-arth/promptster-cli-releases"
VERSION="${PROMPTSTER_VERSION:-latest}"
BINARY="promptster"

ok()    { printf '  \033[32m✓\033[0m  %s\n' "$*"; }
warn()  { printf '  \033[33m!\033[0m  %s\n' "$*" >&2; }
die()   { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

printf '\033[1m[1/4]\033[0m Detecting platform...\n'
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
RAW_ARCH="$(uname -m)"

case "${OS}" in
  linux|darwin) ;;
  *) die "unsupported OS: ${OS}" ;;
esac

case "${RAW_ARCH}" in
  x86_64) ARCH="x64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) die "unsupported architecture: ${RAW_ARCH}" ;;
esac

ASSET="${BINARY}-${OS}-${ARCH}"
ok "${OS}/${ARCH}"

printf '\033[1m[2/4]\033[0m Downloading CLI...\n'
if command -v curl >/dev/null 2>&1; then
  FETCH_BIN='curl -fsSL --progress-bar'
elif command -v wget >/dev/null 2>&1; then
  FETCH_BIN='wget -q --show-progress -O'
else
  die "curl or wget is required"
fi

TMP="$(mktemp)"
cleanup() { rm -f "${TMP}"; }
trap cleanup EXIT

if [ "${VERSION}" = "latest" ]; then
  URL="https://github.com/${REPO}/releases/latest/download/${ASSET}"
else
  URL="https://github.com/${REPO}/releases/download/cli-v${VERSION}/${ASSET}"
fi

if command -v curl >/dev/null 2>&1; then
  curl -fsSL --progress-bar "${URL}" -o "${TMP}"
else
  wget -q --show-progress "${URL}" -O "${TMP}"
fi

printf '\033[1m[3/4]\033[0m Installing...\n'
INSTALL_DIR="${HOME}/.promptster/bin"
mkdir -p "${INSTALL_DIR}"
DEST="${INSTALL_DIR}/${BINARY}"
mv "${TMP}" "${DEST}"
chmod +x "${DEST}"
ok "installed to ${DEST}"

printf '\033[1m[4/4]\033[0m Configuring PATH...\n'
PATH_ENTRY='export PATH="${HOME}/.promptster/bin:${PATH}"'
PATH_COMMENT='# Added by promptster installer'

case ":${PATH}:" in
  *":${INSTALL_DIR}:"*)
    ok "already in PATH"
    ;;
  *)
    ADDED=0
    for RC_FILE in "${HOME}/.zshrc" "${HOME}/.bashrc"; do
      if [ -f "${RC_FILE}" ]; then
        if grep -q '\.promptster/bin' "${RC_FILE}" 2>/dev/null; then
          ADDED=1
          continue
        fi
        printf '\n%s\n%s\n' "${PATH_COMMENT}" "${PATH_ENTRY}" >> "${RC_FILE}"
        ok "added PATH to ${RC_FILE}"
        ADDED=1
      fi
    done
    if [ "${ADDED}" -eq 0 ]; then
      printf '\n%s\n%s\n' "${PATH_COMMENT}" "${PATH_ENTRY}" >> "${HOME}/.bashrc"
      ok "created ${HOME}/.bashrc with PATH entry"
    fi
    warn "run this to use promptster now (or restart your shell):"
    warn "  source ~/.zshrc  # or: source ~/.bashrc"
    ;;
esac

printf '\n'
printf '\033[1mPromptster installed!\033[0m\n'
printf 'Get started:\n'
printf '  promptster start PST-XXXX-XXXX\n'
printf '\n'
