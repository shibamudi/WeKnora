package service

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	werrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)
// ListTenantsParams defines parameters for listing tenants with filtering and pagination
type ListTenantsParams struct {
	Page     int    // Page number for pagination
	PageSize int    // Number of items per page
	Status   string // Filter by tenant status
	Name     string // Filter by tenant name
}

// tenantService implements the TenantService interface
type tenantService struct {
	repo              interfaces.TenantRepository
	storageRepo       interfaces.StorageBackendRepository
	settings          interfaces.SystemSettingService
	modelRepo         interfaces.ModelRepository
	sandboxConfigRepo interfaces.SandboxConfigCreator
	agentRepo         interfaces.CustomAgentRepository
}

func NewTenantService(
	repo interfaces.TenantRepository,
	storageRepo interfaces.StorageBackendRepository,
	settings interfaces.SystemSettingService,
	modelRepo interfaces.ModelRepository,
	sandboxConfigRepo interfaces.SandboxConfigCreator,
	agentRepo interfaces.CustomAgentRepository,
) interfaces.TenantService {
	return &tenantService{
		repo:              repo,
		storageRepo:       storageRepo,
		settings:          settings,
		modelRepo:         modelRepo,
		sandboxConfigRepo: sandboxConfigRepo,
		agentRepo:         agentRepo,
	}
}

// CreateTenant creates a new tenant
func (s *tenantService) CreateTenant(ctx context.Context, tenant *types.Tenant) (*types.Tenant, error) {
	logger.Info(ctx, "Start creating tenant")

	if tenant.Name == "" {
		logger.Error(ctx, "Workspace name cannot be empty")
		return nil, errors.New("workspace name cannot be empty")
	}

	logger.Infof(ctx, "Creating tenant, name: %s", tenant.Name)

	// New tenants do not receive an API key by default. Integrations create
	// keys explicitly through tenant_api_keys.
	tenant.Status = "active"
	tenant.CreatedAt = time.Now()
	tenant.UpdatedAt = time.Now()
	// All creation paths, including registration and OIDC provisioning, must
	// persist the resolved default instead of relying on the database default.
	if tenant.StorageQuota <= 0 {
		tenant.StorageQuota = ResolveDefaultTenantStorageQuota(ctx, s.settings)
	}

	if err := s.validateStorageBucketUniqueness(ctx, tenant); err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"tenant_name": tenant.Name,
		})
		return nil, err
	}

	logger.Info(ctx, "Saving tenant information to database")
	if err := s.repo.CreateTenant(ctx, tenant); err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"tenant_name": tenant.Name,
		})
		return nil, err
	}
	if err := s.createDefaultStorageBackend(ctx, tenant); err != nil {
		// No related rows exist yet, so rolling the tenant back is safe and
		// avoids leaving a workspace that cannot bind new knowledge bases.
		_ = s.repo.DeleteTenant(ctx, tenant.ID)
		return nil, err
	}

	logger.Infof(ctx, "Tenant created successfully, ID: %d, name: %s", tenant.ID, tenant.Name)

	s.populateDefaultModelSettings(ctx, tenant)
	sandboxConfigID := s.populateDefaultSandboxConfig(ctx, tenant)
	s.populateBuiltinAgentConfigs(ctx, tenant, sandboxConfigID)

	return tenant, nil
}

