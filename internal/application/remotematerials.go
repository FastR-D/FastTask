package application

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/FastR-D/FastTask/internal/integrationclient"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

type taskMaterialMetadata struct {
	Text         string                            `json:"text"`
	Truncated    bool                              `json:"truncated"`
	Warning      *integrationclient.Diagnostic     `json:"warning"`
	Evidence     *integrationclient.SourceEvidence `json:"evidence,omitempty"`
	DetachedFrom string                            `json:"task_material_detached_from"`
}

func taskMaterialQuery(tx *gorm.DB, userID, taskID string) *gorm.DB {
	return tx.Where("user_id = ? AND kind = 'research_material' AND status != 'rejected' AND json_valid(metadata_json) AND coalesce(json_extract(metadata_json, '$.task_material_detached_from'), '') != ? AND (task_id = ? OR (task_id IS NULL AND json_extract(metadata_json, '$.target_task_id') = ?))", userID, taskID, taskID, taskID)
}

func taskMaterialText(record persistence.ExternalImport) (string, taskMaterialMetadata) {
	var metadata taskMaterialMetadata
	_ = json.Unmarshal([]byte(record.MetadataJSON), &metadata)
	if metadata.Text != "" {
		return metadata.Text, metadata
	}
	return record.Description, metadata
}

func taskMaterialView(record persistence.ExternalImport, text string, metadata taskMaterialMetadata) TaskMaterial {
	title := boundedJSONText(record.Title, 500, 1024)
	address := record.SourceURL
	if len(address) > 2000 || jsonStringBytes(address) > 4096 {
		address = ""
	}
	warning, warningTruncated := boundedIntegrationDiagnostic(metadata.Warning)
	return TaskMaterial{ID: record.ID, System: record.SourceSystem, Title: title, URL: address, Status: record.Status, Revision: record.Revision, Text: text, Truncated: metadata.Truncated, Warning: warning, Evidence: metadata.Evidence, MetadataTruncated: title != record.Title || address != record.SourceURL || warningTruncated}
}

// TaskMaterialContent pages an immutable saved snapshot. SourceTruncated is
// distinct from HasMore: fetching every page cannot recover text the source
// adapter never saved in the first place.
type TaskMaterialContent struct {
	TaskMaterial
	Offset          int  `json:"offset"`
	NextOffset      int  `json:"next_offset"`
	TotalBytes      int  `json:"total_bytes"`
	HasMore         bool `json:"has_more"`
	SourceTruncated bool `json:"source_truncated"`
}

func (s *RemoteToolService) ReadTaskMaterial(ctx context.Context, userID, taskID, materialID string, limit, offset int) (TaskMaterialContent, error) {
	var result TaskMaterialContent
	if limit < 128 || limit > 12000 || offset < 0 || offset > 1<<20 {
		return result, ErrValidation
	}
	err := s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if _, err := RequireLiveTaskTx(tx, userID, taskID); err != nil {
			return err
		}
		var record persistence.ExternalImport
		if err := taskMaterialQuery(tx, userID, taskID).Where("id = ?", materialID).First(&record).Error; err != nil {
			return notFound(err)
		}
		text, metadata := taskMaterialText(record)
		if offset > len(text) || (offset < len(text) && !utf8.RuneStart(text[offset])) {
			return ErrValidation
		}
		chunk := boundedJSONText(text[offset:], limit, 24<<10)
		result.TaskMaterial = taskMaterialView(record, chunk, metadata)
		result.Offset, result.NextOffset, result.TotalBytes = offset, offset+len(chunk), len(text)
		result.HasMore, result.SourceTruncated = result.NextOffset < len(text), metadata.Truncated
		result.Truncated = metadata.Truncated || offset > 0 || result.HasMore
		return nil
	})
	return result, err
}

