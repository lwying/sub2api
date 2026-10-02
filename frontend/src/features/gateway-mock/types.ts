/**
 * The admin contract for the downstream-test-request mock: the replace-set rule
 * list and the read-only minimal hit events.
 *
 * The events answer "which rule fired, on which request, when" and carry
 * observable metadata only. They never carry the configured keyword or the reply
 * text — neither is stored — and the parsers below keep that property: an
 * unexpected field is dropped rather than carried into a view, so no field the
 * server did not send can appear here.
 */

/** Rule and reply bounds the server enforces; mirrored so a view can state them. */
export const gatewayMockKeywordMaxLength = 200;
export const gatewayMockReplyMaxLength = 4000;
export const gatewayMockMaxRules = 200;

/** The largest page the events endpoint will answer with. */
export const gatewayMockEventMaxPageSize = 100;

/**
 * The protocols a hit can be recorded from. The database constrains the column
 * to this set, so a value outside it is labelled as unknown rather than echoed.
 */
export const gatewayMockProtocols = [
  "messages",
  "chat_completions",
  "responses",
] as const;

export type GatewayMockProtocol = (typeof gatewayMockProtocols)[number];

/**
 * The closed set of content-audit states a hit can carry. `skipped_local_mock` is the
 * new strict early match: the gateway answered locally before either content audit
 * ran, so no audit executed. `unknown` is every other case — a legacy record written
 * before this field existed, or a value this build does not recognise. History is
 * never guessed at, so an absent or unexpected value degrades to `unknown` rather
 * than being reported as `skipped_local_mock`.
 */
export const gatewayMockContentAuditStates = [
  "unknown",
  "skipped_local_mock",
] as const;

export type GatewayMockContentAuditState =
  (typeof gatewayMockContentAuditStates)[number];

/** One stored rule as the admin API returns it. */
export interface GatewayMockRule {
  id: string;
  keyword: string;
  /** The normalized keyword the gateway actually compares against; always present. */
  normalized_keyword: string;
  reply: string;
  enabled: boolean;
  /** The rule-content version the minimal hit events record. */
  updated_at: string | null;
}

export interface GatewayMockOperatorStatus {
  enabled: boolean;
  rules: GatewayMockRule[];
  /** Seeding is only meaningful for an empty set; the GET answer always carries both. */
  preset_available: boolean;
  preset_created: number;
}

/** One submitted rule. An empty `id` asks the server to mint one. */
export interface GatewayMockRuleInput {
  id: string;
  keyword: string;
  reply: string;
  enabled: boolean;
}

/** The whole rule set plus the global switch: the server replaces, never patches. */
export interface GatewayMockOperatorUpdateInput {
  enabled: boolean;
  rules: GatewayMockRuleInput[];
}

export interface GatewayMockPresetSeed {
  status: GatewayMockOperatorStatus;
  /** How many preset rules were written; `0` means the set was already non-empty. */
  created: number;
}

/**
 * One minimal hit event. Every field is a fact the gateway observed while it
 * answered locally; an empty string and a `0` id mean "not recorded", not an
 * empty value, and the panel renders them as such.
 */
export interface GatewayMockEvent {
  occurred_at: string;
  rule_id: string;
  rule_version: string;
  protocol: string;
  model: string;
  api_key_id: number;
  user_id: number;
  group_id: number;
  account_id: number;
  client_ip: string;
  trace_id: string;
  /**
   * Whether the prompt/content audit executed for this hit. `skipped_local_mock`
   * means the strict early match answered locally before either audit ran;
   * `unknown` covers a legacy record or an unrecognised value. This is an audit
   * action, never an audit verdict, and it carries no body.
   */
  content_audit_state: GatewayMockContentAuditState;
  /**
   * Always `null`: the stored deadline is an internal estimate, not the effective
   * cleanup time, so the server does not disclose it. Cleanup follows the current
   * usage-log retention policy. The key stays in the contract so "no deadline is
   * disclosed" is distinct from "the field is missing", and the panel never
   * renders a value here even if an older server sends one.
   */
  cleanup_after: string | null;
}

export interface GatewayMockEventPage {
  items: GatewayMockEvent[];
  total: number;
  page: number;
  page_size: number;
}

export interface GatewayMockEventListParams {
  page: number;
  page_size: number;
}

function record(value: unknown): Record<string, unknown> {
  if (value !== null && typeof value === "object" && !Array.isArray(value))
    return value as Record<string, unknown>;
  throw new Error("Invalid gateway mock record");
}

/** A count is a non-negative safe integer or it is not a count. */
function count(value: unknown, message: string): number {
  if (!Number.isSafeInteger(value) || (value as number) < 0)
    throw new Error(message);
  return value as number;
}

function boundedString(
  value: unknown,
  maxRunes: number,
  message: string,
): string {
  if (typeof value !== "string" || [...value].length > maxRunes)
    throw new Error(message);
  return value;
}

function eventString(
  value: unknown,
  maxLength: number,
  message: string,
): string {
  if (typeof value !== "string" || value.length > maxLength)
    throw new Error(message);
  return value;
}

function timestamp(value: unknown, message: string): string {
  if (
    typeof value !== "string" ||
    value.length > 80 ||
    Number.isNaN(Date.parse(value))
  )
    throw new Error(message);
  return value;
}

