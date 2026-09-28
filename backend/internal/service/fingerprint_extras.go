package service

// 指纹 seed 准备只有这一处组合点。
//
// Codex 与 Claude 各自管理一份系统 seed，两者互相独立，但准备顺序有硬约束：
// prepareClaude* 依赖「account.Extra 仍是行里已存的旧值」来读取该账号现有 seed。
// 谁先把 account.Extra 覆盖成传入载荷，第二级就会把调用方刚塞进来的 seed
// 误认成账号真实 seed 并保留下来——那正是 seed 生命周期规则要防的事
// （见 prepareCodexFingerprintExtraForUpdate / prepareClaudeFingerprintExtraForUpdate）。
//
// 之前这段顺序在两个文件里手写嵌套了六处，改一处漏一处就会静默出错，
// 因此收回到这里：调用点只表达「准备这个载荷」，顺序由本文件保证。

// prepareFingerprintExtraForCreate 组合两级 seed 准备（Codex → Claude）。
// 新建账号没有存量 seed，两级都只从本次传入的 extra 出发并各自剥离自己的键，
// 因此内层顺序只影响哪一级先剥，不影响结果。
func prepareFingerprintExtraForCreate(platform, accountType string, extra map[string]any) map[string]any {
	return prepareClaudeFingerprintExtraForCreate(platform, accountType,
		prepareCodexFingerprintExtraForCreate(platform, accountType, extra))
}

// prepareFingerprintExtraForUpdate 组合两级 seed 准备（Codex → Claude）。
//
// 调用方必须把返回值赋给 account.Extra，并且在调用之前**不要**覆盖 account.Extra：
// 两级都以 account.Extra 为准读取该账号已存的 seed。传入 nil extra 时可直接传
// account.Extra（赋值发生在两次准备都返回之后，读到的是同一个旧值）。
func prepareFingerprintExtraForUpdate(account *Account, extra map[string]any) map[string]any {
	return prepareClaudeFingerprintExtraForUpdate(account,
		prepareCodexFingerprintExtraForUpdate(account, extra))
}
