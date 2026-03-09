# ── Build stage ──
FROM golang:1.23-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /monet-bot .

# ── Runtime stage ──
FROM alpine:3.20

RUN apk add --no-cache ca-certificates git tzdata

# Create non-root user
RUN adduser -D -h /app monet
WORKDIR /app

# Copy binary
COPY --from=builder /monet-bot /usr/local/bin/monet-bot

# Copy default workspace files (SOUL.md, AGENTS.md, skills, etc.)
COPY workspace/ /app/workspace-default/

# Ports: 9000 = Lark webhook, 8080 = Dashboard
EXPOSE 9000 8080

USER monet

ENTRYPOINT ["monet-bot"]
CMD ["run", "--channel", "all"]
