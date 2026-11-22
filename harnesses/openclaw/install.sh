#!/bin/sh
# Installs OpenClaw for the box image.
#
# Runs at image build time as an unprivileged user, in a folder holding this
# script, package.json and package-lock.json. Install scripts stay off.
set -eu

npm ci --ignore-scripts --omit=dev --no-audit --no-fund
npm cache clean --force

# Fail the build now if the command is missing, not later at run time.
test -x node_modules/.bin/openclaw
# The box image links bin/openclaw into the agent's PATH (a relative link, so
# it still works after the folder moves to /opt/ezbx/harnesses/openclaw).
mkdir bin
ln -s ../node_modules/.bin/openclaw bin/openclaw
