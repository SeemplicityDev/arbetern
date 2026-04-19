FROM golang:1.26-bookworm AS builder
RUN curl -fsSL https://github.com/mikefarah/yq/releases/latest/download/yq_linux_amd64 -o /usr/local/bin/yq \
    && chmod +x /usr/local/bin/yq

# Fetch all known programming-language file extensions from GitHub Linguist.
RUN curl -sf https://raw.githubusercontent.com/github-linguist/linguist/master/lib/linguist/languages.yml -o /tmp/languages.yml

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN yq '[.[] | select(has("extensions")) | .extensions[]] | unique | sort' /tmp/languages.yml -o=json \
    > config/extensions.json

RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /arbetern .

# Pre-create the dashboards data directory owned by the distroless `nonroot`
# user (uid/gid 65532) so the runtime can persist JSON snapshots even when
# DASHBOARDS_DIR points at an unmounted default path.
RUN mkdir -p /var/lib/arbetern/dashboards \
    && chown -R 65532:65532 /var/lib/arbetern

FROM gcr.io/distroless/static:nonroot

WORKDIR /app

COPY --from=builder /arbetern /app/arbetern
COPY agents/ /app/agents/
COPY --from=builder --chown=65532:65532 /var/lib/arbetern /var/lib/arbetern

ENTRYPOINT ["/app/arbetern"]
