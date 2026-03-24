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

	fs "cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
)

const ownerField = "owner_email"

type categoryHandle struct {
	client     *fs.Client
	collection *fs.CollectionRef
}

var (
	categoryHandleMu   sync.Mutex
	categoryHandleSeq  int64
	categoryHandlePool = map[int64]*categoryHandle{}
)

func main() {}

func setError(errOut **C.char, err error) {
	if errOut == nil || err == nil {
		return
	}
	*errOut = C.CString(err.Error())
}

func takeHandle(id int64) (*categoryHandle, error) {
	categoryHandleMu.Lock()
	defer categoryHandleMu.Unlock()
	handle := categoryHandlePool[id]
	if handle == nil {
		return nil, fmt.Errorf("category handle %d not found", id)
	}
	return handle, nil
}

func putHandle(handle *categoryHandle) int64 {
	categoryHandleMu.Lock()
	defer categoryHandleMu.Unlock()
	categoryHandleSeq++
	categoryHandlePool[categoryHandleSeq] = handle
	return categoryHandleSeq
}

func dropHandle(id int64) *categoryHandle {
	categoryHandleMu.Lock()
	defer categoryHandleMu.Unlock()
	handle := categoryHandlePool[id]
	delete(categoryHandlePool, id)
	return handle
}

func decodePayload(payload string) (map[string]interface{}, error) {
	if strings.TrimSpace(payload) == "" {
		return map[string]interface{}{}, nil
	}
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &data); err != nil {
		return nil, err
	}
	return data, nil
}

func marshalJSON(value interface{}) (*C.char, error) {
	bytes, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return C.CString(string(bytes)), nil
}

//export CategoriesNew
func CategoriesNew(collectionName *C.char, databaseID *C.char, errOut **C.char) C.longlong {
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
	return C.longlong(putHandle(&categoryHandle{
		client:     client,
		collection: client.Collection(name),
	}))
}

//export CategoriesClose
func CategoriesClose(handleID C.longlong) {
	handle := dropHandle(int64(handleID))
	if handle == nil || handle.client == nil {
		return
	}
	handle.client.Close()
}

func requireOwnership(data map[string]interface{}, owner string) error {
	if owner == "" {
		return fmt.Errorf("owner_email is required")
	}
	stored, _ := data["owner_email"].(string)
	if stored != owner {
		return fmt.Errorf("category not found")
	}
	return nil
}

//export CategoriesCreate
func CategoriesCreate(
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
	owner := strings.TrimSpace(C.GoString(ownerEmail))
	if owner == "" {
		setError(errOut, fmt.Errorf("owner_email is required"))
		return nil
	}
	if payload["name"] == nil {
		setError(errOut, fmt.Errorf("name is required"))
		return nil
	}
	payload[ownerField] = owner
	payload = map[string]interface{}{
		"name":        strings.TrimSpace(fmt.Sprint(payload["name"])),
		"description": strings.TrimSpace(fmt.Sprint(payload["description"])),
		ownerField:    owner,
	}
	doc := handle.collection.NewDoc()
	if _, err := doc.Set(context.Background(), payload); err != nil {
		setError(errOut, err)
		return nil
	}
	return C.CString(doc.ID)
}

//export CategoriesList
func CategoriesList(
	handleID C.longlong,
	ownerEmail *C.char,
	errOut **C.char,
) *C.char {
	handle, err := takeHandle(int64(handleID))
	if err != nil {
		setError(errOut, err)
		return nil
	}
	owner := strings.TrimSpace(C.GoString(ownerEmail))
	if owner == "" {
		setError(errOut, fmt.Errorf("owner_email is required"))
		return nil
	}
	iter := handle.collection.Where(ownerField, "==", owner).Documents(context.Background())
	defer iter.Stop()
	var categories []map[string]interface{}
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
		if err := requireOwnership(data, owner); err != nil {
			continue
		}
		categories = append(categories, map[string]interface{}{
			"id":          snapshot.Ref.ID,
			"name":        data["name"],
			"description": data["description"],
		})
	}
	result, err := marshalJSON(categories)
	if err != nil {
		setError(errOut, err)
		return nil
	}
	return result
}

