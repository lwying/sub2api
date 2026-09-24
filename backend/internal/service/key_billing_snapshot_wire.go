package service

func ProvideKeyBillingSnapshotService(store KeyBillingSnapshotStore, settings *SettingService, rates UserGroupRateRepository) *KeyBillingSnapshotService {
	return NewKeyBillingSnapshotService(store, settings, rates)
}
