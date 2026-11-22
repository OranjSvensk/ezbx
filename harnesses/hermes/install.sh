#!/bin/sh
# Installs Hermes Agent (hermes-agent) for the box image.
#
# Runs at image build time as an unprivileged user, in a folder holding this
# script and requirements.lock. Every wheel is pinned by version and SHA-256
# hash, and --only-binary :all: with --no-deps refuses source tarballs so no
# package setup code runs during the build.
set -eu

python3 -m pip install --no-deps --require-hashes --only-binary :all: --no-cache-dir --target packages -r requirements.lock
# Marks the install tree as a container build so Hermes skips its PyPI update
# check on startup.
echo docker > packages/.install_method

# Fail the build now if the entry point is missing, not later at run time.
test -f packages/hermes_cli/main.py
mkdir bin
cat > bin/hermes <<'EOF'
#!/bin/sh
self=$(readlink -f "$0")
root=$(CDPATH= cd -- "$(dirname -- "$self")/.." && pwd)
PYTHONPATH="$root/packages" exec python3 -m hermes_cli.main "$@"
EOF
chmod 0755 bin/hermes
