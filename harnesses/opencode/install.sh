#!/bin/sh
# Installs OpenCode for the box image.
#
# Runs at image build time as an unprivileged user, in a folder holding this
# script, package.json and package-lock.json.
#
# OpenCode's own npm package (@opencode/cli) is only a launcher: its command
# is a stub until its postinstall script copies the real program out of a
# platform package, and that script runs npm install by itself when the
# platform package is missing. ezbx keeps install scripts off, so the
# lockfile pins the platform package directly instead, with its integrity
# hash. It holds one prebuilt program and has no install script or
# dependencies.
#
# The baseline build is used because it runs on every x64 CPU; the other
# build needs AVX2, which would make the image depend on the computer that
# built it.
set -eu

npm ci --ignore-scripts --omit=dev --no-audit --no-fund
npm cache clean --force

# Fail the build now if the program is missing, not later at run time.
program=node_modules/@opencode/cli-linux-x64-baseline/bin/opencode
test -f "$program"
chmod 0755 "$program"
# The box image links bin/opencode into the agent's PATH (a relative link, so
# it still works after the folder moves to /opt/ezbx/harnesses/opencode).
mkdir bin
ln -s "../$program" bin/opencode
