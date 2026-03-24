FROM golang:1.23-bookworm AS go-api-build

WORKDIR /src/cmd/apiserver

COPY cmd/apiserver/go.mod ./go.mod
COPY cmd/apiserver/go.sum ./go.sum
COPY cmd/apiserver/main.go ./main.go
COPY cmd/apiserver/users.go ./users.go
COPY cmd/apiserver/receipts.go ./receipts.go

RUN apt-get update && \
    apt-get install -y --no-install-recommends build-essential && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/* && \
    go mod download && \
    go build -o /out/apiserver

FROM python:3.11-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

WORKDIR /app

COPY requirements.txt ./requirements.txt

RUN pip install --upgrade pip setuptools wheel && \
    pip install --no-cache-dir -r requirements.txt

COPY app ./app
COPY --from=go-api-build /out/apiserver /app/apiserver

EXPOSE 8080

CMD ["/app/apiserver"]
