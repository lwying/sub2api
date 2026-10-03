import type {
  Account,
  AccountPlatform,
  AccountType,
  AccountUsageInfo,
  AccountUsageStatsResponse,
  GroupPlatform,
  SubscriptionType,
} from "@/types";

/**
 * 账号展示单元格的窄类型集合。
 *
 * 管理端继续传完整 `Account`；普通用户只读模式传服务端已脱敏的窄投影，
 * 不伪造 `credentials`／`extra` 或补零。单元格用类型守卫区分
 * admin／readOnly 分支，避免每个字段访问都落在联合类型上。
 */

/** 只读用量单元格所需的账号身份与安全配额字段。 */
export interface ReadonlyUsageAccount {
  id: number;
  platform: AccountPlatform;
  type: AccountType;
  quota_limit?: number | null;
  quota_used?: number | null;
  quota_daily_limit?: number | null;
  quota_daily_used?: number | null;
  quota_weekly_limit?: number | null;
  quota_weekly_used?: number | null;
}

/** AccountUsageCell 的 account 联合类型。 */
export type ReadonlyUsageCellAccount = Account | ReadonlyUsageAccount;

/**
 * 注入的被动用量读取器。只读模式只消费它或 `batchedUsage`，
 * 绝不回退到管理端接口或主动探测。
 */
export type AccountUsageFetcher = (
  account: ReadonlyUsageAccount,
) => Promise<AccountUsageInfo>;

/** 只读容量单元格所需的安全字段。 */
export interface ReadonlyCapacityAccount {
  platform: AccountPlatform;
  type: AccountType;
  concurrency: number;
  current_concurrency?: number | null;
  window_cost_limit?: number | null;
  window_cost_sticky_reserve?: number | null;
  current_window_cost?: number | null;
  max_sessions?: number | null;
  active_sessions?: number | null;
  session_idle_timeout_minutes?: number | null;
  base_rpm?: number | null;
  rpm_strategy?: string | null;
  rpm_sticky_buffer?: number | null;
  current_rpm?: number | null;
  quota_limit?: number | null;
  quota_used?: number | null;
  quota_daily_limit?: number | null;
  quota_daily_used?: number | null;
  quota_weekly_limit?: number | null;
  quota_weekly_used?: number | null;
}

/**
 * 只读状态单元格所需的安全字段。不含 `extra`／`error_message`，
 * 因此只读分支不会渲染原始错误或模型级限流明细。
 *
 * `status` 允许服务端历史状态值；展示适配器负责规范化停用状态，
 * 不把只读响应强制窄化为管理端枚举。三个时间字段可缺失，不伪造值。
 */
export interface ReadonlyStatusAccount {
  platform: AccountPlatform;
  type: AccountType;
  status: string;
  schedulable: boolean;
  rate_limit_reset_at?: string | null;
  overload_until?: string | null;
  temp_unschedulable_until?: string | null;
  quota_limit?: number | null;
  quota_used?: number | null;
  quota_daily_limit?: number | null;
  quota_daily_used?: number | null;
  quota_weekly_limit?: number | null;
  quota_weekly_used?: number | null;
}

/** 只读统计弹窗所需的最小账号头部字段。 */
export interface ReadonlyStatsAccount {
  id: number;
  name: string;
  status: string;
}

/**
 * 注入的只读统计读取器。只读模式必须提供，缺失时 fail-closed，
 * 不回退到 `adminAPI.accounts.getStats`。
 */
export type AccountStatsFetcher = (
  account: ReadonlyStatsAccount,
) => Promise<AccountUsageStatsResponse>;

/** AccountGroupsCell 只读展示所需的安全分组子集（既有 GroupBadge 字段）。 */
export interface AccountCellGroup {
  id: number;
  name: string;
  platform?: GroupPlatform;
  subscription_type?: SubscriptionType;
  rate_multiplier?: number;
}