// DetachTaskMaterial removes only a local task reference. Keeping the original
// status, task ID, decision note and snapshot preserves an earlier conversion's
// evidence, while the association marker prevents it resurfacing in task reads.
func (s *RemoteToolService) DetachTaskMaterial(ctx context.Context, userID, taskID, materialID string, expected int) (TaskMaterial, error) {
	var result TaskMaterial
	err := s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if _, err := RequireLiveTaskTx(tx, userID, taskID); err != nil {
			return err
		}
		var record persistence.ExternalImport
		if err := taskMaterialQuery(tx, userID, taskID).Where("id = ?", materialID).First(&record).Error; err != nil {
			return notFound(err)
		}
		if record.Revision != expected {
			return ErrRevision
		}
		metadata := map[string]json.RawMessage{}
		if err := json.Unmarshal([]byte(record.MetadataJSON), &metadata); err != nil {
			return ErrValidation
		}
		if metadata == nil {
			metadata = map[string]json.RawMessage{}
		}
		now := persistence.Now()
		metadata["task_material_detached_from"], _ = json.Marshal(taskID)
		metadata["task_material_detached_at"], _ = json.Marshal(now)
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return err
		}
		update := tx.Model(&persistence.ExternalImport{}).Where("id = ? AND user_id = ? AND revision = ?", materialID, userID, expected).Updates(map[string]any{"metadata_json": string(encoded), "revision": expected + 1, "updated_at": now})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrRevision
		}
		record.MetadataJSON, record.Revision, record.UpdatedAt = string(encoded), expected+1, now
		text, decoded := taskMaterialText(record)
		result = taskMaterialView(record, boundedJSONText(text, 6000, 8<<10), decoded)
		result.Truncated = result.Truncated || len(result.Text) < len(text)
		return createOutbox(tx, userID, "external_import.material_detached", map[string]any{"import_id": record.ID, "target_task_id": taskID, "preserved_status": record.Status})
	})
	return result, err
}

func materialDetachedFrom(record persistence.ExternalImport, taskID string) bool {
	// Import metadata is an extensible JSON object. Unrelated legacy fields
	// must not prevent recognizing this independently typed association marker.
	var metadata struct {
		DetachedFrom string `json:"task_material_detached_from"`
	}
	return json.Unmarshal([]byte(record.MetadataJSON), &metadata) == nil && metadata.DetachedFrom == taskID
}

// reattachTaskMaterialMetadata only restores the reference explicitly selected
// by an existing-task attachment. The caller must validate the candidate,
// revision and task ownership before committing this metadata in its transaction.
func reattachTaskMaterialMetadata(record persistence.ExternalImport, taskID string, now time.Time) (string, bool, error) {
	if record.Kind != "research_material" || !materialDetachedFrom(record, taskID) {
		return record.MetadataJSON, false, nil
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal([]byte(record.MetadataJSON), &metadata); err != nil {
		return "", false, ErrValidation
	}
	var history []json.RawMessage
	if raw := metadata["task_material_detach_history"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &history); err != nil {
			return "", false, ErrValidation
		}
	}
	entry := map[string]json.RawMessage{}
	entry["task_id"], _ = json.Marshal(taskID)
	entry["reattached_at"], _ = json.Marshal(now)
	if detachedAt, exists := metadata["task_material_detached_at"]; exists {
		entry["detached_at"] = detachedAt
	}
	encodedEntry, err := json.Marshal(entry)
	if err != nil {
		return "", false, err
	}
	history = append(history, encodedEntry)
	metadata["task_material_detach_history"], err = json.Marshal(history)
	if err != nil {
		return "", false, err
	}
	delete(metadata, "task_material_detached_from")
	delete(metadata, "task_material_detached_at")
	encoded, err := json.Marshal(metadata)
	return string(encoded), true, err
}

func jsonStringBytes(value string) int {
	encoded, _ := json.Marshal(value)
	return len(encoded)
}

// boundedJSONText bounds both source bytes and their JSON encoding. Control
// characters and HTML-like text can otherwise multiply a small raw excerpt by
// six when it becomes a tool result. Returned prefixes always end on UTF-8.
func boundedJSONText(value string, rawLimit, encodedLimit int) string {
	value = boundedIntegrationText(value, rawLimit)
	if jsonStringBytes(value) <= encodedLimit {
		return value
	}
	low, high := 0, len(value)
	for low < high {
		mid := low + (high-low+1)/2
		prefix := boundedIntegrationText(value, mid)
		if jsonStringBytes(prefix) <= encodedLimit {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return boundedIntegrationText(value, low)
}

func boundedIntegrationDiagnostic(problem *integrationclient.Diagnostic) (*integrationclient.Diagnostic, bool) {
	if problem == nil {
		return nil, false
	}
	result := *problem
	result.Code = boundedJSONText(strings.TrimSpace(result.Code), 80, 128)
	result.Message = boundedJSONText(result.Message, 500, 768)
	result.Reason = boundedJSONText(result.Reason, 2000, 1024)
	result.Action = boundedJSONText(result.Action, 2000, 1024)
	return &result, result.Code != problem.Code || result.Message != problem.Message || result.Reason != problem.Reason || result.Action != problem.Action
}
