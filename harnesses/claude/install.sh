#!/bin/sh
# Installs Claude Code for the box image.
#
# Runs at image build time as an unprivileged user, in a folder holding this
# script, package.json and package-lock.json.
#
# The main @anthropic-ai/claude-code npm package ships a stub command until
# its postinstall script (install.cjs) copies the real native program out of
# a platform package. ezbx keeps install scripts off, so the lockfile pins
# @anthropic-ai/claude-code-linux-x64 directly instead, with its integrity
# hash. It holds one prebuilt program and has no install script or
# dependencies.
set -eu

npm ci --ignore-scripts --omit=dev --no-audit --no-fund
npm cache clean --force

# Fail the build now if the program is missing, not later at run time.
program=node_modules/@anthropic-ai/claude-code-linux-x64/claude
test -f "$program"
chmod 0755 "$program"
# The box image links bin/claude into the agent's PATH (a relative link, so it
# still works after the folder moves to /opt/ezbx/harnesses/claude).
mkdir bin
ln -s "../$program" bin/claude
