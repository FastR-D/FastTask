package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/danielgtaylor/huma/v2"
)

// nullableString preserves the difference between an omitted field and an
// explicit JSON null, which PATCH needs to detach a parent or clear a deadline.
type nullableString struct {
	Sent  bool
	Null  bool
	Value string
}

func (value *nullableString) UnmarshalJSON(data []byte) error {
	value.Sent = true
	value.Null = bytes.Equal(bytes.TrimSpace(data), []byte("null"))
	if value.Null {
		value.Value = ""
		return nil
	}
	return json.Unmarshal(data, &value.Value)
}

func (value nullableString) MarshalJSON() ([]byte, error) {
	if value.Null || !value.Sent {
		return []byte("null"), nil
	}
	return json.Marshal(value.Value)
}

func (nullableString) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{Type: huma.TypeString, Nullable: true}
}

// optionalStringList distinguishes PATCH omission from an explicit empty list.
// A dependency list may be cleared with [], but null has no write meaning.
type optionalStringList struct {
	Sent  bool
	Value []string
}

func (value *optionalStringList) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("expected an array of task IDs, not null")
	}
	value.Sent = true
	return json.Unmarshal(data, &value.Value)
}

func (value optionalStringList) MarshalJSON() ([]byte, error) {
	if value.Value == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(value.Value)
}

func (optionalStringList) Schema(huma.Registry) *huma.Schema {
	maxItems := 40
	return &huma.Schema{Type: huma.TypeArray, Items: &huma.Schema{Type: huma.TypeString}, MaxItems: &maxItems}
}
