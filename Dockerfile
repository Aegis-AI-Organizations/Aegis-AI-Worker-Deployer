# Optimized Dockerfile for Go project
# Stage 1: Build
FROM golang:alpine AS builder
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download || true
COPY . .
# Build static binary with no debug info (-s -w) to reduce size
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o /go/bin/app ./cmd/deployer || echo "Build failed or no Go code"

# Stage 2: Minimal Runtime (scratch is an empty image)
FROM scratch
# Copy CA certificates in case out-bound HTTPS calls are required
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
# Copy only the compiled static binary
COPY --from=builder /go/bin/app /app
ENTRYPOINT ["/app"]
