package service

import "context"

type stubRequestAuditRepo struct {
	created *RequestAuditRecord
	err     error
}

func (s *stubRequestAuditRepo) CreateRequestAudit(ctx context.Context, rec *RequestAuditRecord) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	s.created = rec
	return s.err
}

func (s *stubRequestAuditRepo) GetByUsageLogID(_ context.Context, usageLogID int64) (*RequestAuditRecord, error) {
	if s.created != nil && s.created.UsageLogID == usageLogID {
		return s.created, s.err
	}
	return nil, s.err
}

type settingGetAllRepoStub struct {
	values map[string]string
}

func (s *settingGetAllRepoStub) Get(ctx context.Context, key string) (*Setting, error) {
	panic("unexpected Get call")
}

func (s *settingGetAllRepoStub) GetValue(ctx context.Context, key string) (string, error) {
	panic("unexpected GetValue call")
}

func (s *settingGetAllRepoStub) Set(ctx context.Context, key, value string) error {
	panic("unexpected Set call")
}

func (s *settingGetAllRepoStub) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	panic("unexpected GetMultiple call")
}

func (s *settingGetAllRepoStub) SetMultiple(ctx context.Context, settings map[string]string) error {
	panic("unexpected SetMultiple call")
}

func (s *settingGetAllRepoStub) GetAll(ctx context.Context) (map[string]string, error) {
	out := make(map[string]string, len(s.values))
	for key, value := range s.values {
		out[key] = value
	}
	return out, nil
}

func (s *settingGetAllRepoStub) Delete(ctx context.Context, key string) error {
	panic("unexpected Delete call")
}
