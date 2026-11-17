FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/usdt-scanner ./cmd/usdt-scanner \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/usdt-deploy ./cmd/usdt-deploy

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 scanner
COPY --from=build /out/ /usr/local/bin/
USER scanner
VOLUME /data
ENV DATA_DIR=/data LISTEN_ADDR=0.0.0.0:9180
HEALTHCHECK --interval=60s --timeout=5s CMD wget -qO- http://127.0.0.1:9180/ || exit 1
ENTRYPOINT ["usdt-scanner"]