// populateDefaultModelSettings queries builtin models flagged is_default=true
// and writes their IDs into the tenant's DefaultSettings so that new
// workspaces are immediately usable (KB creation → embedding, chat → LLM,
// summary → utility) without manual configuration. Admins can override
// via the workspace settings page afterwards.
func (s *tenantService) populateDefaultModelSettings(ctx context.Context, tenant *types.Tenant) {
	if s.modelRepo == nil || tenant == nil || tenant.ID == 0 {
		return
	}
	models, err := s.modelRepo.List(ctx, types.DefaultBuiltinModelTenantID, "", "")
	if err != nil || len(models) == 0 {
		logger.Warnf(ctx, "[tenant] skip default model population: no builtin models found for tenant %d", tenant.ID)
		return
	}

	ds := &types.DefaultSettings{}
	for _, m := range models {
		if !m.IsDefault || !m.IsBuiltin {
			continue
		}
		switch m.Type {
		case types.ModelTypeKnowledgeQA:
			if ds.ChatModelID == "" {
				ds.ChatModelID = m.ID
			} else if ds.UtilityModelID == "" {
				ds.UtilityModelID = m.ID
			}
		case types.ModelTypeEmbedding:
			if ds.EmbeddingModelID == "" {
				ds.EmbeddingModelID = m.ID
			}
		case types.ModelTypeRerank:
			if ds.RerankModelID == "" {
				ds.RerankModelID = m.ID
			}
		}
	}

	if ds.ChatModelID == "" && ds.EmbeddingModelID == "" && ds.RerankModelID == "" {
		return
	}

	tenant.DefaultSettings = ds
	if err := s.repo.UpdateTenant(ctx, tenant); err != nil {
		logger.Warnf(ctx, "[tenant] failed to persist default model settings for tenant %d: %v", tenant.ID, err)
	} else {
		logger.Infof(ctx, "[tenant] populated default models for tenant %d: chat=%s embed=%s rerank=%s utility=%s",
			tenant.ID, ds.ChatModelID, ds.EmbeddingModelID, ds.RerankModelID, ds.UtilityModelID)
	}
}

// populateDefaultSandboxConfig creates a default Docker sandbox config for
// new workspaces when WEKNORA_SANDBOX_DOCKER_ENABLED=true. Uses the default
// sandbox image and conservative resource limits. Admins can override via
// the workspace sandbox settings page.
func (s *tenantService) populateDefaultSandboxConfig(ctx context.Context, tenant *types.Tenant) string {
	if tenant == nil || tenant.ID == 0 {
		return ""
	}
	if enabled := strings.TrimSpace(os.Getenv("WEKNORA_SANDBOX_DOCKER_ENABLED")); !strings.EqualFold(enabled, "true") {
		return ""
	}
	cfg := &types.TenantSandboxConfig{
		SandboxType:      "docker",
		DefaultTimeoutSec: 300,
		Docker: &types.DockerSandboxConfig{
			Image: "wechatopenai/weknora-sandbox:latest",
		},
	}
	entity := &types.TenantSandboxConfigEntity{
		ID:          uuid.New().String(),
		TenantID:    tenant.ID,
		Name:        "Default Sandbox",
		SandboxType: cfg.SandboxType,
		Config:      cfg,
	}
	if err := s.sandboxConfigRepo.Create(ctx, entity); err != nil {
		logger.Warnf(ctx, "[tenant] failed to create default sandbox config for tenant %d: %v", tenant.ID, err)
		return ""
	}
	logger.Infof(ctx, "[tenant] populated default sandbox config for tenant %d: type=docker image=%s", tenant.ID, cfg.Docker.Image)
	return entity.ID
}

// populateBuiltinAgentConfigs persists built-in agents for a new workspace with
// the workspace's default model and sandbox config already filled in. Without
// this, built-in agents load from YAML with empty model_id / sandbox_config_id;
// the runtime fallbacks cover execution, but the editor's save validation and
// the agent card's model display both expect stored values.
func (s *tenantService) populateBuiltinAgentConfigs(ctx context.Context, tenant *types.Tenant, sandboxConfigID string) {
	if s.agentRepo == nil || tenant == nil || tenant.ID == 0 {
		return
	}
	chatModelID := ""
	if tenant.DefaultSettings != nil {
		chatModelID = tenant.DefaultSettings.ChatModelID
	}
	for _, agentID := range types.GetBuiltinAgentIDs() {
		agent := types.GetBuiltinAgent(agentID, tenant.ID)
		if agent == nil {
			continue
		}
		if chatModelID != "" {
			agent.Config.ModelID = chatModelID
		}
		if sandboxConfigID != "" {
			agent.Config.SandboxConfigID = sandboxConfigID
		}
		agent.EnsureDefaults()
		if err := s.agentRepo.CreateAgent(ctx, agent); err != nil {
			logger.Warnf(ctx, "[tenant] failed to seed builtin agent %s for tenant %d: %v", agentID, tenant.ID, err)
		}
	}
	logger.Infof(ctx, "[tenant] seeded builtin agents for tenant %d: model=%s sandbox=%s", tenant.ID, chatModelID, sandboxConfigID)
}

