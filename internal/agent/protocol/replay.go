package protocol

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// ApplyStateJSON folds exactly one committed protocol event. Snapshots and the
// replay cursor are advanced by the persistence layer in the same transaction.
func ApplyStateJSON(snapshot, event string) (string, error) {
	var root any
	if snapshot == "" || snapshot == "{}" {
		snapshot = `{"messages":[],"isRunning":false,"fasttask":{}}`
	}
	if err := json.Unmarshal([]byte(snapshot), &root); err != nil {
		return "", err
	}
	var chunk Chunk
	if err := json.Unmarshal([]byte(event), &chunk); err != nil {
		return "", err
	}
	if err := chunk.Validate(); err != nil {
		return "", err
	}
	if chunk.Type == ChunkUpdateState {
		for _, op := range chunk.Operations {
			var err error
			root, err = apply(root, op.Path, op)
			if err != nil {
				return "", err
			}
		}
	}
	b, err := json.Marshal(root)
	return string(b), err
}
func apply(node any, path []string, op Operation) (any, error) {
	if len(path) == 0 {
		if op.Type == OpSet {
			return op.Value, nil
		}
		a, ok := node.(string)
		b, valid := op.Value.(string)
		if !ok || !valid {
			return nil, fmt.Errorf("append-text target is not a string")
		}
		return a + b, nil
	}
	if err := validatePath(path); err != nil {
		return nil, err
	}
	if node == nil {
		if _, err := strconv.Atoi(path[0]); err == nil {
			node = []any{}
		} else {
			node = map[string]any{}
		}
	}
	switch v := node.(type) {
	case map[string]any:
		next, err := apply(v[path[0]], path[1:], op)
		if err != nil {
			return nil, err
		}
		v[path[0]] = next
		return v, nil
	case []any:
		i, err := strconv.Atoi(path[0])
		if err != nil || i < 0 || i > 100000 {
			return nil, fmt.Errorf("invalid array index")
		}
		for len(v) <= i {
			v = append(v, nil)
		}
		next, err := apply(v[i], path[1:], op)
		if err != nil {
			return nil, err
		}
		v[i] = next
		return v, nil
	default:
		return nil, fmt.Errorf("invalid state path")
	}
}
