# Build stage using Go 1.23
FROM golang:1.23 AS builder

WORKDIR /app
# Copy go.mod and go.sum to download dependencies.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code.
COPY . .

# Build the binary.
RUN CGO_ENABLED=0 GOOS=linux go build -o ldap-sync .

# Final stage using Ubuntu.
FROM ubuntu:latest

# Install necessary certificates.
RUN apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/*

WORKDIR /app
# Copy the built binary from the builder stage.
COPY --from=builder /app/ldap-sync /app/ldap-sync

EXPOSE 8080
ENTRYPOINT ["/app/ldap-sync"]
