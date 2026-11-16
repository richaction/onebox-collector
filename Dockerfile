FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/collector ./cmd/collector

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && \
    adduser -S -D -H -u 10001 collector && \
    mkdir -p /vault /state && chown -R collector /vault /state
COPY --from=build /out/collector /usr/local/bin/collector
USER collector
ENV VAULT_ROOT=/vault DATA_DIR=/state
EXPOSE 8080 8081
ENTRYPOINT ["/usr/local/bin/collector"]
