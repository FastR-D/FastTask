package application

import "strings"

// Task dependencies are explicit IDs, never inferred from titles or tree order.
// Empty arrays clear them; null and non-string entries are rejected.
func taskDependencyList(raw any) ([]string, error) {
	var values []string
	switch v := raw.(type) {
	case []string:
		values = v
	case []any:
		for _, entry := range v {
			id, ok := entry.(string)
			if !ok {
				return nil, ErrValidation
			}
			values = append(values, id)
		}
	default:
		return nil, ErrValidation
	}
	if len(values) > 40 {
		return nil, ErrValidation
	}
	result := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		id := strings.TrimSpace(value)
		if id == "" || len(id) > 200 {
			return nil, ErrValidation
		}
		if !seen[id] {
			result = append(result, id)
			seen[id] = true
		}
	}
	return result, nil
}

func taskDependenciesFromPatch(patch map[string]any, refs map[string]string) ([]string, bool, error) {
	result := []string{}
	changed := false
	if raw, exists := patch["depends_on"]; exists {
		ids, err := taskDependencyList(raw)
		if err != nil {
			return nil, false, err
		}
		result, changed = ids, true
	}
	if raw, exists := patch["depends_on_refs"]; exists {
		names, err := taskDependencyList(raw)
		if err != nil {
			return nil, false, err
		}
		for _, name := range names {
			id, exists := refs[name]
			if !exists {
				return nil, false, ErrValidation
			}
			result = append(result, id)
		}
		changed = true
	}
	if !changed {
		return result, false, nil
	}
	result, err := taskDependencyList(result)
	return result, true, err
}
