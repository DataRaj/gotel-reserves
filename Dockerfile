# ── Stage 1: build ────────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache upx

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -trimpath -o /bin/recallo ./cmd/api

RUN upx --best --lzma /bin/recallo

# ── Stage 2: runtime ──────────────────────────────────────────────────────────
FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /bin/recallo /recallo

EXPOSE 8080

ENTRYPOINT ["/recallo"]
