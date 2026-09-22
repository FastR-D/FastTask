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
// ConvertExternalImport composes with the shared createTaskTx primitive inside its
// own transaction (wiring.md §4.1).
type IntegrationService struct {
	Store *persistence.Store
}

// NewIntegrationService builds the external-import service.
func NewIntegrationService(store *persistence.Store) *IntegrationService {
	return &IntegrationService{Store: store}
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
