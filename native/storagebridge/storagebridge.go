package main

/*
#include <stdint.h>
#include <stdlib.h>
*/
import "C"

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"unsafe"

	gcs "cloud.google.com/go/storage"
)

type storageHandle struct {
	client *gcs.Client
	bucket *gcs.BucketHandle
	name   string
}

var (
	handleMu   sync.Mutex
	handleSeq  int64
	handlePool = map[int64]*storageHandle{}
)

func main() {}

func setError(errOut **C.char, err error) {
	if errOut == nil || err == nil {
		return
	}
	*errOut = C.CString(err.Error())
}

func takeHandle(id int64) (*storageHandle, error) {
	handleMu.Lock()
	defer handleMu.Unlock()
	handle := handlePool[id]
	if handle == nil {
		return nil, fmt.Errorf("storage handle %d not found", id)
	}
	return handle, nil
}

func putHandle(handle *storageHandle) int64 {
	handleMu.Lock()
	defer handleMu.Unlock()
	handleSeq++
	handlePool[handleSeq] = handle
	return handleSeq
}

func dropHandle(id int64) *storageHandle {
	handleMu.Lock()
	defer handleMu.Unlock()
	handle := handlePool[id]
	delete(handlePool, id)
	return handle
}

func objectPublicURL(bucketName string, objectPath string) string {
	escapedBucket := url.PathEscape(bucketName)
	escapedObject := strings.ReplaceAll(url.PathEscape(objectPath), "%2F", "/")
	return fmt.Sprintf("https://storage.googleapis.com/%s/%s", escapedBucket, escapedObject)
}

//export StorageNew
func StorageNew(bucketName *C.char, errOut **C.char) C.longlong {
	name := strings.TrimSpace(C.GoString(bucketName))
	if name == "" {
		setError(errOut, fmt.Errorf("bucket_name is required"))
		return 0
	}
	client, err := gcs.NewClient(context.Background())
	if err != nil {
		setError(errOut, err)
		return 0
	}
	handle := &storageHandle{
		client: client,
		bucket: client.Bucket(name),
		name:   name,
	}
	return C.longlong(putHandle(handle))
}

//export StorageClose
func StorageClose(handleID C.longlong) {
	handle := dropHandle(int64(handleID))
	if handle == nil || handle.client == nil {
		return
	}
	_ = handle.client.Close()
}

//export StorageUpload
func StorageUpload(
	handleID C.longlong,
	dataPtr *C.uchar,
	dataLen C.longlong,
	destinationPath *C.char,
	contentType *C.char,
	errOut **C.char,
) *C.char {
	handle, err := takeHandle(int64(handleID))
	if err != nil {
		setError(errOut, err)
		return nil
	}
	path := strings.TrimSpace(C.GoString(destinationPath))
	if path == "" {
		setError(errOut, fmt.Errorf("destination_path is required"))
		return nil
	}
	size := int64(dataLen)
	var payload []byte
	if size > 0 {
		payload = unsafe.Slice((*byte)(unsafe.Pointer(dataPtr)), int(size))
	} else {
		payload = []byte{}
	}
	writer := handle.bucket.Object(path).NewWriter(context.Background())
	mimeType := strings.TrimSpace(C.GoString(contentType))
	if mimeType != "" {
		writer.ContentType = mimeType
	}
	if _, err := writer.ReadFrom(bytes.NewReader(payload)); err != nil {
		_ = writer.Close()
		setError(errOut, err)
		return nil
	}
	if err := writer.Close(); err != nil {
		setError(errOut, err)
		return nil
	}
	return C.CString(objectPublicURL(handle.name, path))
}

//export StorageDownload
func StorageDownload(
	handleID C.longlong,
	destinationPath *C.char,
	contentTypeOut **C.char,
	dataLenOut *C.longlong,
	errOut **C.char,
) *C.uchar {
	handle, err := takeHandle(int64(handleID))
	if err != nil {
		setError(errOut, err)
		return nil
	}
	path := strings.TrimSpace(C.GoString(destinationPath))
	if path == "" {
		setError(errOut, fmt.Errorf("destination_path is required"))
		return nil
	}
	reader, err := handle.bucket.Object(path).NewReader(context.Background())
	if err != nil {
		setError(errOut, err)
		return nil
	}
	defer reader.Close()
	buffer := new(bytes.Buffer)
	if _, err := buffer.ReadFrom(reader); err != nil {
		setError(errOut, err)
		return nil
	}
	if contentTypeOut != nil {
		*contentTypeOut = C.CString(reader.Attrs.ContentType)
	}
	if dataLenOut != nil {
		*dataLenOut = C.longlong(buffer.Len())
	}
	if buffer.Len() == 0 {
		return nil
	}
	return (*C.uchar)(C.CBytes(buffer.Bytes()))
}

//export StorageDelete
func StorageDelete(
	handleID C.longlong,
	destinationPath *C.char,
	errOut **C.char,
) C.int {
	handle, err := takeHandle(int64(handleID))
	if err != nil {
		setError(errOut, err)
		return 0
	}
	path := strings.TrimSpace(C.GoString(destinationPath))
	if path == "" {
		setError(errOut, fmt.Errorf("destination_path is required"))
		return 0
	}
	err = handle.bucket.Object(path).Delete(context.Background())
	if err != nil && !errors.Is(err, gcs.ErrObjectNotExist) {
		setError(errOut, err)
		return 0
	}
	return 1
}

//export StorageFree
func StorageFree(ptr unsafe.Pointer) {
	if ptr != nil {
		C.free(ptr)
	}
}
