# SPDX-License-Identifier: AGPL-3.0-only
# OpenPrintHQ Link server, run in the hub's stack. It needs NET_ADMIN for the iptables redirect in its namespace.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS TARGETARCH
ARG OPHQ_APP_COMMIT=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X main.version=$(printf %.7s "$OPHQ_APP_COMMIT")" -o /out/link-hub ./cmd/link-hub

FROM alpine:3.22
LABEL org.opencontainers.image.source=https://github.com/norjms/artisanhub-link org.opencontainers.image.licenses=AGPL-3.0-only
RUN apk add --no-cache iptables ca-certificates && mkdir -p /data && chmod 700 /data
COPY --from=build /out/link-hub /usr/local/bin/link-hub
VOLUME /data
EXPOSE 8080 7001
HEALTHCHECK --interval=30s --timeout=5s CMD wget -qO- http://127.0.0.1:7001/healthz > /dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/link-hub"]