func (s *tenantService) createDefaultStorageBackend(ctx context.Context, tenant *types.Tenant) error {
	if s.storageRepo == nil || tenant == nil {
		return nil
	}
	provider := ""
	if tenant.StorageEngineConfig != nil {
		provider = tenant.StorageEngineConfig.DefaultProvider
	}
	backend := types.StorageBackendFromLegacy(tenant.ID, provider, tenant.StorageEngineConfig)
	if backend == nil {
		backend = types.StorageBackendFromEnvironment(tenant.ID)
	}
	if backend == nil {
		return errors.New("no supported default storage backend is configured")
	}
	backend.LegacyAlias = true
	if err := s.storageRepo.Create(ctx, backend); err != nil {
		return err
	}
	tenant.DefaultStorageBackendID = &backend.ID
	if err := s.repo.UpdateTenant(ctx, tenant); err != nil {
		_ = s.storageRepo.Delete(ctx, tenant.ID, backend.ID)
		return err
	}
	return nil
}

// GetTenantByID retrieves a tenant by their ID
func (s *tenantService) GetTenantByID(ctx context.Context, id uint64) (*types.Tenant, error) {
	if id == 0 {
		logger.Error(ctx, "Workspace ID cannot be 0")
		return nil, errors.New("tenant ID cannot be 0")
	}

	tenant, err := s.repo.GetTenantByID(ctx, id)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"tenant_id": id,
		})
		return nil, err
	}

	return tenant, nil
}

// GetTenantsByIDs batches GetTenantByID; returns a map keyed by tenant ID.
func (s *tenantService) GetTenantsByIDs(ctx context.Context, ids []uint64) (map[uint64]*types.Tenant, error) {
	return s.repo.GetTenantsByIDs(ctx, ids)
}

// ListTenants retrieves a list of all tenants
func (s *tenantService) ListTenants(ctx context.Context) ([]*types.Tenant, error) {
	tenants, err := s.repo.ListTenants(ctx)
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		return nil, err
	}

	logger.Infof(ctx, "Tenant list retrieved successfully, total: %d", len(tenants))
	return tenants, nil
}

// SetChatHistoryConfig persists the managed setting without writing a stale
// snapshot of other tenant fields. Callers hold MessageService's history lock.
func (s *tenantService) SetChatHistoryConfig(
	ctx context.Context, tenantID uint64, cfg *types.ChatHistoryConfig,
) error {
	if tenantID == 0 || cfg == nil {
		return errors.New("tenant ID and chat history configuration are required")
	}
	return s.repo.SetChatHistoryConfig(ctx, tenantID, cfg)
}

// UpdateTenant updates an existing tenant's information
func (s *tenantService) UpdateTenant(ctx context.Context, tenant *types.Tenant) (*types.Tenant, error) {
	if tenant.ID == 0 {
		logger.Error(ctx, "Workspace ID cannot be 0")
		return nil, errors.New("tenant ID cannot be 0")
	}

	logger.Infof(ctx, "Updating tenant, ID: %d, name: %s", tenant.ID, tenant.Name)

	if err := s.validateStorageBucketUniqueness(ctx, tenant); err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"tenant_id": tenant.ID,
		})
		return nil, err
	}

	tenant.UpdatedAt = time.Now()
	logger.Info(ctx, "Saving tenant information to database")

	if err := s.repo.UpdateTenant(ctx, tenant); err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"tenant_id": tenant.ID,
		})
		return nil, err
	}

	logger.Infof(ctx, "Tenant updated successfully, ID: %d", tenant.ID)
	return tenant, nil
}

