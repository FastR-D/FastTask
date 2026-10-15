package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/FastR-D/FastTask/internal/integrationclient"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

var IntegrationSystems = []string{"fastwrite", "fastread", "fastnews", "fastppt", "fastlab"}

// RemoteToolService exposes fixed adapters using the current user's
// connection. Neither the browser nor a model can turn it into a general proxy.
type RemoteToolService struct {
	Store     *persistence.Store
	secretKey []byte
	client    integrationclient.Client
}

func NewRemoteToolService(store *persistence.Store, key []byte) *RemoteToolService {
	return &RemoteToolService{Store: store, secretKey: key, client: integrationclient.Client{Timeout: 5 * time.Second}}
}

type IntegrationConnectionView struct {
	System           string `json:"system"`
	APIURL           string `json:"api_url"`
	WebURL           string `json:"web_url"`
	Profile          string `json:"profile"`
	AuthKind         string `json:"auth_kind"`
	AuthConfigured   bool   `json:"auth_configured"`
	PersonalInstance bool   `json:"personal_instance"`
	Revision         int    `json:"revision"`
	CanEditAddress   bool   `json:"can_edit_address"`
}

type IntegrationConnectionCommand struct {
	APIURL           string `json:"api_url" maxLength:"2000"`
	WebURL           string `json:"web_url" maxLength:"2000"`
	Profile          string `json:"profile" enum:"auto,fastwrite,fastread-web,fastnews-panel,fastnews-local,fastppt-studio,fastlabs"`
	AuthKind         string `json:"auth_kind" enum:"bearer,cookie"`
	AuthToken        string `json:"auth_token,omitempty" maxLength:"8192"`
	ClearAuth        bool   `json:"clear_auth,omitempty"`
	PersonalInstance bool   `json:"personal_instance"`
}

func validIntegrationSystem(system string) bool {
	for _, candidate := range IntegrationSystems {
		if candidate == system {
			return true
		}
	}
	return false
}

func integrationView(record persistence.IntegrationConnection, admin bool) IntegrationConnectionView {
	return IntegrationConnectionView{System: record.System, APIURL: record.APIURL, WebURL: record.WebURL, Profile: record.Profile, AuthKind: record.AuthKind, AuthConfigured: record.CredentialCiphertext != "", PersonalInstance: record.PersonalInstance, Revision: record.Revision, CanEditAddress: admin}
}

func (s *RemoteToolService) connection(ctx context.Context, userID, system string) (persistence.IntegrationConnection, error) {
	return s.connectionFromDB(s.Store.DB.WithContext(ctx), userID, system)
}

func (s *RemoteToolService) connectionFromDB(db *gorm.DB, userID, system string) (persistence.IntegrationConnection, error) {
	if !validIntegrationSystem(system) {
		return persistence.IntegrationConnection{}, ErrValidation
	}
	var record persistence.IntegrationConnection
	err := db.Where("user_id = ? AND system = ?", userID, system).First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		authKind := "bearer"
		if system == "fastread" || system == "fastnews" {
			authKind = "cookie"
		}
		defaultRecord := persistence.IntegrationConnection{UserID: userID, System: system, Profile: "auto", AuthKind: authKind}
		var user persistence.User
		if err := db.Where("id = ? AND status = 'active'", userID).First(&user).Error; err != nil {
			return defaultRecord, notFound(err)
		}
		// Administrators register service endpoints. Other users inherit only an
		// address template, and must supply their own target-system credential.
		if user.Role != "admin" {
			var template persistence.IntegrationConnection
			err := db.Table("integration_connections AS ic").Select("ic.api_url, ic.web_url, ic.profile, ic.auth_kind").Joins("JOIN users u ON u.id = ic.user_id").Where("ic.system = ? AND u.role = 'admin' AND u.status = 'active' AND (ic.api_url != '' OR ic.web_url != '')", system).Order("ic.updated_at DESC, ic.id DESC").Take(&template).Error
			if err == nil {
				defaultRecord.APIURL, defaultRecord.WebURL, defaultRecord.Profile, defaultRecord.AuthKind = template.APIURL, template.WebURL, template.Profile, template.AuthKind
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return defaultRecord, err
			}
		}
		return defaultRecord, nil
	}
	return record, err
}

