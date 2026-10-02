import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import en from "@/i18n/locales/en";
import zh from "@/i18n/locales/zh";

const here = dirname(fileURLToPath(import.meta.url));
const read = (path: string) => readFileSync(resolve(here, path), "utf8");

/**
 * 源码在不同格式化批次下会在单/双引号之间切换，这里按语义切片而不是按字面
 * 引号匹配，避免"格式一变断言就红"。切片使用容忍两种引号的正则。
 */
function sliceBetween(source: string, start: RegExp, end: RegExp): string {
  const startMatch = source.match(start);
  if (!startMatch || startMatch.index === undefined) return "";
  const rest = source.slice(startMatch.index + startMatch[0].length);
  const endMatch = rest.match(end);
  return endMatch && endMatch.index !== undefined
    ? rest.slice(0, endMatch.index)
    : rest;
}

describe("Prompt Audit integration surface", () => {
  it("registers an admin-guarded route that stays reachable without risk control", () => {
    const router = read("../../../router/index.ts");
    const route = sliceBetween(
      router,
      /path:\s*["']\/admin\/prompt-audit["']/,
      /path:\s*["']\/admin\/usage["']/,
    );
    expect(route).toMatch(/requiresAuth:\s*true/);
    expect(route).toMatch(/requiresAdmin:\s*true/);
    // 合并页面在风控关闭时仍需可进入以管理下游 Mock，故本页面不再受风控可见性限制。
    expect(route).not.toMatch(/requiresRiskControl/);
  });

  it("keeps the content moderation route risk-control guarded", () => {
    const router = read("../../../router/index.ts");
    const route = sliceBetween(
      router,
      /path:\s*["']\/admin\/risk-control["']/,
      /path:\s*["']\/admin\/prompt-audit["']/,
    );
    expect(route).toMatch(/requiresRiskControl:\s*true/);
  });

  it("keeps the security group visible and gates only the content moderation child", () => {
    const sidebar = read("../../../components/layout/AppSidebar.vue");
    const group = sliceBetween(
      sidebar,
      /path:\s*["']\/admin\/security-audit["']/,
      /path:\s*["']\/admin\/redeem["']/,
    );
    expect(group).toMatch(/expandOnly:\s*true/);
    expect(group).toMatch(/path:\s*["']\/admin\/risk-control["']/);
    expect(group).toMatch(/path:\s*["']\/admin\/prompt-audit["']/);

    // 分组本身不再挂 featureFlag，风控关闭时整组仍可见；只有内容审计子入口受风控开关约束。
    const moderationIndex = group.search(
      /path:\s*["']\/admin\/risk-control["']/,
    );
    const promptIndex = group.search(/path:\s*["']\/admin\/prompt-audit["']/);
    expect(group.slice(0, moderationIndex)).not.toContain("featureFlag");
    expect(group.slice(moderationIndex, promptIndex)).toContain(
      "flagRiskControl",
    );
    expect(group.slice(promptIndex)).not.toContain("featureFlag");
  });

  it("keeps Prompt Audit locale trees symmetric and all operational controls named", () => {
    expect(Object.keys(zh.admin.promptAudit)).toEqual(
      Object.keys(en.admin.promptAudit),
    );
    // 下游 Mock 标签与风控关闭提示在两种语言下都存在。
    expect(zh.admin.promptAudit.tabs.mock).toBeTruthy();
    expect(en.admin.promptAudit.tabs.mock).toBeTruthy();
    expect(zh.admin.promptAudit.mock.riskControlOff).toBeTruthy();
    expect(en.admin.promptAudit.mock.riskControlOff).toBeTruthy();
    expect(zh.nav.securityAudit).toBeTruthy();
    expect(en.nav.securityAudit).toBeTruthy();
    const endpoint = read("../components/EndpointPool.vue");
    const events = read("../components/EventWorkspace.vue");
    expect(endpoint).toContain("aria-label");
    expect(events).toContain("aria-label");
    expect(events).toContain("overflow-x-auto");
    expect(events).toContain("sm:grid-cols-2");
  });
});
