#!/usr/bin/env bash
# Build the lls index(es) for one language, auto-detecting library locations.
#
#   scripts/build-index.sh LANG [EXTRA_ROOT...]
#
# Indexes are written to $LLS_HOME/<lang>/ (default ~/.local/share/lls).
# Extra roots are indexed into "<lang>/extra.llsi" (e.g. a venv's
# site-packages or a project's node_modules).
set -euo pipefail
LANG_NAME="${1:?usage: build-index.sh LANG [EXTRA_ROOT...]}"; shift || true
LLS="${LLS:-$(command -v lls || echo "$(dirname "$0")/../bin/lls")}"
HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
build() { "$LLS" build -l "$LANG_NAME" "$@"; }
have() { command -v "$1" >/dev/null 2>&1; }

case "$LANG_NAME" in
  python)
    PY="${PYTHON:-python3}"
    STD="$("$PY" -c 'import sysconfig; print(sysconfig.get_paths()["stdlib"])')"
    "$PY" -I "$HERE/introspect/python.py" "$TMP/py.jsonl"
    build --name stdlib --jsonl "$TMP/py.jsonl" -r "$STD"
    SITE="$("$PY" -c 'import site,sys; print(" ".join(p for p in site.getsitepackages()+[site.getusersitepackages()]))' 2>/dev/null || true)"
    ROOTS=(); for d in $SITE; do [ -d "$d" ] && ROOTS+=(-r "$d"); done
    [ ${#ROOTS[@]} -gt 0 ] && build --name site-packages "${ROOTS[@]}" || true ;;
  go)
    build --name stdlib -r "$(go env GOROOT)/src"
    MOD="$(go env GOMODCACHE)"
    [ -n "${LLS_GO_MODCACHE:-}" ] && [ -d "$MOD" ] && build --name modcache -r "$MOD" || true ;;
  c)
    MA="$(gcc -print-multiarch 2>/dev/null || true)"
    if [ -n "$MA" ] && [ -d "/usr/include/$MA" ]; then
      build --name system -r /usr/include --exclude "$MA"
      build --name multiarch -r "/usr/include/$MA"
    else
      build --name system -r /usr/include
    fi
    GCCINC="$(gcc -print-file-name=include 2>/dev/null || true)"
    [ -d "$GCCINC" ] && build --name compiler -r "$GCCINC" || true ;;
  javascript)
    if have node; then
      node --no-warnings --no-deprecation "$HERE/introspect/node.js" "$TMP/node.jsonl"
      build --name node --jsonl "$TMP/node.jsonl"
      G="$(npm root -g 2>/dev/null || true)"
      [ -d "$G" ] && build --name global -r "$G" || true
    fi ;;
  rust)
    SR="$(rustc --print sysroot)/lib/rustlib/src/rust/library"
    [ -d "$SR" ] || { echo "install sources: rustup component add rust-src" >&2; exit 1; }
    build --name std -r "$SR" ;;
  lua)
    LUA="$(command -v lua || command -v luajit || command -v lua5.4 || true)"
    [ -n "$LUA" ] && "$LUA" "$HERE/introspect/lua.lua" "$TMP/lua.jsonl" && build --name stdlib --jsonl "$TMP/lua.jsonl"
    ROOTS=(); for d in /usr/share/lua/* /usr/local/share/lua/*; do [ -d "$d" ] && ROOTS+=(-r "$d"); done
    [ ${#ROOTS[@]} -gt 0 ] && build --name share "${ROOTS[@]}" || true ;;
  *)
    # Any other language with a spec: index the given roots.
    [ $# -gt 0 ] || { echo "no auto-detection for $LANG_NAME; pass roots" >&2; exit 1; } ;;
esac

if [ $# -gt 0 ]; then
  R=(); for d in "$@"; do R+=(-r "$d"); done
  build --name extra "${R[@]}"
fi
