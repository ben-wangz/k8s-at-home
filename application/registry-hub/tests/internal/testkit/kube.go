package testkit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

type Object struct {
	Kind      string
	Type      string
	Immutable bool
	Name      string
	UID       string
	Labels    map[string]string
	Metadata  map[string]any
	Data      map[string]any
	Raw       map[string]any
}

func KubectlList(ctx context.Context, namespace string, args ...string) ([]Object, error) {
	commandArgs := append([]string{"-n", namespace}, args...)
	commandArgs = append(commandArgs, "-o", "json")
	result, err := Command(ctx, "kubectl", commandArgs...)
	if err != nil {
		return nil, err
	}
	var collection struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(result.Stdout, &collection); err != nil {
		return nil, fmt.Errorf("decode kubectl resource list: %w", err)
	}
	objects := make([]Object, 0, len(collection.Items))
	for _, item := range collection.Items {
		objects = append(objects, objectFromMap(item))
	}
	return objects, nil
}

func KubectlGetObject(ctx context.Context, namespace, resource, name string) (Object, error) {
	item, err := KubectlGet(ctx, namespace, resource, name)
	if err != nil {
		return Object{}, err
	}
	return objectFromMap(item), nil
}

func objectFromMap(item map[string]any) Object {
	metadata := Map(item["metadata"])
	return Object{
		Kind:      String(item["kind"]),
		Type:      String(item["type"]),
		Immutable: boolValue(item["immutable"]),
		Name:      String(metadata["name"]),
		UID:       String(metadata["uid"]),
		Labels:    stringMap(metadata["labels"]),
		Metadata:  metadata,
		Data:      Map(item["data"]),
		Raw:       item,
	}
}

func boolValue(value any) bool {
	result, _ := value.(bool)
	return result
}

func DecodeSecretData(object Object) (map[string][]byte, error) {
	encoded := make(map[string]any, len(object.Data))
	for key, value := range object.Data {
		encoded[key] = value
	}
	data := make(map[string][]byte, len(encoded))
	for key, value := range encoded {
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("Secret %s has a non-string data value", object.Name)
		}
		decoded, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return nil, fmt.Errorf("Secret %s key %s is not valid base64", object.Name, key)
		}
		data[key] = decoded
	}
	return data, nil
}

func stringMap(value any) map[string]string {
	result := make(map[string]string)
	for key, item := range Map(value) {
		if text, ok := item.(string); ok {
			result[key] = text
		}
	}
	return result
}
