FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/docproc ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/loadgen ./cmd/loadgen

FROM alpine:3.22

RUN apk add --no-cache ca-certificates \
 && addgroup -S app \
 && adduser -S -G app app

COPY --from=build /out/docproc /usr/local/bin/docproc
COPY --from=build /out/loadgen /usr/local/bin/loadgen

USER app
EXPOSE 50051 9090

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=5 \
  CMD wget -qO- http://127.0.0.1:9090/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["docproc"]
