FROM golang:1.26-bookworm AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /arbetern .

FROM gcr.io/distroless/static:nonroot

WORKDIR /app

COPY --from=builder /arbetern /app/arbetern
COPY agents/ /app/agents/

ENTRYPOINT ["/app/arbetern"]
