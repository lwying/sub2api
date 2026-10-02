package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// 手动清理 Trace 的独立用例。它只删除 Trace 信封／阶段／本文授权的链路断言，
// 绝不触碰 usage、计费或用户数据；链接的 Trace 允许删除，但它指向的使用记录
// 与计费事实保持原样。删除能力与采集、导出设置完全解耦。
const (
	// RequestTraceDeleteMaxSelectedIDs 与"导出所选"同一个有界集合上限：
	// 勾选清理复用列表勾选与导出的数量口径，不另造更小的上限，否则同一批勾选
	// 在导出可用、清理却被拒。
	RequestTraceDeleteMaxSelectedIDs = RequestTraceExportMaxSelectedIDs
	// RequestTraceDeleteBatchSize 是筛选删除每一批的行数上限。它只约束单次事务的
	// 大小，不限制总共能删多少：循环直到没有更多匹配行。
	RequestTraceDeleteBatchSize = 200
	// RequestTraceDeleteConfirmationTTL 是预览令牌的有效期。预览是确定的历史边界，
	// 不承诺数据集静止；过期、换管理员或换筛选都必须重新预览。
	RequestTraceDeleteConfirmationTTL = 5 * time.Minute
	// 单次清理在前端 30 秒请求超时前停止，并回显已完成批次的实际数量。
	RequestTraceDeleteMaxRuntime = 25 * time.Second
)

var (
	// ErrRequestTraceDeleteInvalidFilter 表示筛选缺失或非法。空筛选是"删除全部"，
	// 绝不退化成合法输入。
	ErrRequestTraceDeleteInvalidFilter = errors.New("request trace delete requires a non-empty explicit filter")
	// ErrRequestTraceDeleteConfirmRequired 表示缺少显式 confirm=true。
	ErrRequestTraceDeleteConfirmRequired = errors.New("request trace delete requires confirm=true")
	// ErrRequestTraceDeleteSelectionInvalid 表示勾选集合为空、超限、形状非法或重复。
	ErrRequestTraceDeleteSelectionInvalid = errors.New("request trace delete selection is invalid")
	// ErrRequestTraceDeleteConfirmationInvalid 表示确认令牌不可读、已过期、换管理员或与
	// 本次筛选／快照边界不一致。
	ErrRequestTraceDeleteConfirmationInvalid = errors.New("request trace delete confirmation token is invalid or expired")
)

// RequestTraceDeleteRepository 是手动清理的窄写接缝：预览只读，删除按明确集合或
// 已执行筛选的有界批次进行。它与读取／导出仓储分开，避免把删除职责塞进读侧接口。
type RequestTraceDeleteRepository interface {
	// PreviewRequestTraceDelete 返回匹配行数与该筛选下的内部最大 id 快照边界。
	PreviewRequestTraceDelete(ctx context.Context, filter RequestTraceExportFilter) (matched int64, snapshotMaxID int64, err error)
	// DeleteRequestTracesByIDs 按外部 trace_id 集合删除，返回实际删除行数。
	DeleteRequestTracesByIDs(ctx context.Context, traceIDs []string) (int64, error)
	// DeleteRequestTracesByFilter 只删除 id <= snapshotMaxID 的匹配行，分批推进。
	// completed 为假且 deleted>0 表示被中断的部分完成，必须如实回显。
	DeleteRequestTracesByFilter(ctx context.Context, filter RequestTraceExportFilter, snapshotMaxID int64, batchSize int) (deleted int64, completed bool, err error)
}

