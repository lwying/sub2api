import type { Account, AccountUsageInfo, WindowStats } from "@/types";
import type { Column } from "@/components/common/types";
import type {
  AccountCellGroup,
  ReadonlyCapacityAccount,
  ReadonlyStatusAccount,
  ReadonlyUsageAccount,
} from "./accountCellTypes";

/** 管理页、只读页和授权选择共用的基础展示字段，不包含凭据或内部配置。 */
export interface AccountTableRow {
  id: number;
  name: string;
  platform: string;
  type: string;
  status: string;
}

/** 用户账号行由服务端安全投影构成，管理端完整账号在结构上也兼容。 */
export interface AccountDisplayRow
  extends
    AccountTableRow,
    ReadonlyCapacityAccount,
    ReadonlyStatusAccount,
    ReadonlyUsageAccount {
  platform: Account["platform"];
  type: Account["type"];
  status: string;
  groups?: AccountCellGroup[];
  email_masked?: string;
  username_masked?: string;
  upstream_account_id_masked?: string;
}

export interface AccountRuntimeSnapshot {
  usage: AccountUsageInfo | null;
  today_stats: WindowStats | null;
  current_concurrency: number | null;
}

export const normalizeAccountStatus = (status: string): string => {
  const value = status.trim().toLowerCase();
  return value === "disabled" ? "inactive" : value;
};

const statusLabelKeys: Record<string, string> = {
  active: "active",
  inactive: "inactive",
  error: "error",
  expired: "expired",
  cooldown: "cooldown",
  paused: "paused",
  limited: "limited",
  overloaded: "overloaded",
  rate_limited: "rateLimited",
  temp_unschedulable: "tempUnschedulable",
  quota_exceeded: "quotaExceeded",
  unschedulable: "unschedulable",
};

export const accountStatusLabel = (
  status: string,
  t: (key: string) => string,
): string => {
  const key = statusLabelKeys[normalizeAccountStatus(status)];
  return key ? t(`admin.accounts.status.${key}`) : status;
};

export const accountReadColumns = (t: (key: string) => string): Column[] => [
  { key: "name", label: t("admin.accounts.columns.name"), sortable: true },
  { key: "platform_type", label: t("admin.accounts.columns.platformType") },
  { key: "status", label: t("admin.accounts.columns.status") },
  { key: "capacity", label: t("admin.accounts.columns.capacity") },
  { key: "today_stats", label: t("admin.accounts.columns.todayStats") },
  { key: "groups", label: t("admin.accounts.columns.groups") },
  { key: "usage", label: t("admin.accounts.columns.usageWindows") },
  { key: "actions", label: t("common.actions") },
];
