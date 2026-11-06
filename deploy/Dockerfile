# ---- frontend ----
FROM --platform=$BUILDPLATFORM node:22-alpine AS frontend
WORKDIR /web
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --cache /tmp/npm-cache
COPY frontend/ ./
RUN npm run build

# ---- go control plane ----
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG APP_VERSION=dev
ARG APP_COMMIT=unknown
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /web/dist/ ./internal/webui/static/
	RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -trimpath -ldflags "-s -w -X github.com/caigee-cmd/cli2api/internal/buildinfo.Version=${APP_VERSION} -X github.com/caigee-cmd/cli2api/internal/buildinfo.Commit=${APP_COMMIT}" -o /out/qoder-api-proxy ./cmd/server \
	  && CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -trimpath -ldflags "-s -w -X github.com/caigee-cmd/cli2api/internal/buildinfo.Version=${APP_VERSION} -X github.com/caigee-cmd/cli2api/internal/buildinfo.Commit=${APP_COMMIT}" -o /out/cli2api-updater ./cmd/updater

# ---- single runtime ----
FROM node:20-bookworm-slim
RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates curl tini chromium \
  && rm -rf /var/lib/apt/lists/* \
	  && npm install -g @qoder-ai/qodercli@1.1.32 @qodercn-ai/qoderclicn@1.1.32
WORKDIR /app
COPY --from=build /out/qoder-api-proxy /app/qoder-api-proxy
COPY --from=build /out/cli2api-updater /app/cli2api-updater
COPY worker/package.json worker/package-lock.json /app/worker/
RUN cd /app/worker && npm ci --omit=dev
COPY worker/src /app/worker
COPY worker/NOTICE /app/worker/NOTICE
COPY worker/last-plain.sample.json /app/worker/last-plain.sample.json
RUN test -f /app/worker/daemon.mjs \
  && test -f /app/worker/compat.mjs \
  && test -d /app/worker/node_modules/undici
# The ZCode plan channel solves Aliyun's traceless verification in a real
# browser, so the runtime image carries chromium plus the puppeteer driver that
# wraps it. Providers point at these paths through the environment.
COPY deploy/captcha_node/package.json deploy/captcha_node/package-lock.json /app/captcha_node/
RUN cd /app/captcha_node && npm ci --omit=dev
COPY deploy/captcha_node /app/captcha_node
RUN test -f /app/captcha_node/solver.js && test -x /usr/bin/chromium
RUN mkdir -p /data && chmod 700 /data
ENV HOST=0.0.0.0 \
    PORT=3010 \
    QODER_DATA_DIR=/data \
    QODER_RUNTIME_DIR=/run/cli2api \
    QODER_NODE_BINARY=/usr/local/bin/node \
    QODER_WORKER_DAEMON=/app/worker/daemon.mjs \
    QODERCLI_JS=/usr/local/lib/node_modules/@qoder-ai/qodercli/bundle/qodercli.js \
    QODERCNCLI_JS=/usr/local/lib/node_modules/@qodercn-ai/qoderclicn/bundle/qoderclicn.js \
    PLAIN_TEMPLATE_PATH=/app/worker/last-plain.sample.json \
    ZCODE_CAPTCHA_DIR=/app/captcha_node \
    ZCODE_CHROMIUM_PATH=/usr/bin/chromium \
    ZCODE_NODE_BINARY=/usr/local/bin/node \
    QODER_WORKER_BASE_PORT=32100
EXPOSE 3010
ENTRYPOINT ["/usr/bin/tini","--"]
CMD ["/app/qoder-api-proxy"]