type requestTraceDeleteClaims struct {
	FilterHash    string    `json:"filter_hash"`
	SnapshotMaxID int64     `json:"snapshot_max_id"`
	AdminID       int64     `json:"admin_id"`
	IssuedAt      time.Time `json:"issued_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type RequestTraceDeletePreview struct {
	MatchedCount      int64     `json:"matched_count"`
	SnapshotMaxID     int64     `json:"snapshot_max_id"`
	FilterHash        string    `json:"filter_hash"`
	ConfirmationToken string    `json:"confirmation_token"`
	ExpiresAt         time.Time `json:"expires_at"`
}

// RequestTraceDeleteResult 是删除结果：deleted_count 是实际删除行数，completed 说明
// 这次调用是否走完了整个匹配集合。部分完成绝不报告成全成功。
type RequestTraceDeleteResult struct {
	DeletedCount int64 `json:"deleted_count"`
	Completed    bool  `json:"completed"`
}

type RequestTraceDeleteByFilterRequest struct {
	Filter            RequestTraceExportFilter `json:"filter"`
	SnapshotMaxID     int64                    `json:"snapshot_max_id"`
	FilterHash        string                   `json:"filter_hash"`
	ConfirmationToken string                   `json:"confirmation_token"`
	Confirm           bool                     `json:"confirm"`
}

// RequestTraceDeleteService 只编排预览／确认／批量删除；筛选语义由仓储复用列表与
// 导出的同一 clause，令牌只绑定管理员、筛选哈希与快照边界，不落任何持久存储。
type RequestTraceDeleteService struct {
	repo   RequestTraceDeleteRepository
	cipher SecretEncryptor
	now    func() time.Time
}

func NewRequestTraceDeleteService(repo RequestTraceDeleteRepository, cipher SecretEncryptor) *RequestTraceDeleteService {
	return &RequestTraceDeleteService{repo: repo, cipher: cipher, now: time.Now}
}

// RequestTraceDeleteFilterHash 绑定规范化筛选与快照边界：任一变化都会让旧的确认
// 令牌失效，因此换筛选、换边界或篡改哈希都必须重新预览。
func RequestTraceDeleteFilterHash(filter RequestTraceExportFilter, snapshotMaxID int64) string {
	payload := struct {
		Filter        RequestTraceExportFilter `json:"filter"`
		SnapshotMaxID int64                    `json:"snapshot_max_id"`
	}{filter, snapshotMaxID}
	raw, _ := json.Marshal(payload)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func (s *RequestTraceDeleteService) PreviewDelete(ctx context.Context, filter RequestTraceExportFilter, adminID int64) (*RequestTraceDeletePreview, error) {
	if s == nil || s.repo == nil || s.cipher == nil || adminID <= 0 {
		return nil, ErrRequestTraceRepositoryUnavailable
	}
	canonical, err := canonicalRequestTraceDeleteFilter(filter)
	if err != nil {
		return nil, err
	}
	matched, snapshotMaxID, err := s.repo.PreviewRequestTraceDelete(ctx, canonical)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	expires := now.Add(RequestTraceDeleteConfirmationTTL)
	claims := requestTraceDeleteClaims{
		FilterHash: RequestTraceDeleteFilterHash(canonical, snapshotMaxID), SnapshotMaxID: snapshotMaxID,
		AdminID: adminID, IssuedAt: now, ExpiresAt: expires,
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		return nil, err
	}
	token, err := s.cipher.Encrypt(string(raw))
	if err != nil {
		return nil, err
	}
	return &RequestTraceDeletePreview{
		MatchedCount: matched, SnapshotMaxID: snapshotMaxID,
		FilterHash: claims.FilterHash, ConfirmationToken: token, ExpiresAt: expires,
	}, nil
}

func (s *RequestTraceDeleteService) DeleteByFilter(ctx context.Context, request RequestTraceDeleteByFilterRequest, adminID int64) (*RequestTraceDeleteResult, error) {
	if s == nil || s.repo == nil || s.cipher == nil || adminID <= 0 {
		return nil, ErrRequestTraceRepositoryUnavailable
	}
	if !request.Confirm {
		return nil, ErrRequestTraceDeleteConfirmRequired
	}
	canonical, err := canonicalRequestTraceDeleteFilter(request.Filter)
	if err != nil {
		return nil, err
	}
	plain, err := s.cipher.Decrypt(strings.TrimSpace(request.ConfirmationToken))
	if err != nil {
		return nil, ErrRequestTraceDeleteConfirmationInvalid
	}
	var claims requestTraceDeleteClaims
	if json.Unmarshal([]byte(plain), &claims) != nil {
		return nil, ErrRequestTraceDeleteConfirmationInvalid
	}
	computed := RequestTraceDeleteFilterHash(canonical, request.SnapshotMaxID)
	if claims.AdminID != adminID || claims.SnapshotMaxID != request.SnapshotMaxID ||
		claims.FilterHash != request.FilterHash || request.FilterHash != computed ||
		!s.now().UTC().Before(claims.ExpiresAt) {
		return nil, ErrRequestTraceDeleteConfirmationInvalid
	}
	deleteCtx, cancel := context.WithTimeout(ctx, RequestTraceDeleteMaxRuntime)
	defer cancel()
	deleted, completed, err := s.repo.DeleteRequestTracesByFilter(deleteCtx, canonical, request.SnapshotMaxID, RequestTraceDeleteBatchSize)
	result := &RequestTraceDeleteResult{DeletedCount: deleted, Completed: completed}
	if err != nil {
		if deleted > 0 {
			// 部分完成如实回显：保留真实删除数，completed=false，同时把错误交回
			// 调用方以便审计，不把它粉饰成成功。
			return result, err
		}
		return nil, err
	}
	return result, nil
}

func (s *RequestTraceDeleteService) DeleteByIDs(ctx context.Context, traceIDs []string, confirm bool) (*RequestTraceDeleteResult, error) {
	if s == nil || s.repo == nil {
		return nil, ErrRequestTraceRepositoryUnavailable
	}
	if !confirm {
		return nil, ErrRequestTraceDeleteConfirmRequired
	}
	ids, err := validRequestTraceDeleteSelection(traceIDs)
	if err != nil {
		return nil, err
	}
	deleted, err := s.repo.DeleteRequestTracesByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	return &RequestTraceDeleteResult{DeletedCount: deleted, Completed: true}, nil
}

// canonicalRequestTraceDeleteFilter 规范化筛选并强制"非空实际筛选"。
// 勾选集合走独立的批量入口，因此这里拒绝 TraceIDs，避免两条删除路径的语义重叠。
func canonicalRequestTraceDeleteFilter(filter RequestTraceExportFilter) (RequestTraceExportFilter, error) {
	if len(filter.TraceIDs) > 0 {
		return RequestTraceExportFilter{}, ErrRequestTraceDeleteInvalidFilter
	}
	canonical := filter
	canonical.TraceID = strings.TrimSpace(filter.TraceID)
	canonical.RouteFamily = strings.TrimSpace(filter.RouteFamily)
	canonical.RequestedModel = strings.TrimSpace(filter.RequestedModel)
	canonical.Platform = strings.TrimSpace(filter.Platform)
	canonical.Keyword = strings.TrimSpace(filter.Keyword)
	if filter.CreatedFrom != nil {
		value := filter.CreatedFrom.UTC()
		canonical.CreatedFrom = &value
	}
	if filter.CreatedTo != nil {
		value := filter.CreatedTo.UTC()
		canonical.CreatedTo = &value
	}
	if !exportFilterValid(canonical) || requestTraceDeleteFilterEmpty(canonical) {
		return RequestTraceExportFilter{}, ErrRequestTraceDeleteInvalidFilter
	}
	return canonical, nil
}

func requestTraceDeleteFilterEmpty(filter RequestTraceExportFilter) bool {
	return filter.TraceID == "" && filter.RouteFamily == "" && filter.ClientStatus == nil &&
		filter.CreatedFrom == nil && filter.CreatedTo == nil && filter.UsageLinked == nil &&
		filter.UsageLogID == nil && filter.AccountID == nil && filter.GroupID == nil &&
		!requestTraceDeleteUnknownSelected(filter.GroupUnknown) && filter.RequestedModel == "" &&
		!requestTraceDeleteUnknownSelected(filter.ModelUnknown) && filter.Platform == "" &&
		!requestTraceDeleteUnknownSelected(filter.PlatformUnknown) && filter.UserID == nil &&
		!requestTraceDeleteUnknownSelected(filter.UserUnknown) && filter.APIKeyID == nil &&
		!requestTraceDeleteUnknownSelected(filter.APIKeyUnknown) && filter.Keyword == ""
}

// requestTraceDeleteUnknownSelected 把"未知"开关按**选中未知**来判空：只有 true 才是
// 一个真实的正向条件。`*_unknown=false` 在界面上通常只是默认值，若把它当成非空筛选，
// 一个只有 `{"model_unknown":false}` 的请求就会以"看似有筛选"的形式匹配所有已知模型
// 的记录。删除侧因此把 false 视同未给，只允许 true 单独构成筛选。
func requestTraceDeleteUnknownSelected(value *bool) bool {
	return value != nil && *value
}

// validRequestTraceDeleteSelection 校验勾选集合：非空、有界、逐个 32 位十六进制且
// 不重复。重复会让"勾选 N 条"与实际删除基数对不上，因此直接拒绝而不是静默去重。
func validRequestTraceDeleteSelection(traceIDs []string) ([]string, error) {
	if len(traceIDs) == 0 || len(traceIDs) > RequestTraceDeleteMaxSelectedIDs {
		return nil, ErrRequestTraceDeleteSelectionInvalid
	}
	seen := make(map[string]struct{}, len(traceIDs))
	for _, id := range traceIDs {
		if !requestTraceExportIDShape.MatchString(id) {
			return nil, ErrRequestTraceDeleteSelectionInvalid
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, ErrRequestTraceDeleteSelectionInvalid
		}
		seen[id] = struct{}{}
	}
	return traceIDs, nil
}
