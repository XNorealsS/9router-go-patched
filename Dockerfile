FROM golang:1.26-alpine AS go-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/9router-go ./cmd/9router-go/

FROM node:22-alpine AS ui-builder
ARG DASHBOARD_PKG=9router@0.5.119
RUN npm install -g "${DASHBOARD_PKG}" --omit=dev && npm cache clean --force

FROM node:22-alpine
RUN apk add --no-cache ca-certificates tzdata bash
COPY --from=go-builder /out/9router-go /usr/local/bin/9router-go
COPY --from=ui-builder /usr/local/lib/node_modules/9router /opt/9router-ui
COPY deploy/entrypoint.sh /usr/local/bin/9router-entrypoint.sh
RUN chmod +x /usr/local/bin/9router-entrypoint.sh

ENV HOST=0.0.0.0 \
    PORT=20128 \
    DATA_DIR=/data \
    DASHBOARD_PORT=3000 \
    DASHBOARD_URL=http://127.0.0.1:3000 \
    HOME=/data

VOLUME ["/data"]
EXPOSE 20128
ENTRYPOINT ["/usr/local/bin/9router-entrypoint.sh"]
