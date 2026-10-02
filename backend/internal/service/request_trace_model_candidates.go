package service

import (
	"context"
	"sort"
	"strings"
)

// 请求模型名称沿用 Trace 的 128 字节上限，过长值跳过而不是截成另一个模型。
const requestTraceModelCandidateMaxLength = 128

// RequestTraceModelCandidateProvider 是全局模型候选的窄读接缝，
// 不扩大 AdminService 接口，保留既有调用方和测试替身的兼容性。
type RequestTraceModelCandidateProvider interface {
	GetRequestTraceModelCandidates(ctx context.Context, channelModelKeys []string) ([]string, error)
}

// 复合路由只需读取对外模型名；旧仓储替身不需要实现该可选能力。
type compositeModelRoutePublicModelLister interface {
	ListEnabledPublicModels(ctx context.Context) ([]string, error)
}

// 在数据库内提取账号映射源键，避免为候选清单加载完整账号凭据。
type accountModelMappingSourceKeyReader interface {
	ListModelMappingSourceKeys(ctx context.Context) ([]string, error)
}

// GetRequestTraceModelCandidates 汇总已有平台目录、分组白名单与精确路由名、
// 账号映射源键、复合路由对外名称和渠道映射源键。只返回客户端请求模型名，
// 不使用映射目标，也不改变 Trace 的精确匹配语义；读取失败显式返回。
func (s *adminServiceImpl) GetRequestTraceModelCandidates(ctx context.Context, channelModelKeys []string) ([]string, error) {
	if s == nil {
		return nil, nil
	}
	builder := newRequestTraceModelCandidateBuilder()

	for _, model := range compositeDefaultModelsListCandidateIDs() {
		builder.add(model)
	}

	if s.groupRepo != nil {
		groups, err := s.GetAllGroupsIncludingInactive(ctx)
		if err != nil {
			return nil, err
		}
		for i := range groups {
			for _, model := range groups[i].ModelAllowlist.Models {
				builder.add(model)
			}
			for model := range groups[i].ModelRouting {
				builder.add(model)
			}
		}
	}

	if s.accountRepo != nil {
		// 优先只读源键；仅未提供该窄能力的旧仓储或替身使用既有全量读取。
		if reader, ok := s.accountRepo.(accountModelMappingSourceKeyReader); ok {
			keys, err := reader.ListModelMappingSourceKeys(ctx)
			if err != nil {
				return nil, err
			}
			for _, model := range keys {
				builder.add(model)
			}
		} else {
			accounts, err := s.accountRepo.ListAllWithFilters(ctx, "", "", "", "", 0, "")
			if err != nil {
				return nil, err
			}
			for i := range accounts {
				for model := range accounts[i].GetModelMapping() {
					builder.add(model)
				}
			}
		}
	}

	if s.compositeRouteRepo != nil {
		if lister, ok := s.compositeRouteRepo.(compositeModelRoutePublicModelLister); ok {
			publicModels, err := lister.ListEnabledPublicModels(ctx)
			if err != nil {
				return nil, err
			}
			for _, model := range publicModels {
				builder.add(model)
			}
		}
	}

	for _, model := range channelModelKeys {
		builder.add(model)
	}

	return builder.sorted(), nil
}

// 候选名称去空白、忽略大小写去重；模式与超长条目不是可选的精确模型。
type requestTraceModelCandidateBuilder struct {
	seen   map[string]struct{}
	models []string
}

func newRequestTraceModelCandidateBuilder() *requestTraceModelCandidateBuilder {
	return &requestTraceModelCandidateBuilder{seen: make(map[string]struct{})}
}

func (b *requestTraceModelCandidateBuilder) add(model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	if strings.Contains(model, "*") {
		return
	}
	if len(model) > requestTraceModelCandidateMaxLength {
		return
	}
	key := strings.ToLower(model)
	if _, ok := b.seen[key]; ok {
		return
	}
	b.seen[key] = struct{}{}
	b.models = append(b.models, model)
}

func (b *requestTraceModelCandidateBuilder) sorted() []string {
	sort.Strings(b.models)
	return b.models
}
