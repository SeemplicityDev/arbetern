FROM golang:1.27-trixie AS builder
# Pinned so a rebuild of a given commit produces the same binary. Bump
# deliberately; "latest" would silently change the build inputs.
ARG YQ_VERSION=v4.53.6
ARG LINGUIST_REF=v9.7.0

RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    curl -fsSL "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/yq_linux_${arch}" -o /usr/local/bin/yq; \
    chmod +x /usr/local/bin/yq; \
    yq --version

# Fetch all known programming-language file extensions from GitHub Linguist,
# pinned to a release tag rather than a moving branch.
RUN curl -fsSL "https://raw.githubusercontent.com/github-linguist/linguist/${LINGUIST_REF}/lib/linguist/languages.yml" -o /tmp/languages.yml

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN yq '[.[] | select(has("extensions")) | .extensions[]] | unique | sort' /tmp/languages.yml -o=json \
    > config/extensions.json

RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /arbetern .

FROM gcr.io/distroless/static:nonroot

WORKDIR /app

COPY --from=builder /arbetern /app/arbetern
COPY agents/ /app/agents/

ENTRYPOINT ["/app/arbetern"]
