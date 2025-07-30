# Build stage
FROM golang:1.21-alpine AS builder

# Install git and ca-certificates
RUN apk add --no-cache git ca-certificates

# Set working directory
WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o prometheus-multi-tenant-proxy ./cmd/proxy

# Final stage
FROM alpine:3.19

# Install curl
RUN apk add --no-cache curl ca-certificates

# Copy the binary
COPY --from=builder /app/prometheus-multi-tenant-proxy /prometheus-multi-tenant-proxy

# Create non-root user
USER 65534:65534

# Expose port
EXPOSE 8080

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/prometheus-multi-tenant-proxy", "--help"]

# Run the binary
ENTRYPOINT ["/prometheus-multi-tenant-proxy"] 