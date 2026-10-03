import { apiClient } from "./client";
import type {
  Account,
  AccountUsageStatsResponse,
  PaginatedResponse,
} from "@/types";
import type {
  AccountDisplayRow,
  AccountRuntimeSnapshot,
} from "@/components/account/accountDisplay";
import type { AccountCellGroup } from "@/components/account/accountCellTypes";

/** 用户专属安全投影；与管理端显示共用组件，不共用管理员接口或完整 DTO。 */
export interface AssignedAccount extends Omit<AccountDisplayRow, "type"> {
  account_type: Account["type"];
  email_masked: string;
  username_masked: string;
  upstream_account_id_masked: string;
}

export interface AssignedAccountFilters {
  platform?: string;
  account_type?: string;
  status?: string;
  group?: string;
  search?: string;
}

export async function list(
  page = 1,
  pageSize = 20,
  filters: AssignedAccountFilters = {},
  options?: { signal?: AbortSignal },
): Promise<PaginatedResponse<AssignedAccount>> {
  const { data } = await apiClient.get<PaginatedResponse<AssignedAccount>>(
    "/accounts",
    {
      params: { page, page_size: pageSize, ...filters },
      signal: options?.signal,
    },
  );
  return data;
}

export async function getById(
  id: number,
  options?: { signal?: AbortSignal },
): Promise<AssignedAccount> {
  const { data } = await apiClient.get<AssignedAccount>(`/accounts/${id}`, {
    signal: options?.signal,
  });
  return data;
}

export async function getGroups(options?: {
  signal?: AbortSignal;
}): Promise<AccountCellGroup[]> {
  const { data } = await apiClient.get<AccountCellGroup[]>("/accounts/groups", {
    signal: options?.signal,
  });
  return data;
}

export async function getRuntime(
  accountIDs: number[],
  options?: { signal?: AbortSignal },
): Promise<Record<string, AccountRuntimeSnapshot>> {
  const { data } = await apiClient.post<{
    accounts: Record<string, AccountRuntimeSnapshot>;
  }>(
    "/accounts/runtime/batch",
    {
      account_ids: accountIDs,
    },
    { signal: options?.signal },
  );
  return data.accounts;
}

export async function getStats(
  id: number,
  days = 30,
): Promise<AccountUsageStatsResponse> {
  const { data } = await apiClient.get<
    Omit<AccountUsageStatsResponse, "upstream_endpoints">
  >(`/accounts/${id}/stats`, { params: { days } });
  return { ...data, upstream_endpoints: [] };
}

export const assignedAccountsAPI = {
  list,
  getById,
  getGroups,
  getRuntime,
  getStats,
};

export function isAssignedAccountsAccessDenied(error: unknown): boolean {
  const candidate = error as
    { status?: number; code?: string | number } | null | undefined;
  return (
    candidate?.status === 403 || candidate?.code === "ACCOUNT_VIEW_DISABLED"
  );
}

/** 不存在、未授权与已删除统一为 404，不通过错误推断账号是否存在。 */
export function isAssignedAccountNotFound(error: unknown): boolean {
  return (error as { status?: number } | null | undefined)?.status === 404;
}

export default assignedAccountsAPI;