func (s *RemoteToolService) IntegrationConnections(ctx context.Context, userID string, admin bool) ([]IntegrationConnectionView, error) {
	items := make([]IntegrationConnectionView, 0, len(IntegrationSystems))
	for _, system := range IntegrationSystems {
		record, err := s.connection(ctx, userID, system)
		if err != nil {
			return nil, err
		}
		items = append(items, integrationView(record, admin))
	}
	return items, nil
}

func validateIntegrationURL(value string) error {
	if value == "" {
		return nil
	}
	if integrationclient.ValidateAddress(value) != nil {
		return ErrValidation
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(value, "\r\n\\") {
		return ErrValidation
	}
	// Explicit administrator configuration may target LAN tools. Metadata and
	// unspecified endpoints never represent a supported application endpoint.
	ip := net.ParseIP(u.Hostname())
	if ip != nil && (ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast()) {
		return ErrValidation
	}
	return nil
}

func (s *RemoteToolService) SaveIntegrationConnection(ctx context.Context, userID, system string, expected int, command IntegrationConnectionCommand) (IntegrationConnectionView, error) {
	var user persistence.User
	if err := s.Store.DB.WithContext(ctx).Where("id = ? AND status = 'active'", userID).First(&user).Error; err != nil {
		return IntegrationConnectionView{}, notFound(err)
	}
	admin := user.Role == "admin"
	command.APIURL, command.WebURL = strings.TrimRight(strings.TrimSpace(command.APIURL), "/"), strings.TrimRight(strings.TrimSpace(command.WebURL), "/")
	command.AuthToken = strings.TrimSpace(command.AuthToken)
	if command.Profile == "" {
		command.Profile = "auto"
	}
	if command.AuthKind == "" {
		command.AuthKind = "bearer"
	}
	if !validIntegrationSystem(system) || validateIntegrationURL(command.APIURL) != nil || validateIntegrationURL(command.WebURL) != nil || strings.ContainsAny(command.AuthToken, "\r\n") || len(command.AuthToken) > 8192 {
		return IntegrationConnectionView{}, ErrValidation
	}
	for _, char := range command.AuthToken {
		if char < 32 || char == 127 || (command.AuthKind == "cookie" && (char < 33 || char > 126 || strings.ContainsRune("\";,\\", char))) {
			return IntegrationConnectionView{}, ErrValidation
		}
	}
	profiles := map[string][]string{"fastwrite": {"fastwrite"}, "fastread": {"fastread-web"}, "fastnews": {"fastnews-panel", "fastnews-local"}, "fastppt": {"fastppt-studio"}, "fastlab": {"fastlabs"}}
	profileValid := command.Profile == "auto"
	for _, candidate := range profiles[system] {
		if command.Profile == candidate {
			profileValid = true
		}
	}
	if !profileValid || (command.AuthKind != "bearer" && command.AuthKind != "cookie") || (command.ClearAuth && command.AuthToken != "") {
		return IntegrationConnectionView{}, ErrValidation
	}
	var record persistence.IntegrationConnection
	err := s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var err error
		record, err = s.connectionFromDB(tx, userID, system)
		if err != nil {
			return err
		}
		if record.Revision != expected {
			return ErrRevision
		}
		if !admin && ((record.APIURL == "" && record.WebURL == "") || command.APIURL != record.APIURL || command.WebURL != record.WebURL || command.PersonalInstance != record.PersonalInstance) {
			return ErrForbidden
		}
		// Changing an endpoint or credential scheme never forwards an old secret.
		if command.ClearAuth || command.APIURL != record.APIURL || command.AuthKind != record.AuthKind || (command.APIURL == "" && command.WebURL != record.WebURL) {
			record.CredentialCiphertext = ""
		}
		if command.AuthToken != "" {
			record.CredentialCiphertext, err = encryptWithKey(s.secretKey, strings.TrimSpace(command.AuthToken))
			if err != nil {
				return fmt.Errorf("cannot securely store integration credential: %w", err)
			}
		}
		record.APIURL, record.WebURL, record.Profile, record.AuthKind, record.PersonalInstance = command.APIURL, command.WebURL, command.Profile, command.AuthKind, command.PersonalInstance
		record.Revision++
		record.UpdatedAt = persistence.Now()
		if record.ID == "" {
			record.ID, record.UserID, record.System, record.CreatedAt = persistence.NewID("integration"), userID, system, record.UpdatedAt
			return tx.Create(&record).Error
		}
		return tx.Save(&record).Error
	})
	return integrationView(record, admin), err
}

