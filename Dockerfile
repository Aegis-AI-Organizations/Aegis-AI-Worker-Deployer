# Optimized Dockerfile for Go project
# Stage 1: Build
FROM golang:alpine AS builder
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download || true
COPY . .
# Build static binary with no debug info (-s -w) to reduce size
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o /go/bin/app ./cmd/deployer || echo "Build failed or no Go code"

# Stage 2: Runtime with Docker CLI for importing archived local images.
FROM alpine:3.22
RUN apk add --no-cache ca-certificates docker-cli
COPY --from=builder /go/bin/app /app
ENTRYPOINT ["/app"]
