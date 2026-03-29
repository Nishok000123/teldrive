# ─── Stage 1: Build ──────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

# git            – needed to read the commit SHA for build ldflags
# ca-certificates – needed to download UI assets and Go modules over HTTPS
RUN apk add --no-cache git ca-certificates

# Install go-task (taskfile runner)
RUN go install github.com/go-task/task/v3/cmd/task@latest

WORKDIR /build

# Download Go modules first (cache-friendly layer)
COPY go.mod go.sum ./
RUN go mod download

# Copy the full source tree
# internal/api is pre-generated and committed; no ogen invocation needed.
COPY . .

# Download pre-built frontend assets from GitHub Releases
RUN task ui

# Compile the static binary.
# internal/api is committed to the repository so ogen does not need to run
# here, avoiding the memory spike that previously caused an OOM-induced
# BuildKit EOF ("failed to receive status: rpc error … EOF").
RUN CGO_ENABLED=0 task build

# ─── Stage 2: Minimal runtime image ──────────────────────────────────────────
FROM alpine:3.20

# ca-certificates – required for TLS connections to Telegram
# tzdata          – required for timezone-aware scheduling
RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /build/bin/teldrive /teldrive

EXPOSE 8080
ENTRYPOINT ["/teldrive", "run"]
