//go:build unit

package service

import (
	"context"
)

// traceSettingRepoStub is a minimal SettingRepository for Trace gate tests.
type traceSettingRepoStub struct {
	values map[string]string
}

func (r *traceSettingRepoStub) Get(_ context.Context, key string) (*Setting, error) {
	if value, ok := r.values[key]; ok {
		return &Setting{Key: key, Value: value}, nil
	}
	return nil, ErrSettingNotFound
}

func (r *traceSettingRepoStub) GetValue(ctx context.Context, key string) (string, error) {
	setting, err := r.Get(ctx, key)
	if err != nil {
		return "", err
	}
	return setting.Value, nil
}

func (r *traceSettingRepoStub) Set(ctx context.Context, key, value string) error {
	return r.SetMultiple(ctx, map[string]string{key: value})
}

func (r *traceSettingRepoStub) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	out := map[string]string{}
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func (r *traceSettingRepoStub) SetMultiple(_ context.Context, settings map[string]string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	for key, value := range settings {
		r.values[key] = value
	}
	return nil
}

func (r *traceSettingRepoStub) GetAll(_ context.Context) (map[string]string, error) {
	out := map[string]string{}
	for key, value := range r.values {
		out[key] = value
	}
	return out, nil
}

func (r *traceSettingRepoStub) Delete(_ context.Context, key string) error {
	delete(r.values, key)
	return nil
}