func (s *RemoteToolService) remoteConfig(ctx context.Context, userID, system string) (integrationclient.Config, *integrationclient.Diagnostic) {
	record, err := s.connection(ctx, userID, system)
	if err != nil {
		return integrationclient.Config{}, &integrationclient.Diagnostic{Code: "CONFIG_UNAVAILABLE", Message: "无法读取工具连接配置", Reason: "当前账号的连接配置不可用。", Action: "刷新后重试，或请管理员检查本地数据库。"}
	}
	credential, err := decryptWithKey(s.secretKey, record.CredentialCiphertext)
	if err != nil {
		return integrationclient.Config{}, &integrationclient.Diagnostic{Code: "CREDENTIAL_UNAVAILABLE", Message: "无法解密访问凭据", Reason: "连接凭据加密密钥未配置或已经变更。", Action: "请管理员恢复原加密配置，或在连接设置中重新填写本人凭据。"}
	}
	return integrationclient.Config{System: system, APIURL: record.APIURL, WebURL: record.WebURL, Profile: record.Profile, AuthKind: record.AuthKind, Credential: credential, PersonalInstance: record.PersonalInstance}, nil
}

func (s *RemoteToolService) DiagnoseIntegration(ctx context.Context, userID, system string) integrationclient.Diagnostic {
	config, diagnostic := s.remoteConfig(ctx, userID, system)
	if diagnostic != nil {
		return *diagnostic
	}
	return s.client.Diagnose(ctx, config)
}

func (s *RemoteToolService) ListIntegrationResources(ctx context.Context, userID, system, query string, limit, offset int) (integrationclient.Page, *integrationclient.Diagnostic) {
	config, diagnostic := s.remoteConfig(ctx, userID, system)
	if diagnostic != nil {
		return integrationclient.Page{}, diagnostic
	}
	return s.client.List(ctx, config, query, limit, offset)
}

func (s *RemoteToolService) ReadIntegrationContent(ctx context.Context, userID, system, id, path string) (integrationclient.Content, *integrationclient.Diagnostic) {
	config, diagnostic := s.remoteConfig(ctx, userID, system)
	if diagnostic != nil {
		return integrationclient.Content{}, diagnostic
	}
	return s.client.Read(ctx, config, id, path)
}