function optionalTimestamp(value: unknown, message: string): string | null {
  if (value == null) return null;
  return timestamp(value, message);
}

function normalizeGatewayMockRule(value: unknown): GatewayMockRule {
  const source = record(value);
  // The id is minted by the server or echoed back from a submitted rule; it is
  // never bounded here, because an id this view refused would blank the whole set.
  if (typeof source.id !== "string" || source.id === "")
    throw new Error("Invalid gateway mock rule id");
  if (typeof source.enabled !== "boolean")
    throw new Error("Invalid gateway mock rule switch");
  const keyword = boundedString(
    source.keyword,
    gatewayMockKeywordMaxLength,
    "Invalid gateway mock rule keyword",
  );
  return {
    id: source.id,
    keyword,
    normalized_keyword:
      typeof source.normalized_keyword === "string"
        ? source.normalized_keyword
        : keyword,
    reply: boundedString(
      source.reply,
      gatewayMockReplyMaxLength,
      "Invalid gateway mock rule reply",
    ),
    enabled: source.enabled,
    updated_at: optionalTimestamp(
      source.updated_at,
      "Invalid gateway mock rule version",
    ),
  };
}

/**
 * Normalizes the operator status. Only the switch, the rule set and the preset
 * counters are read: the reply text is part of the configured set the operator
 * just wrote, and nothing else is carried over.
 */
export function normalizeGatewayMockOperatorStatus(
  value: unknown,
): GatewayMockOperatorStatus {
  const source = record(value);
  if (typeof source.enabled !== "boolean")
    throw new Error("Invalid gateway mock switch state");
  if (!Array.isArray(source.rules) || source.rules.length > gatewayMockMaxRules)
    throw new Error("Invalid gateway mock rule list");
  if (typeof source.preset_available !== "boolean")
    throw new Error("Invalid gateway mock preset state");
  return {
    enabled: source.enabled,
    rules: source.rules.map(normalizeGatewayMockRule),
    preset_available: source.preset_available,
    preset_created: count(
      source.preset_created,
      "Invalid gateway mock preset count",
    ),
  };
}

export function normalizeGatewayMockPresetSeed(
  value: unknown,
): GatewayMockPresetSeed {
  const source = record(value);
  return {
    status: normalizeGatewayMockOperatorStatus(source.status),
    created: count(source.created, "Invalid gateway mock preset count"),
  };
}

const contentAuditStates = new Set<string>(gatewayMockContentAuditStates);

/**
 * Reads the content-audit state. A missing field (an older server that predates the
 * column) and a value outside the closed set both degrade to `unknown`; the raw
 * token is never kept, so an unrecognised state cannot be rendered as a fact and
 * old data is never guessed to be `skipped_local_mock`.
 */
function contentAuditState(value: unknown): GatewayMockContentAuditState {
  if (typeof value !== "string" || !contentAuditStates.has(value))
    return "unknown";
  return value as GatewayMockContentAuditState;
}

function normalizeGatewayMockEvent(value: unknown): GatewayMockEvent {
  const source = record(value);
  return {
    occurred_at: timestamp(
      source.occurred_at,
      "Invalid gateway mock event time",
    ),
    rule_id: eventString(
      source.rule_id,
      128,
      "Invalid gateway mock event rule",
    ),
    rule_version: eventString(
      source.rule_version,
      64,
      "Invalid gateway mock event rule version",
    ),
    protocol: eventString(
      source.protocol,
      32,
      "Invalid gateway mock event protocol",
    ),
    model: eventString(source.model, 256, "Invalid gateway mock event model"),
    api_key_id: count(source.api_key_id, "Invalid gateway mock event API key"),
    user_id: count(source.user_id, "Invalid gateway mock event user"),
    group_id: count(source.group_id, "Invalid gateway mock event group"),
    account_id: count(source.account_id, "Invalid gateway mock event account"),
    client_ip: eventString(
      source.client_ip,
      64,
      "Invalid gateway mock event client IP",
    ),
    trace_id: eventString(
      source.trace_id,
      64,
      "Invalid gateway mock event Trace id",
    ),
    // 缺失或闭集之外的取值一律降级为 unknown：旧服务不带该字段，不能被猜成"已确认未审计"。
    content_audit_state: contentAuditState(source.content_audit_state),
    // 只有确实记录到期限时才有值：null 是"没有记录到"，不是要被猜成某个日期的空值。
    cleanup_after: optionalTimestamp(
      source.cleanup_after,
      "Invalid gateway mock event cleanup time",
    ),
  };
}

/** An event the parser refuses is refused as a page: a half-read hit is not a hit. */
export function normalizeGatewayMockEventPage(
  value: unknown,
): GatewayMockEventPage {
  const source = record(value);
  if (
    !Array.isArray(source.items) ||
    source.items.length > gatewayMockEventMaxPageSize
  ) {
    throw new Error("Invalid gateway mock event page");
  }
  const page = count(source.page, "Invalid gateway mock event page number");
  const pageSize = count(
    source.page_size,
    "Invalid gateway mock event page size",
  );
  if (!page || !pageSize)
    throw new Error("Invalid gateway mock event pagination");
  return {
    items: source.items.map(normalizeGatewayMockEvent),
    total: count(source.total, "Invalid gateway mock event total"),
    page,
    page_size: pageSize,
  };
}
