<template>
  <section
    class="rounded-xl border border-gray-200 bg-white p-4 dark:border-dark-700 dark:bg-dark-800"
    data-testid="request-trace-export-settings"
  >
    <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
      {{ t("admin.requestTrace.export.settings.title") }}
    </h2>
    <p class="mt-1 text-sm text-gray-500 dark:text-dark-300">
      {{ t("admin.requestTrace.export.settings.description") }}
    </p>
    <p
      class="mt-2 text-xs text-gray-500 dark:text-dark-300"
      data-testid="request-trace-export-settings-session-note"
    >
      {{ t("admin.requestTrace.export.settings.sessionOnly") }}
    </p>

    <!-- Risk acknowledgement: the statement is the server's own text, and the
         admin retypes it verbatim. A mismatch is not a submission. -->
    <div
      class="mt-4 border-t border-gray-200 pt-4 dark:border-dark-700"
      data-testid="request-trace-export-risk"
    >
      <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
        {{ t("admin.requestTrace.export.settings.risk.title") }}
      </h3>
      <template v-if="risk">
        <dl
          class="mt-2 grid grid-cols-[max-content_1fr] items-baseline gap-x-4 gap-y-2 text-sm"
        >
          <dt class="text-gray-500 dark:text-dark-300">
            {{ t("admin.requestTrace.export.settings.risk.state") }}
          </dt>
          <dd
            data-testid="request-trace-export-risk-state"
            :data-state="risk.acknowledged ? 'acknowledged' : 'pending'"
          >
            {{
              risk.acknowledged
                ? t("admin.requestTrace.export.settings.risk.acknowledged")
                : t("admin.requestTrace.export.settings.risk.pending")
            }}
          </dd>
          <dt class="text-gray-500 dark:text-dark-300">
            {{ t("admin.requestTrace.export.settings.risk.version") }}
          </dt>
          <dd class="font-mono" data-testid="request-trace-export-risk-version">
            {{ risk.version }}
          </dd>
          <template v-if="risk.accepted_at">
            <dt class="text-gray-500 dark:text-dark-300">
              {{ t("admin.requestTrace.export.settings.risk.acceptedAt") }}
            </dt>
            <dd
              class="font-mono"
              data-testid="request-trace-export-risk-accepted-at"
            >
              {{ formatDate(risk.accepted_at) }}
            </dd>
          </template>
        </dl>

        <label
          class="mt-3 block text-sm"
          for="request-trace-export-risk-language"
          >{{ t("admin.requestTrace.export.settings.risk.language") }}</label
        >
        <select
          id="request-trace-export-risk-language"
          v-model="language"
          data-testid="request-trace-export-risk-language"
          class="mt-1 rounded-lg border p-2 text-sm dark:bg-dark-900"
        >
          <option value="en">{{ ackLanguageLabel(t, "en") }}</option>
          <option value="zh">{{ ackLanguageLabel(t, "zh") }}</option>
        </select>
        <p class="mt-2 text-sm">
          {{ t("admin.requestTrace.export.settings.risk.requiredPhrase") }}
        </p>
        <p
          class="mt-1 rounded-lg border p-3 text-xs"
          data-testid="request-trace-export-risk-phrase"
        >
          {{ requiredPhrase }}
        </p>
        <label
          class="mt-3 block text-sm"
          for="request-trace-export-risk-input"
          >{{ t("admin.requestTrace.export.settings.risk.typePhrase") }}</label
        >
        <textarea
          id="request-trace-export-risk-input"
          v-model="typedPhrase"
          data-testid="request-trace-export-risk-input"
          rows="3"
          autocomplete="off"
          spellcheck="false"
          class="mt-1 w-full rounded-lg border p-3 text-xs dark:bg-dark-900"
        />
        <button
          type="button"
          class="btn btn-primary mt-3"
          data-testid="request-trace-export-risk-submit"
          :disabled="!canAcknowledge"
          @click="acknowledge"
        >
          {{
            acknowledging
              ? t("admin.requestTrace.export.settings.risk.submitting")
              : t("admin.requestTrace.export.settings.risk.submit")
          }}
        </button>
        <p
          v-if="acknowledgeError"
          role="alert"
          class="mt-2 text-xs text-red-700 dark:text-red-300"
          data-testid="request-trace-export-risk-error"
        >
          {{ t("admin.requestTrace.export.settings.risk.failed") }}
        </p>
        <p
          v-else-if="acknowledgedNow"
          role="status"
          class="mt-2 text-xs text-green-700 dark:text-green-300"
          data-testid="request-trace-export-risk-saved"
        >
          {{ t("admin.requestTrace.export.settings.risk.saved") }}
        </p>
      </template>
      <p
        v-else-if="riskFailed"
        role="alert"
        class="mt-2 text-xs text-red-700 dark:text-red-300"
        data-testid="request-trace-export-risk-unavailable"
      >
        {{ t("admin.requestTrace.export.settings.risk.unavailable") }}
      </p>
      <p
        v-else
        role="status"
        class="mt-2 text-xs text-gray-500 dark:text-dark-300"
        data-testid="request-trace-export-risk-loading"
      >
        {{ t("common.loading") }}
      </p>
    </div>

    <!-- Task caps: finite by construction. There is no "unlimited" value to choose. -->
    <div
      class="mt-4 border-t border-gray-200 pt-4 dark:border-dark-700"
      data-testid="request-trace-export-limits"
    >
      <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
        {{ t("admin.requestTrace.export.settings.limits.title") }}
      </h3>
      <p class="mt-1 text-xs text-gray-500 dark:text-dark-300">
        {{ t("admin.requestTrace.export.settings.limits.description") }}
      </p>
      <template v-if="limits">
        <p
          class="mt-2 text-xs text-gray-500 dark:text-dark-300"
          data-testid="request-trace-export-limits-provenance"
        >
          {{
            limits.configured
              ? t("admin.requestTrace.export.settings.limits.configured")
              : t("admin.requestTrace.export.settings.limits.defaults")
          }}
        </p>
        <div class="mt-3 grid gap-3 md:grid-cols-2 xl:grid-cols-3">
          <label
            v-for="field in fields"
            :key="field.key"
            class="space-y-1 text-xs text-gray-600 dark:text-dark-300"
          >
            <span>{{
              t(`admin.requestTrace.export.settings.limits.${field.key}`)
            }}</span>
            <input
              v-model.trim="draft[field.key]"
              :data-testid="`request-trace-export-limit-${field.key}`"
              class="input w-full font-mono"
              inputmode="numeric"
              autocomplete="off"
            />
            <span
              :data-testid="`request-trace-export-limit-${field.key}-hint`"
              >{{ field.hint }}</span
            >
          </label>
        </div>
        <button
          type="button"
          class="btn btn-primary mt-3"
          data-testid="request-trace-export-limits-save"
          :disabled="!canSave"
          @click="save"
        >
          {{
            saving
              ? t("admin.requestTrace.export.settings.limits.saving")
              : t("admin.requestTrace.export.settings.limits.save")
          }}
        </button>
        <p
          v-if="limitsError"
          role="alert"
          class="mt-2 text-xs text-red-700 dark:text-red-300"
          data-testid="request-trace-export-limits-error"
        >
          {{ t("admin.requestTrace.export.settings.limits.invalid") }}
        </p>
        <p
          v-else-if="limitsSaved"
          role="status"
          class="mt-2 text-xs text-green-700 dark:text-green-300"
          data-testid="request-trace-export-limits-saved"
        >
          {{ t("admin.requestTrace.export.settings.limits.saved") }}
        </p>
      </template>
      <p
        v-else-if="limitsFailed"
        role="alert"
        class="mt-2 text-xs text-red-700 dark:text-red-300"
        data-testid="request-trace-export-limits-unavailable"
      >
        {{ t("admin.requestTrace.export.settings.limits.unavailable") }}
      </p>
      <p
        v-else
        role="status"
        class="mt-2 text-xs text-gray-500 dark:text-dark-300"
        data-testid="request-trace-export-limits-loading"
      >
        {{ t("common.loading") }}
      </p>
    </div>
  </section>
