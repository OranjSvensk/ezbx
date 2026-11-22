# ezbx box image: Node, git, the box's harnesses and the chosen tools, for
# an unprivileged user.
#
# ezbx build builds it from the files carried inside the ezbx program and
# passes the harnesses and the chosen tools' Debian packages as build
# arguments. To build it by hand from the repository root (for development;
# Pi only, no extra tools):
#   docker build -f docker/box.Dockerfile --build-arg HARNESS_COMMANDS=pi=pi -t ezbx-box:dev .
#
# Run it with every capability dropped and no-new-privileges, attached only
# to the box's inside network (see docs/isolation-test.md).

# Node 24 LTS on Debian 12, pinned by digest so a rebuild starts from the
# same base. The value is listed in ezbx.lock.
ARG NODE_IMAGE=node:24.22.3-bookworm-slim@sha256:dc26ae92c9fb15cf6b1d63e20f0944b2efe0f115b20e00be48fb23fceb061c54

FROM ${NODE_IMAGE} AS base
# Debian packages come from the dated snapshot the base image was made from,
# which its apt sources name in a comment. So the base digest also fixes
# every package version, and a rebuild gets the same files. apt still checks
# the snapshot's signatures; only the "valid until" date is ignored, because
# an old snapshot's release file has expired.
RUN f=/etc/apt/sources.list.d/debian.sources \
 && sed -i -E '/^# http:\/\/snapshot\.debian\.org\//{s/^# //;h;d};/^URIs:/{x;s/^/URIs: /}' "$f" \
 && grep -q '^URIs: http://snapshot\.debian\.org/archive/' "$f" \
 && ! grep '^URIs:' "$f" | grep -qv 'snapshot\.debian\.org/archive/' \
 && echo 'Acquire::Check-Valid-Until "false";' > /etc/apt/apt.conf.d/99ezbx-snapshot

# Harness install: runs as the base image's unprivileged "node" user, so no
# package code could run as root even if install scripts were on.
# HARNESS_COMMANDS lists the box's harnesses as id=command pairs, such as
# "opencode=opencode pi=pi"; ezbx build fills it from their manifests, and
# the build files hold only those harnesses' folders. Each install script
# leaves its command in bin/ in its folder, and the command must answer
# --version.
FROM base AS harnesses
ARG HARNESS_COMMANDS=""
RUN mkdir /build && chown node:node /build
USER node
WORKDIR /build
COPY --chown=node:node harnesses/ ./
RUN test -n "$HARNESS_COMMANDS" \
 && for h in $HARNESS_COMMANDS; do \
      id="${h%%=*}"; cmd="${h#*=}"; \
      (cd "$id" && sh install.sh) || exit 1; \
      "$id/bin/$cmd" --version >/dev/null || { echo "harness $id: $id/bin/$cmd --version failed" >&2; exit 1; }; \
    done

FROM base
# The chosen tools, as Debian package names, the commands they must provide
# (checked below) and extra command names as name=target, such as fd=fdfind.
# ezbx build fills all three from the tool catalogue.
ARG DEBIAN_PACKAGES=""
ARG TOOL_COMMANDS=""
ARG TOOL_LINKS=""
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git $DEBIAN_PACKAGES \
 && rm -rf /var/lib/apt/lists/*

# The agent user replaces the base image's "node" user at the same id. No
# sudo is installed, and setuid and setgid bits are removed from every file,
# so nothing in the image can raise privileges.
RUN userdel -r node \
 && groupadd -g 1000 agent \
 && useradd -m -u 1000 -g agent -s /bin/bash agent \
 && mkdir /workspace && chown agent:agent /workspace \
 && find / -xdev -perm /6000 -type f -exec chmod a-s {} +

# Harness files belong to root, so the agent can run the harnesses but not
# change them.
ARG HARNESS_COMMANDS=""
COPY --from=harnesses --chown=root:root /build /opt/ezbx/harnesses
RUN for h in $HARNESS_COMMANDS; do \
      id="${h%%=*}"; cmd="${h#*=}"; \
      ln -s "/opt/ezbx/harnesses/$id/bin/$cmd" "/usr/local/bin/$cmd" || exit 1; \
      command -v "$cmd" >/dev/null || { echo "the harness command $cmd is missing" >&2; exit 1; }; \
    done \
 && for l in $TOOL_LINKS; do \
      ln -s "$(command -v "${l#*=}")" "/usr/local/bin/${l%%=*}" || exit 1; \
    done \
 && for c in $TOOL_COMMANDS; do \
      command -v "$c" >/dev/null || { echo "the tool command $c is missing" >&2; exit 1; }; \
    done

# Each harness's environment (such as Pi's PI_OFFLINE) comes from its
# manifest, set by ezbx up on the box container, not from the image.
USER agent
WORKDIR /workspace
LABEL ezbx.managed="true"
CMD ["bash"]