func snapshotToMap(snapshot *fs.DocumentSnapshot) map[string]interface{} {
	data := snapshot.Data()
	result := map[string]interface{}{
		"id":          snapshot.Ref.ID,
		"name":        data["name"],
		"description": data["description"],
	}
	return result
}

//export CategoriesGet
func CategoriesGet(
	handleID C.longlong,
	ownerEmail *C.char,
	categoryID *C.char,
	errOut **C.char,
) *C.char {
	handle, err := takeHandle(int64(handleID))
	if err != nil {
		setError(errOut, err)
		return nil
	}
	id := strings.TrimSpace(C.GoString(categoryID))
	if id == "" {
		setError(errOut, fmt.Errorf("category_id is required"))
		return nil
	}
	snapshot, err := handle.collection.Doc(id).Get(context.Background())
	if err != nil || !snapshot.Exists() {
		setError(errOut, fmt.Errorf("category not found"))
		return nil
	}
	if err := requireOwnership(snapshot.Data(), strings.TrimSpace(C.GoString(ownerEmail))); err != nil {
		setError(errOut, err)
		return nil
	}
	result, err := marshalJSON(snapshotToMap(snapshot))
	if err != nil {
		setError(errOut, err)
		return nil
	}
	return result
}

//export CategoriesUpdate
func CategoriesUpdate(
	handleID C.longlong,
	ownerEmail *C.char,
	categoryID *C.char,
	payloadJSON *C.char,
	errOut **C.char,
) *C.char {
	handle, err := takeHandle(int64(handleID))
	if err != nil {
		setError(errOut, err)
		return nil
	}
	id := strings.TrimSpace(C.GoString(categoryID))
	if id == "" {
		setError(errOut, fmt.Errorf("category_id is required"))
		return nil
	}
	snapshot, err := handle.collection.Doc(id).Get(context.Background())
	if err != nil || !snapshot.Exists() {
		setError(errOut, fmt.Errorf("category not found"))
		return nil
	}
	if err := requireOwnership(snapshot.Data(), strings.TrimSpace(C.GoString(ownerEmail))); err != nil {
		setError(errOut, err)
		return nil
	}
	payload, err := decodePayload(C.GoString(payloadJSON))
	if err != nil {
		setError(errOut, err)
		return nil
	}
	update := map[string]interface{}{}
	if name, ok := payload["name"]; ok {
		update["name"] = strings.TrimSpace(fmt.Sprint(name))
	}
	if description, ok := payload["description"]; ok {
		update["description"] = strings.TrimSpace(fmt.Sprint(description))
	}
	if len(update) == 0 {
		update = nil
	}
	if update != nil {
		if _, err := handle.collection.Doc(id).Set(context.Background(), update, fs.MergeAll); err != nil {
			setError(errOut, err)
			return nil
		}
	}
	updated, err := handle.collection.Doc(id).Get(context.Background())
	if err != nil {
		setError(errOut, err)
		return nil
	}
	result, err := marshalJSON(snapshotToMap(updated))
	if err != nil {
		setError(errOut, err)
		return nil
	}
	return result
}

//export CategoriesDelete
func CategoriesDelete(
	handleID C.longlong,
	ownerEmail *C.char,
	categoryID *C.char,
	errOut **C.char,
) *C.char {
	handle, err := takeHandle(int64(handleID))
	if err != nil {
		setError(errOut, err)
		return nil
	}
	id := strings.TrimSpace(C.GoString(categoryID))
	if id == "" {
		setError(errOut, fmt.Errorf("category_id is required"))
		return nil
	}
	snapshot, err := handle.collection.Doc(id).Get(context.Background())
	if err != nil || !snapshot.Exists() {
		setError(errOut, fmt.Errorf("category not found"))
		return nil
	}
	if err := requireOwnership(snapshot.Data(), strings.TrimSpace(C.GoString(ownerEmail))); err != nil {
		setError(errOut, err)
		return nil
	}
	if _, err := handle.collection.Doc(id).Delete(context.Background()); err != nil {
		setError(errOut, err)
		return nil
	}
	result, err := marshalJSON(snapshotToMap(snapshot))
	if err != nil {
		setError(errOut, err)
		return nil
	}
	return result
}