</template>

<script setup lang="ts">
import {
  computed,
  onBeforeUnmount,
  onMounted,
  reactive,
  ref,
  watch,
} from "vue";
import { useI18n } from "vue-i18n";
import { getLocale } from "@/i18n";
import {
  acknowledgeTraceExportRisk,
  getTraceExportLimits,
  getTraceExportRisk,
  updateTraceExportLimits,
} from "./api";
import { ackLanguageLabel } from "./labels";
import {
  requestTraceExportLimitBounds,
  type RequestTraceExportLimits,
  type RequestTraceExportRisk,
  type TraceAckLanguage,
} from "./types";

type LimitKey =
  | "max_rows"
  | "max_bytes"
  | "max_runtime_seconds"
  | "max_shard_rows"
  | "max_shard_bytes";

const { t } = useI18n();
const language = ref<TraceAckLanguage>(getLocale() === "zh" ? "zh" : "en");
const typedPhrase = ref("");
const acknowledging = ref(false);
const acknowledgeError = ref(false);
const acknowledgedNow = ref(false);
const risk = ref<RequestTraceExportRisk | null>(null);
const riskFailed = ref(false);
const limits = ref<RequestTraceExportLimits | null>(null);
const limitsFailed = ref(false);
const limitsError = ref(false);
const limitsSaved = ref(false);
const saving = ref(false);
const draft = reactive<Record<LimitKey, string>>({
  max_rows: "",
  max_bytes: "",
  max_runtime_seconds: "",
  max_shard_rows: "",
  max_shard_bytes: "",
});
// The two reads are independent, so each keeps its own revision: one finishing
// must never discard the other's verdict.
let riskRevision = 0;
let limitsRevision = 0;
let riskController: AbortController | null = null;
let limitsController: AbortController | null = null;

