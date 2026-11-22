#!/bin/sh
# Installs oh-my-pi (omp) for the box image.
#
# Runs at image build time as an unprivileged user, in a folder holding this
# script, package.json and package-lock.json.
#
# @oh-my-pi/pi-coding-agent uses the Bun runtime (its entry script starts
# with #!/usr/bin/env bun). The lockfile pins @oven/bun-linux-x64-baseline
# beside it, so the baseline Bun executable is checked by its integrity hash
# and runs on every x64 CPU.
set -eu

npm ci --ignore-scripts --omit=dev --no-audit --no-fund
npm cache clean --force

if [ -f node_modules/@oven/bun-linux-x64-baseline/bin/bun ]; then
  bun_bin=node_modules/@oven/bun-linux-x64-baseline/bin/bun
else
  bun_bin=node_modules/@oven/bun-linux-x64-baseline/bun
fi
cli=node_modules/@oh-my-pi/pi-coding-agent/dist/cli.js
test -f "$bun_bin"
test -f "$cli"
chmod 0755 "$bun_bin"

mkdir bin
ln -s "../$bun_bin" bin/bun
cat > bin/omp <<'EOF'
#!/bin/sh
self=$(readlink -f "$0")
dir=$(CDPATH= cd -- "$(dirname -- "$self")" && pwd)
PATH="$dir:$PATH"
exec "$dir/bun" "$dir/../node_modules/@oh-my-pi/pi-coding-agent/dist/cli.js" "$@"
EOF
chmod 0755 bin/omp
