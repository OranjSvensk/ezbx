#!/bin/sh
# Installs OpenAI Codex CLI for the box image.
#
# Runs at image build time as an unprivileged user, in a folder holding this
# script, package.json and package-lock.json.
#
# The main @openai/codex npm package is a Node wrapper that looks up its
# prebuilt native Linux x64 musl program in the platform package
# @openai/codex@<version>-linux-x64. The lockfile pins that platform package
# directly, with its integrity hash, and installs with scripts off.
set -eu

npm ci --ignore-scripts --omit=dev --no-audit --no-fund
npm cache clean --force

# Fail the build now if the program is missing, not later at run time.
program=node_modules/@openai/codex/vendor/x86_64-unknown-linux-musl/bin/codex
test -f "$program"
chmod 0755 "$program"
# The box image links bin/codex into the agent's PATH (a relative link, so it
# still works after the folder moves to /opt/ezbx/harnesses/codex).
mkdir bin
ln -s "../$program" bin/codex
