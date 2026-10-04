FROM golang:1.24-alpine AS builder

WORKDIR /app

# Install dependencies
RUN apk add --no-cache git

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application (schema migrations are embedded in the binary and
# applied by the server at startup)
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o mailserver ./cmd/mailserver

# Final stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/mailserver .
COPY --from=builder /app/configs ./configs

EXPOSE 1143 1993 1587 1465 8080

CMD ["./mailserver", "-config", "/app/configs/config.yaml"]
