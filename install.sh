#!/bin/sh
# Installs the listo command-line tool.
#
#   curl -fsSL https://cli.listo.delivery/install.sh | sh
#
# It installs to $HOME/.listo/bin (override with LISTO_INSTALL_DIR), from the
# newest GitHub Release of listo-delivery/listo-cli (override with
# LISTO_VERSION=0.1.0). Before anything is installed it checks, in this order:
#
#   1. a signature over checksums.txt, made with Listo's release signing key
#      (the public half is typed below) and checked with openssl;
#   2. the archive's sha256 against the line in that checksums.txt.
#
# Nothing is written to the install folder unless both pass. The script then
# writes .listo-install-method ("script") beside the binary, which is how
# "listo update" knows it may replace itself.
#
# Environment: LISTO_INSTALL_DIR, LISTO_VERSION, and LISTO_RELEASES_URL (the
# releases address, https://github.com/listo-delivery/listo-cli/releases; a
# test points it at a local folder or server). The signing key cannot be
# overridden from the environment.

# THE ONLY TOP-LEVEL STATEMENTS ARE FUNCTION DEFINITIONS, COMMENTS, AND THE ONE
# CALL ON THE LAST LINE. `curl ... | sh` feeds the shell as the bytes arrive, so a
# download that stops anywhere must leave the shell nothing it can run: a function
# left without its closing brace is a syntax error and the shell stops, and a
# script that stops before the call defines functions and does nothing. That is why
# `set -eu`, the variables and the traps are INSIDE _main: a bare `set` (which
# prints every shell variable, and in a CI log that can be a secret) or a bare
# `trap` left by a cut would otherwise run. Every function name starts with an
# underscore so that a cut in the middle of a name is never the name of a real
# command (a macOS `say`). The call is a brace group so that a cut in the call is
# also unfinished, and the file has no newline after its last byte, so the only
# complete prefix is the whole script.
# A Go test reads the key below (the PUBLIC_KEY='...' line, so keep that shape), and
# a test of the tool's build cuts this file at every byte and runs each piece.

_say() { printf '%s\n' "$*"; }
_fail() {
  printf 'listo install: %s\n' "$*" >&2
  exit 1
}

_need() {
  command -v "$1" > /dev/null 2>&1 || _fail "$1 is required and was not found. $2"
}

_cleanup() {
  if [ -n "${TMP:-}" ]; then
    rm -rf "$TMP"
  fi
}

_main() {
  set -eu

  REPO="listo-delivery/listo-cli"
  RELEASES_URL="${LISTO_RELEASES_URL:-https://github.com/$REPO/releases}"
  INSTALL_DIR="${LISTO_INSTALL_DIR:-$HOME/.listo/bin}"

  # The release signing key (ECDSA P-256, PEM). cli/internal/update/signing_key.pem
  # holds the same text and a test keeps the two equal.
  PUBLIC_KEY='-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEBNEf4Sl73ibeGqLfivpVkqlehxpB
rigCfJhj8gwsZ8j4asFyk8EQKCFzc+z846Npw4lklTYtxmKJpbrFDWx+lg==
-----END PUBLIC KEY-----'

  TMP=""
  trap _cleanup EXIT
  trap 'exit 1' INT TERM

  case "$PUBLIC_KEY" in
    *PLACEHOLDER*) _fail "this copy of the install script has no release signing key, so it cannot verify a download. Get the script from https://cli.listo.delivery/install.sh." ;;
  esac

  _need curl "Install curl and run this again."
  _need openssl "It verifies the download's signature."
  _need tar "Install tar and run this again."

  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) _fail "this script supports Linux and macOS. On Windows, run: npm install -g listo-cli" ;;
  esac

  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) _fail "unsupported processor: $(uname -m). Supported: x86_64 and arm64." ;;
  esac

  # A shell running under Rosetta reports x86_64 on an Apple silicon Mac; the
  # native build is the right one.
  if [ "$os" = darwin ] && [ "$arch" = amd64 ] \
    && [ "$(sysctl -n sysctl.proc_translated 2> /dev/null || echo 0)" = 1 ]; then
    arch=arm64
  fi

  if command -v sha256sum > /dev/null 2>&1; then
    sha256_of() { sha256sum "$1" | awk '{print $1}'; }
  elif command -v shasum > /dev/null 2>&1; then
    sha256_of() { shasum -a 256 "$1" | awk '{print $1}'; }
  else
    _fail "sha256sum or shasum is required and neither was found."
  fi

  if [ -n "${LISTO_VERSION:-}" ]; then
    tag="v${LISTO_VERSION#v}"
  else
    # /releases/latest redirects to /releases/tag/<tag>; the final address names it.
    latest="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$RELEASES_URL/latest")" \
      || _fail "could not find the latest release at $RELEASES_URL/latest. Check your connection, or set LISTO_VERSION."
    tag="${latest##*/}"
    case "$tag" in
      v[0-9]*) ;;
      *) _fail "the latest release address did not name a version ($latest). Set LISTO_VERSION to install a specific one." ;;
    esac
  fi

  archive="listo_${os}_${arch}.tar.gz"
  base="$RELEASES_URL/download/$tag"

  TMP="$(mktemp -d)"
  _say "Installing listo $tag ($os/$arch)..."

  curl -fsSL -o "$TMP/checksums.txt" "$base/checksums.txt" || _fail "could not download $base/checksums.txt"
  curl -fsSL -o "$TMP/checksums.txt.sig" "$base/checksums.txt.sig" || _fail "could not download $base/checksums.txt.sig"
  curl -fsSL -o "$TMP/$archive" "$base/$archive" || _fail "could not download $base/$archive"

  printf '%s\n' "$PUBLIC_KEY" > "$TMP/key.pem"
  if ! openssl dgst -sha256 -verify "$TMP/key.pem" -signature "$TMP/checksums.txt.sig" "$TMP/checksums.txt" > /dev/null 2>&1; then
    _fail "the signature on checksums.txt does not match Listo's release key. Nothing was installed."
  fi

  expected="$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1 }' "$TMP/checksums.txt")"
  [ -n "$expected" ] || _fail "$archive is not listed in checksums.txt. Nothing was installed."
  actual="$(sha256_of "$TMP/$archive")"
  if [ "$expected" != "$actual" ]; then
    _fail "the checksum of $archive does not match. Nothing was installed."
  fi

  mkdir -p "$TMP/unpacked"
  tar -xzf "$TMP/$archive" -C "$TMP/unpacked" listo || _fail "could not unpack $archive."

  mkdir -p "$INSTALL_DIR" || _fail "could not create $INSTALL_DIR. Set LISTO_INSTALL_DIR to a folder you can write."
  cp "$TMP/unpacked/listo" "$INSTALL_DIR/.listo.new.$$" || _fail "could not write to $INSTALL_DIR."
  chmod 755 "$INSTALL_DIR/.listo.new.$$"
  mv -f "$INSTALL_DIR/.listo.new.$$" "$INSTALL_DIR/listo"
  printf 'script\n' > "$INSTALL_DIR/.listo-install-method"

  _say "Installed listo $tag to $INSTALL_DIR/listo"

  case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *)
      _say ""
      _say "Add listo to your PATH by putting this line in your shell profile:"
      _say ""
      _say "  export PATH=\"$INSTALL_DIR:\$PATH\""
      ;;
  esac

  _say ""
  _say "Next: run  listo login"
}

{ _main "$@"; }