# Claude Code self-hosted runner image for arbetern projects. Build from the repository root:
#   docker build -f runner/Dockerfile --build-arg CLAUDE_CODE_VERSION=<version> .

FROM --platform=$BUILDPLATFORM golang:1.27-trixie AS spawn
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY cmd/runner-spawn/ cmd/runner-spawn/
# runner-spawn is stdlib only; GOPROXY=off keeps it that way.
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" GOPROXY=off \
    go build -trimpath -ldflags="-s -w" -o /out/runner-spawn ./cmd/runner-spawn

FROM --platform=$BUILDPLATFORM debian:trixie-slim AS claude
ARG CLAUDE_CODE_VERSION
ARG TARGETARCH
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl gnupg jq; \
    rm -rf /var/lib/apt/lists/*
WORKDIR /download
# gpg accepts a signature by any imported key; VALIDSIG ends with the signer's primary fingerprint, which must be the pinned one.
RUN set -eu; \
    version="${CLAUDE_CODE_VERSION:?set it with --build-arg CLAUDE_CODE_VERSION=<version>}"; \
    case "$TARGETARCH" in \
        amd64) platform=linux-x64 ;; \
        arm64) platform=linux-arm64 ;; \
        *) echo "unsupported TARGETARCH '$TARGETARCH'" >&2; exit 1 ;; \
    esac; \
    fingerprint=31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE; \
    base="https://downloads.claude.ai/claude-code-releases/$version"; \
    export GNUPGHOME=/download/gnupg; \
    install -d -m 0700 "$GNUPGHOME"; \
    curl -fsSL https://downloads.claude.ai/keys/claude-code.asc -o claude-code.asc; \
    gpg --batch --quiet --import claude-code.asc; \
    curl -fsSL "$base/manifest.json" -o manifest.json; \
    curl -fsSL "$base/manifest.json.sig" -o manifest.json.sig; \
    gpg --batch --status-fd 1 --verify manifest.json.sig manifest.json > verify.status; \
    grep -Eq "^\[GNUPG:] VALIDSIG .* $fingerprint\$" verify.status \
        || { echo "manifest.json is not signed by key $fingerprint" >&2; exit 1; }; \
    jq -e --arg v "$version" '.version == $v' manifest.json > /dev/null \
        || { echo "manifest.json is not for version $version" >&2; exit 1; }; \
    checksum="$(jq -r --arg p "$platform" '.platforms[$p].checksum // empty' manifest.json)"; \
    printf '%s\n' "$checksum" | grep -Eqx '[0-9a-f]{64}' \
        || { echo "manifest.json has no checksum for $platform" >&2; exit 1; }; \
    curl -fsSL "$base/$platform/claude" -o claude; \
    printf '%s  claude\n' "$checksum" | sha256sum --check --strict -; \
    install -m 0555 claude /usr/local/bin/claude

FROM --platform=$BUILDPLATFORM debian:trixie-slim AS layout
COPY --from=spawn /out/runner-spawn /opt/arbetern/bin/
COPY runner/bin/session-wrapper runner/bin/session-auth-header /opt/arbetern/bin/
COPY runner/claude-hooks/stop /opt/arbetern/claude-hooks/
COPY runner/runner-hooks/post-session /opt/arbetern/runner-hooks/
COPY runner/orchestrator-hooks/spawn-runner /opt/arbetern/orchestrator-hooks/
RUN chown -R root:root /opt/arbetern && chmod -R 0555 /opt/arbetern

FROM debian:trixie-slim
ARG EXTRA_APT_PACKAGES=""
RUN set -eux; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates git curl jq bubblewrap socat $EXTRA_APT_PACKAGES; \
    rm -rf /var/lib/apt/lists/*; \
    groupadd --gid 10001 runner; \
    useradd --uid 10001 --gid 10001 --no-log-init --create-home --home-dir /home/runner --shell /bin/bash runner; \
    git config --system --add safe.directory '*'
COPY --from=claude /usr/local/bin/claude /usr/local/bin/claude
# Copying /opt (not /opt/arbetern) keeps the 0555 mode on /opt/arbetern itself.
COPY --from=layout /opt/ /opt/
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/claude"]
