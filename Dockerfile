FROM golang:1.26-bookworm AS builder
RUN curl -fsSL https://github.com/mikefarah/yq/releases/latest/download/yq_linux_amd64 -o /usr/local/bin/yq \
    && chmod +x /usr/local/bin/yq

# Fetch all known programming-language file extensions from GitHub Linguist.
RUN curl -sf https://raw.githubusercontent.com/github-linguist/linguist/master/lib/linguist/languages.yml -o /tmp/languages.yml

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN yq '[.[] | select(has("extensions")) | .extensions[]] | . + ["dockerfile"] | unique | sort' /tmp/languages.yml -o=json \
    > config/extensions.json

RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /arbetern .

FROM gcr.io/distroless/static:nonroot

WORKDIR /app

COPY --from=builder /arbetern /app/arbetern
COPY agents/ /app/agents/

ENTRYPOINT ["/app/arbetern"]