// UpdateTenantProfile persists only supplied fields. In particular, clearing a
// description must not be skipped as a zero value or rewrite storage settings.
func (
	s *tenantService,
) UpdateTenantProfile(
	ctx context.Context,
	id uint64,
	name, description *string,
) (
	*types.Tenant,
	error,
) {
	if id == 0 || (name == nil && description == nil) {
		return nil, werrors.NewValidationError("workspace id and profile fields are required")
	}
	if name != nil {
		value := strings.TrimSpace(*name)
		if value == "" || utf8.RuneCountInString(value) > 128 {
			return nil, werrors.NewValidationError("name must contain 1 to 128 characters")
		}
		name = &value
	}
	if description != nil {
		value := strings.TrimSpace(*description)
		if utf8.RuneCountInString(value) > 512 {
			return nil, werrors.NewValidationError("description must contain at most 512 characters")
		}
		description = &value
	}
	if _, err := s.GetTenantByID(ctx, id); err != nil {
		return nil, err
	}
	if err := s.repo.UpdateTenantProfile(ctx, id, name, description); err != nil {
		return nil, err
	}
	return s.GetTenantByID(ctx, id)
}

// DeleteTenant removes a tenant by their ID
func (s *tenantService) DeleteTenant(ctx context.Context, id uint64) error {
	logger.Info(ctx, "Start deleting tenant")

	if id == 0 {
		logger.Error(ctx, "Workspace ID cannot be 0")
		return errors.New("tenant ID cannot be 0")
	}

	logger.Infof(ctx, "Deleting tenant, ID: %d", id)

	// Get tenant information for logging
	tenant, err := s.repo.GetTenantByID(ctx, id)
	if err != nil {
		if err.Error() == "record not found" {
			logger.Warnf(ctx, "Tenant to be deleted does not exist, ID: %d", id)
		} else {
			logger.ErrorWithFields(ctx, err, map[string]interface{}{
				"tenant_id": id,
			})
			return err
		}
	} else {
		logger.Infof(ctx, "Deleting tenant, ID: %d, name: %s", id, tenant.Name)
	}

	err = s.repo.DeleteTenant(ctx, id)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"tenant_id": id,
		})
		return err
	}

	logger.Infof(ctx, "Workspace deleted successfully, ID: %d", id)
	return nil
}

// ListAllTenants lists all tenants (for users with cross-tenant access permission)
// This method returns all tenants without filtering, intended for admin users
func (s *tenantService) ListAllTenants(ctx context.Context) ([]*types.Tenant, error) {
	tenants, err := s.repo.ListTenants(ctx)
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		return nil, err
	}

	logger.Infof(ctx, "All tenants list retrieved successfully, total: %d", len(tenants))
	return tenants, nil
}

// BulkSetStorageQuota delegates to the repository. Validation is
// minimal — quotaBytes <= 0 is rejected because the storage-quota
// enforcement in knowledge_create.go treats <=0 as "unlimited", which
// is never what a SystemAdmin pressing "apply default" intends.
func (s *tenantService) BulkSetStorageQuota(ctx context.Context, quotaBytes int64) (int64, error) {
	if quotaBytes <= 0 {
		return 0, errors.New("quota must be positive")
	}
	affected, err := s.repo.BulkSetStorageQuota(ctx, quotaBytes)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{"quota_bytes": quotaBytes})
		return 0, err
	}
	logger.Infof(ctx, "Bulk set storage_quota=%d on %d tenants", quotaBytes, affected)
	return affected, nil
}

// SearchTenants searches tenants with pagination and filters
func (s *tenantService) SearchTenants(ctx context.Context, keyword string, tenantID uint64, page, pageSize int) ([]*types.Tenant, int64, error) {
	tenants, total, err := s.repo.SearchTenants(ctx, keyword, tenantID, page, pageSize)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"keyword":  keyword,
			"tenantID": tenantID,
			"page":     page,
			"pageSize": pageSize,
		})
		return nil, 0, err
	}

	logger.Infof(ctx, "Tenants search completed, keyword: %s, tenantID: %d, page: %d, pageSize: %d, total: %d, found: %d",
		keyword, tenantID, page, pageSize, total, len(tenants))
	return tenants, total, nil
}

