# Build stage
FROM golang:1.23 AS builder
WORKDIR /app
COPY go.mod go.sum main.go ./
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -o ordrd-group-x main.go

# Final stage
FROM ubuntu:latest
WORKDIR /app
COPY --from=builder /app/ordrd-group-x .
EXPOSE 5001
ENTRYPOINT ["./ordrd-group-x"]