function formatDate(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}

/** The statement to retype, in the selected language, exactly as the server sends it. */
const requiredPhrase = computed(() =>
  language.value === "zh"
    ? (risk.value?.phrase_zh ?? "")
    : (risk.value?.phrase_en ?? ""),
);

/**
 * A mismatch is not a submission: the server compares the statement byte for
 * byte, so the button stays disabled until the operator has reproduced it.
 */
const canAcknowledge = computed(
  () =>
    !!risk.value &&
    !acknowledging.value &&
    requiredPhrase.value !== "" &&
    typedPhrase.value.trim() === requiredPhrase.value,
);

const fields = computed(() => {
  const bounds = requestTraceExportLimitBounds;
  const current = limits.value;
  const shardRowMax = current
    ? String(current.max_rows)
    : String(bounds.max_rows.max);
  const shardByteMax = current
    ? String(current.max_bytes)
    : String(bounds.max_bytes.max);
  return [
    {
      key: "max_rows" as const,
      hint: t("admin.requestTrace.export.settings.limits.range", {
        min: bounds.max_rows.min,
        max: bounds.max_rows.max,
      }),
    },
    {
      key: "max_bytes" as const,
      hint: t("admin.requestTrace.export.settings.limits.range", {
        min: bounds.max_bytes.min,
        max: bounds.max_bytes.max,
      }),
    },
    {
      key: "max_runtime_seconds" as const,
      hint: t("admin.requestTrace.export.settings.limits.range", {
        min: bounds.max_runtime_seconds.min,
        max: bounds.max_runtime_seconds.max,
      }),
    },
    {
      key: "max_shard_rows" as const,
      hint: t("admin.requestTrace.export.settings.limits.shardRange", {
        min: bounds.max_shard_rows.min,
        max: shardRowMax,
      }),
    },
    {
      key: "max_shard_bytes" as const,
      hint: t("admin.requestTrace.export.settings.limits.shardRange", {
        min: bounds.max_shard_bytes.min,
        max: shardByteMax,
      }),
    },
  ];
});

/** Set while the draft is being replaced by server values, so a re-sync is not an edit. */
let syncingLimits = false;

/** Reads the stored caps into the draft. Only complete server values are applied. */
function syncLimits(next: RequestTraceExportLimits) {
  syncingLimits = true;
  draft.max_rows = String(next.max_rows);
  draft.max_bytes = String(next.max_bytes);
  draft.max_runtime_seconds = String(next.max_runtime_seconds);
  draft.max_shard_rows = String(next.max_shard_rows);
  draft.max_shard_bytes = String(next.max_shard_bytes);
  syncingLimits = false;
}

/** A cap is a positive integer inside the range the server clamps to; anything else is not a cap. */
function parseLimit(raw: string, min: number, max: number): number | null {
  if (!/^[0-9]+$/.test(raw)) return null;
  const value = Number(raw);
  if (!Number.isSafeInteger(value) || value < min || value > max) return null;
  return value;
}

