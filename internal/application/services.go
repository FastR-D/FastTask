package application

import "github.com/FastR-D/FastTask/internal/persistence"

// wiring.md §4 continues the decomposition of the App facade into focused
// per-aggregate services. Step 4 extracts the three with the clearest boundaries
// and fewest dependencies: DeviceService (push devices, §7.7), IntegrationService
// (external imports, §7.8) and AdminService (users/sessions/audit, §7.1).
//
// Each is a plain struct with a plain Go constructor (wiring.md §2 rule 2), so it
// can be built and unit-tested without fx or App. App embeds them, so existing
// call sites keep resolving through promotion while the split proceeds; the
// embedded Store always aliases App.Store, so behaviour is unchanged.

// DeviceService owns push-notification device registration and token rotation
// (arch.md §7.7).
type DeviceService struct {
	Store *persistence.Store
}

// NewDeviceService builds the device service.
func NewDeviceService(store *persistence.Store) *DeviceService {
	return &DeviceService{Store: store}
}

// IntegrationService owns external-import intake and conversion (arch.md §7.8).
// ConvertExternalImport reads the import (Integration) and writes a task (Goal),
// so it holds a *GoalService and calls GoalService.createTaskTx inside its own
// transaction — the §4.1 cross-aggregate composition pattern.
type IntegrationService struct {
	Store *persistence.Store
	Goals *GoalService
}

// NewIntegrationService builds the external-import service. goals supplies the
// task-creation domain function used within the conversion transaction (§4.1).
func NewIntegrationService(store *persistence.Store, goals *GoalService) *IntegrationService {
	return &IntegrationService{Store: store, Goals: goals}
}

// AdminService owns user administration, session revocation and the audit log
// (arch.md §7.1). The admin gate itself is the package-level currentAdmin helper
// so non-App services (e.g. ProviderService) can enforce it too.
type AdminService struct {
	Store *persistence.Store
}

// NewAdminService builds the admin service.
func NewAdminService(store *persistence.Store) *AdminService {
	return &AdminService{Store: store}
}

// GoalService owns goals and the task tree (arch.md §7.2): create/update goals and
// tasks, complete tasks, and apply/reject agent proposals. It also exposes the
// tx-taking primitives createTaskTx and snapshotTaskTree so other aggregates
// (IntegrationService.ConvertExternalImport) can compose task writes inside their
// own transaction (wiring.md §4.1).
type GoalService struct {
	Store *persistence.Store
}

// NewGoalService builds the goal/task-tree service.
func NewGoalService(store *persistence.Store) *GoalService {
	return &GoalService{Store: store}
}

// PlanService owns the daily plan and its items (arch.md §7.3): create/replan/
// close a plan and add/update/satisfy items. satisfyItemTx is the tx-taking domain
// function ProgressService.CompletePlanItem composes within its own transaction
// (wiring.md §4.1: Progress writes the event, Plan changes the item status).
type PlanService struct {
	Store *persistence.Store
}

// NewPlanService builds the daily-plan service.
func NewPlanService(store *persistence.Store) *PlanService {
	return &PlanService{Store: store}
}

// ProgressService owns work sessions and progress events (arch.md §7.4).
// CompletePlanItem is the §4.1 cross-aggregate case: it writes a ProgressEvent and
// changes a plan item's status, so it holds a *PlanService and opens one
// transaction through the persistence.TxManager port (Store.WithTx), calling
// PlanService.satisfyItemTx inside it.
type ProgressService struct {
	Store *persistence.Store
	Plans *PlanService
}

// NewProgressService builds the progress service. plans supplies the plan-item
// domain function composed within CompletePlanItem's transaction (§4.1).
func NewProgressService(store *persistence.Store, plans *PlanService) *ProgressService {
	return &ProgressService{Store: store, Plans: plans}
}
