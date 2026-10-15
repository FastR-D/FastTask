package httpapi

import (
	"context"
	"net/http"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/integrationclient"
	"github.com/danielgtaylor/huma/v2"
)

type integrationContentRoutes struct{ RouteDeps }

func NewIntegrationContentRoutes(d RouteDeps) RouteRegistrar { return integrationContentRoutes{d} }

type integrationDiagnosticBody struct {
	OK         bool                         `json:"ok"`
	Diagnostic integrationclient.Diagnostic `json:"diagnostic"`
}
type integrationResourcesBody struct {
	OK bool `json:"ok"`
	integrationclient.Page
	Diagnostic *integrationclient.Diagnostic `json:"diagnostic,omitempty"`
}
type integrationContentBody struct {
	OK bool `json:"ok"`
	integrationclient.Content
	Diagnostic *integrationclient.Diagnostic `json:"diagnostic,omitempty"`
}

func (s integrationContentRoutes) RegisterRoutes(api huma.API) {
	type taskMaterialsInput struct {
		TaskID string `path:"task_id" minLength:"1" maxLength:"100"`
		Limit  int    `query:"limit" default:"20" minimum:"1" maximum:"100"`
		Offset int    `query:"offset" default:"0" minimum:"0" maximum:"1000000"`
	}
	register(api, "list-task-materials", http.MethodGet, "/tasks/{task_id}/materials", "Read local source snapshots saved for this task", userSecurity(), func(ctx context.Context, input *taskMaterialsInput) (*itemResponse[application.TaskMaterialPage], error) {
		page, err := s.app.ListTaskMaterials(ctx, principal(ctx).UserID, input.TaskID, input.Limit, input.Offset)
		if err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[application.TaskMaterialPage]{Body: page}, nil
	})
	type readTaskMaterialInput struct {
		TaskID     string `path:"task_id" minLength:"1" maxLength:"100"`
		MaterialID string `path:"material_id" minLength:"1" maxLength:"100"`
		Limit      int    `query:"limit" default:"6000" minimum:"128" maximum:"12000" description:"Maximum UTF-8 bytes in this snapshot excerpt"`
		Offset     int    `query:"offset" default:"0" minimum:"0" maximum:"1048576" description:"UTF-8 byte offset; follow next_offset from the preceding page"`
	}
	register(api, "read-task-material", http.MethodGet, "/tasks/{task_id}/materials/{material_id}", "Read a saved source snapshot in UTF-8 pages without reopening its source", userSecurity(), func(ctx context.Context, input *readTaskMaterialInput) (*resourceResponse[application.TaskMaterialContent], error) {
		content, err := s.app.ReadTaskMaterial(ctx, principal(ctx).UserID, input.TaskID, input.MaterialID, input.Limit, input.Offset)
		if err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[application.TaskMaterialContent]{ETag: application.StrongETag("import", content.ID, content.Revision), Body: content}, nil
	})
	type detachTaskMaterialInput struct {
		TaskID         string `path:"task_id" minLength:"1" maxLength:"100"`
		MaterialID     string `path:"material_id" minLength:"1" maxLength:"100"`
		IfMatch        string `header:"If-Match" description:"Required import revision; missing returns 428"`
		IdempotencyKey string `header:"Idempotency-Key" required:"true"`
	}
	register(api, "detach-task-material", http.MethodDelete, "/tasks/{task_id}/materials/{material_id}", "Remove a local task-material reference while preserving its source and prior decision", userSecurity(), func(ctx context.Context, input *detachTaskMaterialInput) (*resourceResponse[application.TaskMaterial], error) {
		revision, err := revisionFromResourceETag(input.IfMatch, "import", input.MaterialID)
		if err != nil {
			return nil, mapError(err)
		}
		material, err := s.app.DetachTaskMaterial(ctx, principal(ctx).UserID, input.TaskID, input.MaterialID, revision)
		if err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[application.TaskMaterial]{ETag: application.StrongETag("import", material.ID, material.Revision), Body: material}, nil
	})
	type listConnectionsInput struct{}
	register(api, "list-integration-connections", http.MethodGet, "/integrations/connections", "List the current user's private tool connections", userSecurity(), func(ctx context.Context, _ *listConnectionsInput) (*listResponse[application.IntegrationConnectionView], error) {
		p := principal(ctx)
		items, err := s.app.IntegrationConnections(ctx, p.UserID, p.Role == "admin")
		if err != nil {
			return nil, mapError(err)
		}
		response := &listResponse[application.IntegrationConnectionView]{}
		response.Body.Items = items
		return response, nil
	})
	type saveConnectionInput struct {
		System         string `path:"system" enum:"fastwrite,fastread,fastnews,fastppt,fastlab"`
		IfMatch        string `header:"If-Match" description:"Required connection revision; missing returns 428"`
		IdempotencyKey string `header:"Idempotency-Key" required:"true"`
		Body           application.IntegrationConnectionCommand
	}
	register(api, "save-integration-connection", http.MethodPut, "/integrations/{system}/connection", "Configure this user's tool connection without exposing credentials", userSecurity(), func(ctx context.Context, input *saveConnectionInput) (*resourceResponse[application.IntegrationConnectionView], error) {
		revision, err := revisionFromResourceETag(input.IfMatch, "integration", input.System)
		if err != nil {
			return nil, mapError(err)
		}
		view, err := s.app.SaveIntegrationConnection(ctx, principal(ctx).UserID, input.System, revision, input.Body)
		if err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[application.IntegrationConnectionView]{ETag: application.StrongETag("integration", input.System, view.Revision), Body: view}, nil
	})
	type systemInput struct {
		System string `path:"system" enum:"fastwrite,fastread,fastnews,fastppt,fastlab"`
	}
	register(api, "diagnose-integration", http.MethodPost, "/integrations/{system}/diagnostics", "Test the actual read contract and explain connection failures", userSecurity(), func(ctx context.Context, input *systemInput) (*itemResponse[integrationDiagnosticBody], error) {
		diagnostic := s.app.DiagnoseIntegration(ctx, principal(ctx).UserID, input.System)
		return &itemResponse[integrationDiagnosticBody]{Body: integrationDiagnosticBody{OK: diagnostic.Code == "OK", Diagnostic: diagnostic}}, nil
	})
	type resourcesInput struct {
		System string `path:"system" enum:"fastwrite,fastread,fastnews,fastppt,fastlab"`
		Query  string `query:"q" maxLength:"1000"`
		Limit  int    `query:"limit" default:"20" minimum:"1" maximum:"100"`
		Offset int    `query:"offset" default:"0" minimum:"0" maximum:"1000000"`
	}
	register(api, "list-integration-resources", http.MethodGet, "/integrations/{system}/resources", "Read authorized external resources through a fixed adapter", userSecurity(), func(ctx context.Context, input *resourcesInput) (*itemResponse[integrationResourcesBody], error) {
		page, problem := s.app.ListIntegrationResources(ctx, principal(ctx).UserID, input.System, input.Query, input.Limit, input.Offset)
		if page.Items == nil {
			page.Items = []integrationclient.Resource{}
		}
		return &itemResponse[integrationResourcesBody]{Body: integrationResourcesBody{OK: problem == nil, Page: page, Diagnostic: problem}}, nil
	})
	type contentInput struct {
		System     string `path:"system" enum:"fastwrite,fastread,fastnews,fastppt,fastlab"`
		ResourceID string `query:"resource_id" required:"true" minLength:"1" maxLength:"256"`
		Path       string `query:"path" maxLength:"2048"`
	}
	register(api, "read-integration-content", http.MethodGet, "/integrations/{system}/content", "Read external text without changing the external tool", userSecurity(), func(ctx context.Context, input *contentInput) (*itemResponse[integrationContentBody], error) {
		content, problem := s.app.ReadIntegrationContent(ctx, principal(ctx).UserID, input.System, input.ResourceID, input.Path)
		if content.Children == nil {
			content.Children = []integrationclient.Resource{}
		}
		return &itemResponse[integrationContentBody]{Body: integrationContentBody{OK: problem == nil, Content: content, Diagnostic: problem}}, nil
	})
	type materialInput struct {
		System         string `path:"system" enum:"fastwrite,fastread,fastnews,fastppt,fastlab"`
		IdempotencyKey string `header:"Idempotency-Key" required:"true"`
		Body           struct {
			ResourceID string `json:"resource_id" minLength:"1" maxLength:"256"`
			Path       string `json:"path,omitempty" maxLength:"2048"`
			TaskID     string `json:"task_id" minLength:"1" maxLength:"100"`
		}
	}
	type materialBody struct {
		OK         bool                          `json:"ok"`
		ImportID   string                        `json:"import_id,omitempty"`
		Diagnostic *integrationclient.Diagnostic `json:"diagnostic,omitempty"`
	}
	register(api, "save-integration-material", http.MethodPost, "/integrations/{system}/materials", "Save a verified resource snapshot as a local candidate material", userSecurity(), func(ctx context.Context, input *materialInput) (*itemResponse[materialBody], error) {
		id, problem, err := s.app.SaveIntegrationMaterial(ctx, principal(ctx).UserID, input.System, input.Body.ResourceID, input.Body.Path, input.Body.TaskID)
		if err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[materialBody]{Body: materialBody{OK: problem == nil, ImportID: id, Diagnostic: problem}}, nil
	})
}
