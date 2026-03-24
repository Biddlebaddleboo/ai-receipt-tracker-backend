FROM golang:1.22-bookworm AS go-storage-build

WORKDIR /src/native/storagebridge

COPY native/storagebridge/go.mod ./
COPY native/storagebridge/go.sum ./
COPY native/storagebridge/storagebridge.go ./

RUN apt-get update && \
    apt-get install -y --no-install-recommends build-essential && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/* && \
    go mod download && \
    go build -buildmode=c-shared -o /out/libstoragebridge.so

FROM golang:1.22-bookworm AS go-firestore-build

WORKDIR /src/native/firestorebridge

COPY native/firestorebridge/go.mod ./
COPY native/firestorebridge/go.sum ./
COPY native/firestorebridge/firestorebridge.go ./

RUN apt-get update && \
    apt-get install -y --no-install-recommends build-essential && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/* && \
    go mod download && \
    go build -buildmode=c-shared -o /out/libfirestorebridge.so

FROM golang:1.22-bookworm AS go-categories-build

WORKDIR /src/native/categoriesbridge

COPY native/categoriesbridge/go.mod ./
COPY native/categoriesbridge/go.sum ./
COPY native/categoriesbridge/categoriesbridge.go ./

RUN apt-get update && \
    apt-get install -y --no-install-recommends build-essential && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/* && \
    go mod download && \
    go build -buildmode=c-shared -o /out/libcategoriesbridge.so

FROM golang:1.22-bookworm AS go-ocr-build

WORKDIR /src/native/ocrbridge

COPY native/ocrbridge/go.mod ./
COPY native/ocrbridge/ocrbridge.go ./

RUN apt-get update && \
    apt-get install -y --no-install-recommends build-essential && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/* && \
    go build -buildmode=c-shared -o /out/libocrbridge.so

FROM golang:1.22-bookworm AS go-auth-build

WORKDIR /src/native/authbridge

COPY native/authbridge/go.mod ./
COPY native/authbridge/go.sum ./
COPY native/authbridge/authbridge.go ./

RUN apt-get update && \
    apt-get install -y --no-install-recommends build-essential && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/* && \
    go mod download && \
    go build -buildmode=c-shared -o /out/libauthbridge.so

FROM golang:1.22-bookworm AS go-api-build

WORKDIR /src/cmd/apiserver

COPY cmd/apiserver/go.mod ./
COPY cmd/apiserver/go.sum ./
COPY cmd/apiserver/main.go ./

RUN apt-get update && \
    apt-get install -y --no-install-recommends build-essential && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/* && \
    go mod download && \
    go build -o /out/apiserver

FROM python:3.11-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PATH="/root/.local/bin:$PATH" \
    GO_STORAGE_LIBRARY_PATH=/app/native/libstoragebridge.so \
    GO_FIRESTORE_LIBRARY_PATH=/app/native/libfirestorebridge.so \
    GO_CATEGORIES_LIBRARY_PATH=/app/native/libcategoriesbridge.so \
    GO_OCR_LIBRARY_PATH=/app/native/libocrbridge.so \
    GO_AUTH_LIBRARY_PATH=/app/native/libauthbridge.so

WORKDIR /app

RUN apt-get update && \
    apt-get install -y --no-install-recommends build-essential && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

COPY requirements.txt .
RUN pip install --upgrade pip setuptools wheel && \
    pip install --no-cache-dir -r requirements.txt

COPY . .
COPY --from=go-storage-build /out/libstoragebridge.so /app/native/libstoragebridge.so
COPY --from=go-firestore-build /out/libfirestorebridge.so /app/native/libfirestorebridge.so
COPY --from=go-categories-build /out/libcategoriesbridge.so /app/native/libcategoriesbridge.so
COPY --from=go-ocr-build /out/libocrbridge.so /app/native/libocrbridge.so
COPY --from=go-auth-build /out/libauthbridge.so /app/native/libauthbridge.so
COPY --from=go-api-build /out/apiserver /app/apiserver

EXPOSE 8080

CMD ["/app/apiserver"]
