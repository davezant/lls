#!/usr/bin/env bash
# Build indexes for every language whose toolchain is installed.
HERE="$(cd "$(dirname "$0")" && pwd)"
have() { command -v "$1" >/dev/null 2>&1; }
have python3 && "$HERE/build-index.sh" python
have go      && "$HERE/build-index.sh" go
[ -d /usr/include ] && "$HERE/build-index.sh" c
have node    && "$HERE/build-index.sh" javascript
have rustc   && "$HERE/build-index.sh" rust
{ have lua || have luajit || [ -d /usr/share/lua ]; } && "$HERE/build-index.sh" lua
exit 0
