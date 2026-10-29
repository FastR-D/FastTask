package application

import (
	"github.com/FastR-D/FastTask/internal/persistence"
	"testing"
)

func TestRestoreScopePreviewMatchesSameBatchSubtree(t *testing.T) {
	now := persistence.Now()
	root, child := "root", "child"
	tasks := []persistence.Task{{ID: root, Title: "Root", DeletedAt: &now, DeletionID: "D1"}, {ID: child, ParentID: &root, Title: "Child", DeletedAt: &now, DeletionID: "D1"}, {ID: "older", ParentID: &root, Title: "Older", DeletedAt: &now, DeletionID: "D0"}, {ID: "grandchild", ParentID: &child, Title: "Grandchild", DeletedAt: &now, DeletionID: "D1"}}
	scope, err := previewRestoreScope([]map[string]any{{"op": "restore", "target_id": root}, {"op": "restore", "target_id": child}}, tasks)
	if err != nil {
		t.Fatal(err)
	}
	if scope.Count != 3 || len(scope.Tasks) != 3 {
		t.Fatalf("wrong scope %+v", scope)
	}
	for _, item := range scope.Tasks {
		if item["task_id"] == "older" {
			t.Fatal("older batch included")
		}
	}
	if _, err = previewRestoreScope([]map[string]any{{"op": "restore", "target_id": "missing"}}, tasks); err == nil {
		t.Fatal("missing target accepted")
	}
}
