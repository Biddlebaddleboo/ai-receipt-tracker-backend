package main

/*
#include <stdint.h>
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unsafe"

	fs "cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
)

const (
	ownerField = "owner_email"
	dateMarker = "__firestorebridge_datetime__"
)

type firestoreHandle struct {
	client     *fs.Client
	collection *fs.CollectionRef
}

var (
	firestoreHandleMu   sync.Mutex
	firestoreHandleSeq  int64
	firestoreHandlePool = map[int64]*firestoreHandle{}
)

func main() {}

func setError(errOut **C.char, err error) {
	if errOut == nil || err == nil {
		return
	}
	*errOut = C.CString(err.Error())
}

func takeHandle(id int64) (*firestoreHandle, error) {
	firestoreHandleMu.Lock()
	defer firestoreHandleMu.Unlock()
	handle := firestoreHandlePool[id]
	if handle == nil {
		return nil, fmt.Errorf("firestore handle %d not found", id)
	}
	return handle, nil
}

func putHandle(handle *firestoreHandle) int64 {
	firestoreHandleMu.Lock()
	defer firestoreHandleMu.Unlock()
	firestoreHandleSeq++
	firestoreHandlePool[firestoreHandleSeq] = handle
	return firestoreHandleSeq
}

func dropHandle(id int64) *firestoreHandle {
	firestoreHandleMu.Lock()
	defer firestoreHandleMu.Unlock()
	handle := firestoreHandlePool[id]
	delete(firestoreHandlePool, id)
	return handle
}

func parseDateTime(text string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func decodeBridgeValue(value interface{}) (interface{}, error) {
	switch typed := value.(type) {
	case map[string]interface{}:
		if len(typed) == 1 {
			if raw, ok := typed[dateMarker]; ok {
				text, ok := raw.(string)
				if !ok {
					return nil, fmt.Errorf("invalid datetime marker payload")
				}
				return parseDateTime(text)
			}
		}
		decoded := make(map[string]interface{}, len(typed))
		for key, item := range typed {
			converted, err := decodeBridgeValue(item)
			if err != nil {
				return nil, err
			}
			decoded[key] = converted
		}
		return decoded, nil
	case []interface{}:
		decoded := make([]interface{}, len(typed))
		for index, item := range typed {
			converted, err := decodeBridgeValue(item)
			if err != nil {
				return nil, err
			}
			decoded[index] = converted
		}
		return decoded, nil
	default:
		return value, nil
	}
}

func decodePayload(raw string) (map[string]interface{}, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]interface{}{}, nil
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, err
	}
	converted, err := decodeBridgeValue(payload)
	if err != nil {
		return nil, err
	}
	asMap, ok := converted.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("payload must decode to an object")
	}
	return asMap, nil
}

func encodeBridgeValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case time.Time:
		return typed.UTC().Format(time.RFC3339Nano)
	case map[string]interface{}:
		encoded := make(map[string]interface{}, len(typed))
		for key, item := range typed {
			encoded[key] = encodeBridgeValue(item)
		}
		return encoded
	case []interface{}:
		encoded := make([]interface{}, len(typed))
		for index, item := range typed {
			encoded[index] = encodeBridgeValue(item)
		}
		return encoded
	default:
		return value
	}
}

func marshalJSONString(value interface{}) (*C.char, error) {
	payload, err := json.Marshal(encodeBridgeValue(value))
	if err != nil {
		return nil, err
	}
	return C.CString(string(payload)), nil
}

func notFoundError(receiptID string) error {
	return fmt.Errorf("Receipt %s not found", receiptID)
}

func ensureOwner(data map[string]interface{}, ownerEmail string, receiptID string) error {
	storedOwner, _ := data[ownerField].(string)
	if storedOwner != ownerEmail {
		return notFoundError(receiptID)
	}
	return nil
}

//export FirestoreNew
func FirestoreNew(collectionName *C.char, databaseID *C.char, errOut **C.char) C.longlong {
	name := strings.TrimSpace(C.GoString(collectionName))
	if name == "" {
		setError(errOut, fmt.Errorf("collection_name is required"))
		return 0
	}
	database := strings.TrimSpace(C.GoString(databaseID))
	if database == "" {
		database = "(default)"
	}
	ctx := context.Background()
	client, err := fs.NewClientWithDatabase(ctx, fs.DetectProjectID, database)
	if err != nil {
		setError(errOut, err)
		return 0
	}
	handle := &firestoreHandle{
		client:     client,
		collection: client.Collection(name),
	}
	return C.longlong(putHandle(handle))
}

//export FirestoreClose
func FirestoreClose(handleID C.longlong) {
	handle := dropHandle(int64(handleID))
	if handle == nil || handle.client == nil {
		return
	}
	_ = handle.client.Close()
}

//export FirestoreInsertReceipt
func FirestoreInsertReceipt(
	handleID C.longlong,
	ownerEmail *C.char,
	payloadJSON *C.char,
	errOut **C.char,
) *C.char {
	handle, err := takeHandle(int64(handleID))
	if err != nil {
		setError(errOut, err)
		return nil
	}
	payload, err := decodePayload(C.GoString(payloadJSON))
	if err != nil {
		setError(errOut, err)
		return nil
	}
	payload[ownerField] = strings.TrimSpace(C.GoString(ownerEmail))
	docRef := handle.collection.NewDoc()
	if _, err := docRef.Set(context.Background(), payload); err != nil {
		setError(errOut, err)
		return nil
	}
	return C.CString(docRef.ID)
}

//export FirestoreGetReceipt
func FirestoreGetReceipt(
	handleID C.longlong,
	receiptID *C.char,
	ownerEmail *C.char,
	errOut **C.char,
) *C.char {
	handle, err := takeHandle(int64(handleID))
	if err != nil {
		setError(errOut, err)
		return nil
	}
	id := strings.TrimSpace(C.GoString(receiptID))
	snapshot, err := handle.collection.Doc(id).Get(context.Background())
	if err != nil {
		setError(errOut, notFoundError(id))
		return nil
	}
	if !snapshot.Exists() {
		setError(errOut, notFoundError(id))
		return nil
	}
	data := snapshot.Data()
	if err := ensureOwner(data, strings.TrimSpace(C.GoString(ownerEmail)), id); err != nil {
		setError(errOut, err)
		return nil
	}
	result, err := marshalJSONString(data)
	if err != nil {
		setError(errOut, err)
		return nil
	}
	return result
}

//export FirestoreDeleteReceipt
func FirestoreDeleteReceipt(
	handleID C.longlong,
	receiptID *C.char,
	ownerEmail *C.char,
	errOut **C.char,
) *C.char {
	handle, err := takeHandle(int64(handleID))
	if err != nil {
		setError(errOut, err)
		return nil
	}
	id := strings.TrimSpace(C.GoString(receiptID))
	snapshot, err := handle.collection.Doc(id).Get(context.Background())
	if err != nil {
		setError(errOut, notFoundError(id))
		return nil
	}
	if !snapshot.Exists() {
		setError(errOut, notFoundError(id))
		return nil
	}
	data := snapshot.Data()
	if err := ensureOwner(data, strings.TrimSpace(C.GoString(ownerEmail)), id); err != nil {
		setError(errOut, err)
		return nil
	}
	if _, err := handle.collection.Doc(id).Delete(context.Background()); err != nil {
		setError(errOut, err)
		return nil
	}
	result, err := marshalJSONString(data)
	if err != nil {
		setError(errOut, err)
		return nil
	}
	return result
}

//export FirestoreUpdateReceipt
func FirestoreUpdateReceipt(
	handleID C.longlong,
	receiptID *C.char,
	ownerEmail *C.char,
	payloadJSON *C.char,
	errOut **C.char,
) *C.char {
	handle, err := takeHandle(int64(handleID))
	if err != nil {
		setError(errOut, err)
		return nil
	}
	id := strings.TrimSpace(C.GoString(receiptID))
	snapshot, err := handle.collection.Doc(id).Get(context.Background())
	if err != nil {
		setError(errOut, notFoundError(id))
		return nil
	}
	if !snapshot.Exists() {
		setError(errOut, notFoundError(id))
		return nil
	}
	data := snapshot.Data()
	if err := ensureOwner(data, strings.TrimSpace(C.GoString(ownerEmail)), id); err != nil {
		setError(errOut, err)
		return nil
	}
	payload, err := decodePayload(C.GoString(payloadJSON))
	if err != nil {
		setError(errOut, err)
		return nil
	}
	delete(payload, ownerField)
	if len(payload) > 0 {
		if _, err := handle.collection.Doc(id).Set(context.Background(), payload, fs.MergeAll); err != nil {
			setError(errOut, err)
			return nil
		}
	}
	updatedSnapshot, err := handle.collection.Doc(id).Get(context.Background())
	if err != nil {
		setError(errOut, notFoundError(id))
		return nil
	}
	result, err := marshalJSONString(updatedSnapshot.Data())
	if err != nil {
		setError(errOut, err)
		return nil
	}
	return result
}

//export FirestoreListReceipts
func FirestoreListReceipts(
	handleID C.longlong,
	ownerEmail *C.char,
	limit C.longlong,
	startAfterID *C.char,
	errOut **C.char,
) *C.char {
	handle, err := takeHandle(int64(handleID))
	if err != nil {
		setError(errOut, err)
		return nil
	}
	owner := strings.TrimSpace(C.GoString(ownerEmail))
	query := handle.collection.Where(ownerField, "==", owner).OrderBy("created_at", fs.Desc).Limit(int(limit))
	afterID := strings.TrimSpace(C.GoString(startAfterID))
	if afterID != "" {
		afterSnapshot, err := handle.collection.Doc(afterID).Get(context.Background())
		if err != nil {
			setError(errOut, notFoundError(afterID))
			return nil
		}
		if !afterSnapshot.Exists() {
			setError(errOut, notFoundError(afterID))
			return nil
		}
		afterData := afterSnapshot.Data()
		if err := ensureOwner(afterData, owner, afterID); err != nil {
			setError(errOut, err)
			return nil
		}
		createdAt, ok := afterData["created_at"]
		if !ok {
			setError(errOut, notFoundError(afterID))
			return nil
		}
		query = query.StartAfter(createdAt)
	}

	iter := query.Documents(context.Background())
	defer iter.Stop()

	docs := make([]map[string]interface{}, 0)
	for {
		snapshot, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			setError(errOut, err)
			return nil
		}
		data := snapshot.Data()
		doc := make(map[string]interface{}, len(data)+1)
		doc["id"] = snapshot.Ref.ID
		for key, value := range data {
			doc[key] = value
		}
		docs = append(docs, doc)
	}

	var nextCursor interface{}
	if len(docs) > 0 {
		nextCursor = docs[len(docs)-1]["id"]
	}
	result := map[string]interface{}{
		"docs":        docs,
		"next_cursor": nextCursor,
	}
	payload, err := marshalJSONString(result)
	if err != nil {
		setError(errOut, err)
		return nil
	}
	return payload
}

//export FirestoreCountReceiptsByOwner
func FirestoreCountReceiptsByOwner(
	handleID C.longlong,
	ownerEmail *C.char,
	startISO *C.char,
	endISO *C.char,
	errOut **C.char,
) C.longlong {
	handle, err := takeHandle(int64(handleID))
	if err != nil {
		setError(errOut, err)
		return 0
	}
	owner := strings.TrimSpace(C.GoString(ownerEmail))
	query := handle.collection.Where(ownerField, "==", owner)

	startText := strings.TrimSpace(C.GoString(startISO))
	if startText != "" {
		start, err := parseDateTime(startText)
		if err != nil {
			setError(errOut, err)
			return 0
		}
		query = query.Where("created_at", ">=", start)
	}

	endText := strings.TrimSpace(C.GoString(endISO))
	if endText != "" {
		end, err := parseDateTime(endText)
		if err != nil {
			setError(errOut, err)
			return 0
		}
		query = query.Where("created_at", "<", end)
	}

	iter := query.Documents(context.Background())
	defer iter.Stop()

	var count int64
	for {
		_, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			setError(errOut, err)
			return 0
		}
		count++
	}
	return C.longlong(count)
}

//export FirestoreFree
func FirestoreFree(ptr unsafe.Pointer) {
	if ptr != nil {
		C.free(ptr)
	}
}
