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
COPY native/firestorebridge/firestorebridge.go ./

RUN apt-get update && \
    apt-get install -y --no-install-recommends build-essential && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/* && \
    go mod download && \
    go build -buildmode=c-shared -o /out/libfirestorebridge.so

FROM python:3.11-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PATH="/root/.local/bin:$PATH" \
    GO_STORAGE_LIBRARY_PATH=/app/native/libstoragebridge.so \
    GO_FIRESTORE_LIBRARY_PATH=/app/native/libfirestorebridge.so

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

EXPOSE 8080

CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8080"]