func boundedIntegrationText(value string, size int) string {
	if len(value) <= size {
		return value
	}
	value = value[:size]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

// SaveIntegrationMaterial deliberately writes only a candidate. The existing
// task-completion and daily-progress use cases remain the sole completion path.
func (s *RemoteToolService) SaveIntegrationMaterial(ctx context.Context, userID, system, id, path, taskID string) (string, *integrationclient.Diagnostic, error) {
	// Check ownership before a remote call, then again in the final transaction.
	if err := s.Store.Transaction(ctx, func(tx *gorm.DB) error { _, err := RequireLiveTaskTx(tx, userID, taskID); return err }); err != nil {
		return "", nil, err
	}
	connectionBeforeRead, err := s.connection(ctx, userID, system)
	if err != nil {
		return "", nil, err
	}
	content, diagnostic := s.ReadIntegrationContent(ctx, userID, system, id, path)
	if diagnostic != nil {
		return "", diagnostic, nil
	}
	if strings.TrimSpace(content.Text) == "" {
		return "", &integrationclient.Diagnostic{Code: "CONTENT_EMPTY", Message: "没有可以保存的正文", Reason: "当前资源只有目录或没有可读取的文本。", Action: "选择具体文件、论文报告或资讯条目后再保存。"}, nil
	}
	config, diagnostic := s.remoteConfig(ctx, userID, system)
	if diagnostic != nil {
		return "", diagnostic, nil
	}
	hash := persistence.Hash(content.Text)
	sourceID := "snapshot:" + persistence.Hash(config.APIURL+"|"+config.WebURL+"|"+id+"|"+path+"|"+content.Version+"|"+hash+"|"+taskID)
	metadata, _ := json.Marshal(map[string]any{"target_task_id": taskID, "resource_id": id, "path": path, "resource_version": content.Version, "text": boundedIntegrationText(content.Text, 64<<10), "truncated": content.Truncated, "warning": content.Warning, "evidence": content.Evidence, "origin": "fasttask_verified_adapter"})
	if len(metadata) > 128<<10 {
		return "", &integrationclient.Diagnostic{Code: "CONTENT_TOO_LARGE", Message: "材料超过保存上限", Reason: "正文转成结构化材料后超过本地收件箱限制。", Action: "选择更小的文件或单篇阅读报告。"}, nil
	}
	var importID string
	err = s.Store.Transaction(ctx, func(tx *gorm.DB) error {
		connectionAfterRead, err := s.connectionFromDB(tx, userID, system)
		if err != nil {
			return err
		}
		if connectionAfterRead.Revision != connectionBeforeRead.Revision || connectionAfterRead.APIURL != connectionBeforeRead.APIURL || connectionAfterRead.WebURL != connectionBeforeRead.WebURL {
			return ErrRevision
		}
		task, err := RequireLiveTaskTx(tx, userID, taskID)
		if err != nil {
			return err
		}
		var existing persistence.ExternalImport
		if err := tx.Where("user_id = ? AND source_system = ? AND (source_external_id = ? OR source_external_id LIKE ?)", userID, system, sourceID, sourceID+":saved:%").Order("created_at DESC, id DESC").First(&existing).Error; err == nil {
			if existing.Status != "rejected" && !materialDetachedFrom(existing, taskID) && (existing.TaskID == nil || *existing.TaskID == taskID) {
				importID = existing.ID
				return nil
			}
			// Removal or explicit reassignment applies to that local snapshot.
			// Saving it for this task again is a new choice and must produce a
			// usable candidate, while preserving the prior decision's audit trail.
			sourceID += ":saved:" + persistence.NewID("snapshot")
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		now := persistence.Now()
		item := persistence.ExternalImport{ID: persistence.NewID("import"), UserID: userID, SchemaVersion: "1.0", SourceSystem: system, SourceExternalID: sourceID, SourceURL: content.Resource.URL, ContentHash: hash, Kind: "research_material", Title: boundedIntegrationText(content.Resource.Title, 500), Description: boundedIntegrationText(content.Text, 1500), SuggestedGoalID: &task.GoalID, ArtifactsJSON: "[]", MetadataJSON: string(metadata), Status: "candidate", Revision: 1, CreatedAt: now, UpdatedAt: now}
		if err := tx.Create(&item).Error; err != nil {
			return err
		}
		importID = item.ID
		return createOutbox(tx, userID, "external_import.created", map[string]any{"import_id": item.ID, "source_system": system, "target_task_id": task.ID})
	})
	return importID, nil, err
}
