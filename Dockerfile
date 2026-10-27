# xcod image with ffmpeg (only needed for qualities other than "Original").
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY deploy ./deploy
COPY internal ./internal
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/xcod ./cmd/xcod

FROM alpine:3.22
RUN apk add --no-cache ffmpeg tzdata ca-certificates \
 && adduser -D -H -u 10001 xcod \
 && mkdir /data && chown xcod:xcod /data && chmod 700 /data
COPY --from=build /out/xcod /usr/local/bin/xcod
USER xcod
# config.json, admin.json and stats.json live in the volume (created on the first run)
VOLUME /data
EXPOSE 8090
ENV XCOD_DATA=/data XCOD_LISTEN=:8090
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD wget -qO /dev/null http://127.0.0.1:8090/healthz || exit 1
ENTRYPOINT ["xcod"]
