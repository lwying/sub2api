import type { AccountType } from "@/types";

export const ACCOUNT_TYPE_OPTIONS = [
  { value: "oauth", labelKey: "admin.accounts.oauthType" },
  { value: "setup-token", labelKey: "admin.accounts.setupToken" },
  { value: "apikey", labelKey: "admin.accounts.apiKey" },
  { value: "upstream", labelKey: "assignedAccounts.types.upstream" },
  { value: "bedrock", labelKey: "assignedAccounts.types.bedrock" },
  {
    value: "service_account",
    labelKey: "assignedAccounts.types.serviceAccount",
  },
] as const satisfies readonly { value: AccountType; labelKey: string }[];