/**
 * The five caps as one submission. They are validated together because the shard
 * caps are bounded by the task caps, so a single field can be legal on its own
 * and illegal in the set. Every field is sent: the server binds a missing one to
 * the minimum rather than leaving it alone.
 */
function limitsPayload(): RequestTraceExportLimits | null {
  const bounds = requestTraceExportLimitBounds;
  const maxRows = parseLimit(
    draft.max_rows,
    bounds.max_rows.min,
    bounds.max_rows.max,
  );
  const maxBytes = parseLimit(
    draft.max_bytes,
    bounds.max_bytes.min,
    bounds.max_bytes.max,
  );
  const runtime = parseLimit(
    draft.max_runtime_seconds,
    bounds.max_runtime_seconds.min,
    bounds.max_runtime_seconds.max,
  );
  const shardRows =
    maxRows === null
      ? null
      : parseLimit(draft.max_shard_rows, bounds.max_shard_rows.min, maxRows);
  const shardBytes =
    maxBytes === null
      ? null
      : parseLimit(draft.max_shard_bytes, bounds.max_shard_bytes.min, maxBytes);
  if (
    maxRows === null ||
    maxBytes === null ||
    runtime === null ||
    shardRows === null ||
    shardBytes === null
  )
    return null;
  return {
    max_rows: maxRows,
    max_bytes: maxBytes,
    max_runtime_seconds: runtime,
    max_shard_rows: shardRows,
    max_shard_bytes: shardBytes,
    configured: true,
  };
}

// The button stays available while a cap is out of range: refusing the save with
// a stated reason is clearer than a button that silently cannot be pressed.
const canSave = computed(() => !!limits.value && !saving.value);

// A confirmation lasts only until the operator edits the draft again — and a
// re-sync from the server is not an edit, so it never clears one that was just
// earned. Both are synchronous so the final state is decided in call order.
watch(
  draft,
  () => {
    if (!syncingLimits) limitsSaved.value = false;
  },
  { flush: "sync" },
);
watch(
  typedPhrase,
  () => {
    acknowledgedNow.value = false;
  },
  { flush: "sync" },
);

async function loadRisk() {
  const current = ++riskRevision;
  riskController?.abort();
  const controller = new AbortController();
  riskController = controller;
  try {
    const next = await getTraceExportRisk({ signal: controller.signal });
    if (current !== riskRevision) return;
    risk.value = next;
    riskFailed.value = false;
  } catch {
    if (current !== riskRevision) return;
    risk.value = null;
    riskFailed.value = true;
  }
}

async function acknowledge() {
  if (!canAcknowledge.value) return;
  acknowledging.value = true;
  acknowledgeError.value = false;
  acknowledgedNow.value = false;
  try {
    const next = await acknowledgeTraceExportRisk({
      language: language.value,
      phrase: typedPhrase.value.trim(),
    });
    risk.value = next;
    typedPhrase.value = "";
    acknowledgedNow.value = true;
  } catch {
    acknowledgeError.value = true;
  } finally {
    acknowledging.value = false;
  }
}

async function loadLimits() {
  const current = ++limitsRevision;
  limitsController?.abort();
  const controller = new AbortController();
  limitsController = controller;
  try {
    const next = await getTraceExportLimits({ signal: controller.signal });
    if (current !== limitsRevision) return;
    limits.value = next;
    syncLimits(next);
    limitsFailed.value = false;
  } catch {
    if (current !== limitsRevision) return;
    limits.value = null;
    limitsFailed.value = true;
  }
}

async function save() {
  if (!canSave.value) return;
  const payload = limitsPayload();
  if (payload === null) {
    limitsError.value = true;
    return;
  }
  saving.value = true;
  limitsError.value = false;
  limitsSaved.value = false;
  try {
    const next = await updateTraceExportLimits(payload);
    limits.value = next;
    // The server clamps into the allowed range: showing what it accepted is the
    // only honest way to say what a new task will snapshot.
    syncLimits(next);
    limitsSaved.value = true;
  } catch {
    limitsError.value = true;
  } finally {
    saving.value = false;
  }
}

onMounted(() => {
  void loadRisk();
  void loadLimits();
});
onBeforeUnmount(() => {
  riskRevision += 1;
  limitsRevision += 1;
  riskController?.abort();
  limitsController?.abort();
});
</script>
