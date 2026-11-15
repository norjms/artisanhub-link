# SPDX-License-Identifier: AGPL-3.0-only
# Link Site for Docker: run with --network host so it can hear printers announce themselves.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS TARGETARCH
ARG OPHQ_APP_COMMIT=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X main.version=$(printf %.7s "$OPHQ_APP_COMMIT")" -o /out/link-site ./cmd/link-site

FROM alpine:3.22
LABEL org.opencontainers.image.source=https://github.com/norjms/artisanhub-link org.opencontainers.image.licenses=AGPL-3.0-only
RUN apk add --no-cache ca-certificates && adduser -D -H -u 10001 link && mkdir -p /data && chown link:link /data && chmod 700 /data
COPY --from=build /out/link-site /usr/local/bin/link-site
USER link
VOLUME /data
ENV LINK_DATA=/data
ENTRYPOINT ["/usr/local/bin/link-site"]
