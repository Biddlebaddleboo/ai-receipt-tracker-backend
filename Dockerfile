FROM golang:1.23-bookworm AS go-api-build

WORKDIR /src/cmd/apiserver

COPY cmd/apiserver/go.mod ./go.mod
COPY cmd/apiserver/go.sum ./go.sum
COPY cmd/apiserver/*.go ./

RUN apt-get update && \
    apt-get install -y --no-install-recommends build-essential && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/* && \
    go mod download && \
    go build -o /out/apiserver

FROM debian:bookworm-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates ffmpeg && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=go-api-build /out/apiserver /app/apiserver

EXPOSE 8080

CMD ["/app/apiserver"]
