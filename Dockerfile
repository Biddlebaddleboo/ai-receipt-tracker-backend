FROM golang:1.25-bookworm AS go-api-build

WORKDIR /src/cmd/apiserver

COPY cmd/apiserver/go.mod ./go.mod
COPY cmd/apiserver/go.sum ./go.sum
COPY cmd/apiserver/*.go ./
COPY cmd/apiserver/openapi.json ./openapi.json
COPY cmd/apiserver/ocr_prompt.webp ./ocr_prompt.webp
COPY cmd/apiserver/ocr_prompt_array.webp ./ocr_prompt_array.webp

RUN apt-get update && \
    apt-get install -y --no-install-recommends build-essential && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/* && \
    go mod tidy && \
    go mod download && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /out/apiserver

FROM gcr.io/distroless/static-debian12

WORKDIR /app
COPY --from=go-api-build /out/apiserver /app/apiserver

EXPOSE 8080

CMD ["/app/apiserver"]
