FROM golang:1.26-alpine AS go-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/9router-go ./cmd/9router-go/

FROM node:22-alpine
ARG DASHBOARD_PKG=9router@0.5.119
RUN apk add --no-cache ca-certificates tzdata bash \
    && npm install -g "${DASHBOARD_PKG}" --omit=dev && npm cache clean --force
COPY --from=go-builder /out/9router-go /opt/9router/bin/9router-go
COPY deploy/start.sh /opt/9router/bin/start.sh
RUN chmod +x /opt/9router/bin/start.sh /opt/9router/bin/9router-go

ENV HOST=0.0.0.0 \
    PORT=20128 \
    DATA_DIR=/data \
    HOME=/data

VOLUME ["/data"]
EXPOSE 20128
ENTRYPOINT ["/opt/9router/bin/start.sh"]
