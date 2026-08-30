FROM golang:1.27-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /coderouter ./cmd/server

FROM alpine:3.19
RUN apk --no-cache add ca-certificates wget \
    && adduser -D -u 10001 coderouter
COPY --from=builder /coderouter /coderouter

USER coderouter
EXPOSE 8080

# The platform uses this to decide when a new container is ready to take
# traffic and when to restart an unhealthy one.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD wget -qO- "http://127.0.0.1:${PORT:-8080}/health" >/dev/null 2>&1 || exit 1

CMD ["/coderouter"]