// GetTenantByIDForUser gets a tenant by ID with permission check
// This method verifies that the user has permission to access the tenant
func (s *tenantService) GetTenantByIDForUser(ctx context.Context, tenantID uint64, userID string) (*types.Tenant, error) {
	tenant, err := s.repo.GetTenantByID(ctx, tenantID)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"tenant_id": tenantID,
			"user_id":   userID,
		})
		return nil, err
	}

	return tenant, nil
}

func (s *tenantService) GetWeKnoraCloudCredentials(ctx context.Context) *types.WeKnoraCloudCredentials {
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok {
		return nil
	}
	// Model initialization may run for a shared KB's owner while middleware
	// still carries the caller's tenant info. Never reuse another tenant's credentials.
	if tenant, ok := types.TenantInfoFromContext(ctx); ok && tenant != nil && tenant.ID == tenantID {
		if creds := tenant.Credentials.GetWeKnoraCloud(); creds != nil {
			return creds
		}
	}

	tenant, err := s.repo.GetTenantByID(ctx, tenantID)
	if err != nil || tenant == nil {
		return nil
	}
	return tenant.Credentials.GetWeKnoraCloud()
}

func (s *tenantService) validateStorageBucketUniqueness(ctx context.Context, tenant *types.Tenant) error {
	if tenant.StorageEngineConfig == nil {
		return nil
	}

	// Fetch existing tenant from DB to compare
	var oldTenant *types.Tenant
	if tenant.ID != 0 {
		var err error
		oldTenant, err = s.repo.GetTenantByID(ctx, tenant.ID)
		if err != nil && err.Error() != "tenant not found" && err.Error() != "record not found" {
			return err
		}
	}

	// Fetch ALL tenants to check for collision.
	allTenants, err := s.repo.ListTenants(ctx)
	if err != nil {
		return err
	}

	// Helper to get bucket names from a StorageEngineConfig
	getBuckets := func(cfg *types.StorageEngineConfig) map[string]string {
		if cfg == nil {
			return nil
		}
		res := make(map[string]string)
		if cfg.MinIO != nil && cfg.MinIO.BucketName != "" {
			res["minio"] = cfg.MinIO.BucketName
		}
		if cfg.COS != nil && cfg.COS.BucketName != "" {
			res["cos"] = cfg.COS.BucketName
		}
		if cfg.TOS != nil && cfg.TOS.BucketName != "" {
			res["tos"] = cfg.TOS.BucketName
		}
		if cfg.S3 != nil && cfg.S3.BucketName != "" {
			res["s3"] = cfg.S3.BucketName
		}
		if cfg.OSS != nil && cfg.OSS.BucketName != "" {
			res["oss"] = cfg.OSS.BucketName
		}
		return res
	}

	var oldBuckets map[string]string
	if oldTenant != nil {
		oldBuckets = getBuckets(oldTenant.StorageEngineConfig)
	}
	newBuckets := getBuckets(tenant.StorageEngineConfig)

	// Collect buckets used by other tenants
	usedByOthers := make(map[string]map[string]bool) // provider -> set of bucket names
	for _, t := range allTenants {
		if t.ID == tenant.ID {
			continue
		}
		tb := getBuckets(t.StorageEngineConfig)
		for p, b := range tb {
			if usedByOthers[p] == nil {
				usedByOthers[p] = make(map[string]bool)
			}
			usedByOthers[p][b] = true
		}
	}

	// Check if any NEW bucket is already used by someone else, AND it's different from the OLD bucket
	for p, b := range newBuckets {
		oldB := oldBuckets[p]
		if b != oldB { // User is trying to change their bucket name or set a new one
			if usedByOthers[p] != nil && usedByOthers[p][b] {
				return werrors.NewBadRequestError("存储桶名称「" + b + "」已被其他空间使用，为保证数据隔离，请使用其他名称")
			}
		}
	}

	return nil
}
