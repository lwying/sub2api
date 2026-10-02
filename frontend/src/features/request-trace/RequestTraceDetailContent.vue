<template>
  <div
    v-if="loading"
    data-testid="trace-detail-loading"
    class="py-8 text-center text-sm text-gray-500"
  >
    {{ t("admin.requestTrace.list.loading") }}
  </div>
  <div
    v-else-if="failed"
    data-testid="trace-detail-failed"
    role="alert"
    class="py-6 text-sm text-red-600"
  >
    {{ t("admin.requestTrace.detail.loadFailed") }}
  </div>
  <div v-else-if="detail" data-testid="trace-detail" class="space-y-5 text-sm">
    <div
      v-if="metadataOnly"
      data-testid="trace-metadata-only"
      class="rounded-xl border border-primary-200 bg-primary-50 px-4 py-3 dark:border-primary-800 dark:bg-primary-900/20"
    >
      <p class="font-medium text-primary-800 dark:text-primary-200">
        {{ t("admin.requestTrace.detail.metadataOnly") }}
      </p>
      <p class="mt-1 text-xs text-primary-700 dark:text-primary-300">
        {{ t("admin.requestTrace.detail.captureBodyDisabledNote") }}
      </p>
    </div>
    <dl class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-2">
      <dt>{{ t("admin.requestTrace.list.traceId") }}</dt>
      <dd data-testid="trace-detail-id" class="break-all font-mono">
        {{ detail.trace_id }}
      </dd>
      <dt>{{ t("admin.requestTrace.list.route") }}</dt>
      <dd>{{ detail.inbound_endpoint }}</dd>
      <dt>{{ t("admin.requestTrace.list.user") }}</dt>
      <dd data-testid="trace-detail-user">
        {{ identityLabel(detail.user_id, detail.user_email) }}
      </dd>
      <dt>{{ t("admin.requestTrace.list.apiKey") }}</dt>
      <dd data-testid="trace-detail-key">
        {{ identityLabel(detail.api_key_id, detail.api_key_name) }}
      </dd>
      <dt>{{ t("admin.requestTrace.list.platform") }}</dt>
      <dd data-testid="trace-detail-platforms">
        {{
          detail.observed_platforms?.join(", ") ||
          t("admin.requestTrace.list.unknownValue")
        }}
      </dd>
      <dt>{{ t("admin.requestTrace.list.status") }}</dt>
      <dd>{{ detail.client_status || "—" }}</dd>
      <dt>{{ t("admin.requestTrace.list.usage") }}</dt>
      <dd v-if="detail.usage_log_id" class="font-mono">
        <RouterLink
          :to="usageLink"
          data-testid="trace-detail-usage-link"
          class="text-primary-600 underline decoration-dashed underline-offset-2 hover:text-primary-700 dark:text-primary-400"
          :title="t('admin.requestTrace.detail.usageMeteringNote')"
          >#{{ detail.usage_log_id }}</RouterLink
        >
      </dd>
      <dd v-else>{{ t("admin.requestTrace.list.usageAbsent") }}</dd>
    </dl>
    <p
      v-if="detail.usage_log_id"
      data-testid="trace-detail-cleanup-rule"
      class="text-xs text-gray-500"
    >
      {{ t("admin.requestTrace.detail.followsUsage") }}
    </p>
    <p
      v-else-if="detail.cleanup_after"
      data-testid="trace-detail-cleanup-rule"
      class="text-xs text-gray-500"
    >
      {{
        t("admin.requestTrace.detail.plannedCleanup", {
          date: formatDate(detail.cleanup_after),
        })
      }}
    </p>
    <section
      v-for="stage in detail.stages"
      :key="stage.ordinal"
      :data-testid="`trace-stage-${stage.ordinal}`"
      class="rounded-lg border border-gray-200 p-4 dark:border-dark-700"
    >
      <div class="flex flex-wrap items-center justify-between gap-2">
        <h4 class="font-semibold">
          {{ t("admin.requestTrace.detail.stage") }} {{ stage.ordinal }} ·
          <span data-testid="trace-stage-name">{{
            stageLabel(t, stage.stage)
          }}</span>
        </h4>
        <span data-testid="trace-stage-state" class="font-mono text-xs">{{
          stageStateLabel(t, stage.state)
        }}</span>
      </div>
      <p
        v-if="
          stage.state === 'not_observed' &&
          isBodyStage(stage.stage) &&
          stage.reason !== 'capture_body_disabled'
        "
        data-testid="trace-stage-not-observed"
        class="mt-2 text-sm text-amber-700 dark:text-amber-300"
      >
        {{ t("admin.requestTrace.detail.notObserved") }}
      </p>
      <dl class="mt-2 grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-xs">
        <dt>{{ t("admin.requestTrace.detail.reason") }}</dt>
        <dd
          data-testid="trace-stage-reason"
          class="break-all font-mono"
          :title="stage.reason"
        >
          {{ stageReasonLabel(t, stage.reason) }}
        </dd>
        <template v-if="stage.attempt_index > 0">
          <dt>{{ t("admin.requestTrace.detail.attempt") }}</dt>
          <dd
            class="font-mono"
            :data-testid="`trace-upstream-attempt-${stage.attempt_index}`"
          >
            {{ stage.attempt_index }}
          </dd>
        </template>
        <template v-if="stage.view_name">
          <dt>{{ t("admin.requestTrace.detail.source") }}</dt>
          <dd data-testid="trace-stage-view">
            {{ stageViewLabel(t, stage.view_name) }}
          </dd>
        </template>
        <dt>{{ t("admin.requestTrace.detail.observed") }}</dt>
        <dd>{{ stage.observed_bytes }}</dd>
        <dt>{{ t("admin.requestTrace.detail.retained") }}</dt>
        <dd>{{ stage.retained_bytes }}</dd>
        <template v-if="stage.dropped_events">
          <dt>{{ t("admin.requestTrace.detail.droppedEvents") }}</dt>
          <dd>{{ stage.dropped_events }}</dd>
        </template>
      </dl>
      <p
        v-if="
          stage.redaction_unverified || stage.state === 'redaction_unverified'
        "
        data-testid="trace-risk-warning"
        role="alert"
        class="mt-2 text-xs font-medium text-red-700 dark:text-red-300"
      >
        {{ t("admin.requestTrace.detail.risk") }}
      </p>
      <div v-if="stage.facts" data-testid="trace-facts" class="mt-3">
        <h5 class="text-xs text-gray-500 dark:text-dark-400">
          {{ t("admin.requestTrace.detail.facts") }}
        </h5>
        <dl
          class="mt-1 grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-xs"
        >
          <template v-if="stage.facts.method">
            <dt>{{ t("admin.requestTrace.detail.method") }}</dt>
            <dd data-testid="trace-fact-method" class="break-all font-mono">
              {{ stage.facts.method }}
            </dd>
          </template>
          <template v-if="stage.facts.url">
            <dt>{{ t("admin.requestTrace.detail.url") }}</dt>
            <dd data-testid="trace-fact-url" class="break-all font-mono">
              {{ stage.facts.url }}
            </dd>
          </template>
          <template v-else-if="stage.facts.url_omitted">
            <dt>{{ t("admin.requestTrace.detail.url") }}</dt>
            <dd data-testid="trace-fact-url-omitted">
              {{ t("admin.requestTrace.detail.urlOmitted") }}
            </dd>
          </template>
          <template v-if="stage.facts.account_id !== null">
            <dt>{{ t("admin.requestTrace.detail.account") }}</dt>
            <dd data-testid="trace-fact-account" class="font-mono">
              #{{ stage.facts.account_id }}
            </dd>
          </template>
          <template v-if="stage.facts.model">
            <dt>{{ t("admin.requestTrace.detail.model") }}</dt>
            <dd data-testid="trace-fact-model" class="break-all font-mono">
              {{ stage.facts.model }}
            </dd>
          </template>
          <template v-if="stage.facts.platform">
            <dt>{{ t("admin.requestTrace.list.platform") }}</dt>
            <dd data-testid="trace-fact-platform" class="break-all font-mono">
              {{ stage.facts.platform }}
            </dd>
          </template>
          <template v-if="stage.facts.protocol">
            <dt>{{ t("admin.requestTrace.detail.protocol") }}</dt>
            <dd data-testid="trace-fact-protocol" class="break-all font-mono">
              {{ stage.facts.protocol }}
            </dd>
          </template>
          <template v-if="stage.facts.value_protocol">
            <dt>{{ t("admin.requestTrace.detail.valueProtocol") }}</dt>
            <dd
              data-testid="trace-fact-value-protocol"
              class="break-all font-mono"
            >
              {{ stage.facts.value_protocol }}
            </dd>
          </template>
          <template v-if="stage.facts.status !== null">
            <dt>{{ t("admin.requestTrace.detail.upstreamStatus") }}</dt>
            <dd data-testid="trace-fact-status" class="font-mono">
              {{ stage.facts.status }}
            </dd>
          </template>
          <template v-if="stage.facts.started_at">
            <dt>{{ t("admin.requestTrace.detail.startedAt") }}</dt>
            <dd data-testid="trace-fact-started-at">
              {{ formatDate(stage.facts.started_at) }}
            </dd>
          </template>
          <template v-if="stage.facts.ended_at">
            <dt>{{ t("admin.requestTrace.detail.endedAt") }}</dt>
            <dd data-testid="trace-fact-ended-at">
              {{ formatDate(stage.facts.ended_at) }}
            </dd>
          </template>
        </dl>
        <div v-if="hasHeaders(stage.facts.request_headers)" class="mt-2">
          <h6 class="text-xs text-gray-500 dark:text-dark-400">
            {{ t("admin.requestTrace.detail.requestHeaders") }}
          </h6>
          <pre
            data-testid="trace-fact-request-headers"
            class="mt-1 max-h-48 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-gray-50 p-2 text-xs dark:bg-dark-900"
            >{{ headerText(stage.facts.request_headers) }}</pre>
        </div>
        <div
          v-if="stage.facts.request_headers_omitted"
          data-testid="trace-fact-request-headers-omitted"
          class="mt-1 text-xs text-gray-500 dark:text-dark-400"
        >
          {{ t("admin.requestTrace.detail.headersOmitted") }}
          <span class="font-mono">{{
            stage.facts.request_headers_omitted
          }}</span>
        </div>
        <div v-if="hasHeaders(stage.facts.response_headers)" class="mt-2">
          <h6 class="text-xs text-gray-500 dark:text-dark-400">
            {{ t("admin.requestTrace.detail.responseHeaders") }}
          </h6>
          <pre
            data-testid="trace-fact-response-headers"
            class="mt-1 max-h-48 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-gray-50 p-2 text-xs dark:bg-dark-900"
            >{{ headerText(stage.facts.response_headers) }}</pre>
        </div>
        <div
          v-if="stage.facts.response_headers_omitted"
          data-testid="trace-fact-response-headers-omitted"
          class="mt-1 text-xs text-gray-500 dark:text-dark-400"
        >
          {{ t("admin.requestTrace.detail.headersOmitted") }}
          <span class="font-mono">{{
            stage.facts.response_headers_omitted
          }}</span>
        </div>
        <p
          v-if="
            hasHeaders(stage.facts.request_headers) ||
            hasHeaders(stage.facts.response_headers)
          "
          class="mt-1 text-xs text-gray-500 dark:text-dark-400"
        >
          {{ t("admin.requestTrace.detail.redactedNote") }}
        </p>
        <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
          {{ t("admin.requestTrace.detail.factsNote") }}
        </p>
      </div>
      <div v-if="stage.decision" data-testid="trace-decision" class="mt-3">
        <h5 class="text-xs text-gray-500 dark:text-dark-400">
          {{ t("admin.requestTrace.detail.decision.title") }}
        </h5>
        <dl
          class="mt-1 grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-xs"
        >
          <dt>{{ t("admin.requestTrace.detail.decision.kind") }}</dt>
          <dd data-testid="trace-decision-kind" class="break-all font-mono">
            {{
              t(
                `admin.requestTrace.detail.decision.kindLabel.${stage.decision.decision}`,
              )
            }}
          </dd>
          <dt>{{ t("admin.requestTrace.detail.decision.outcome") }}</dt>
          <dd data-testid="trace-decision-outcome" class="break-all font-mono">
            {{
              t(
                `admin.requestTrace.detail.decision.outcomeLabel.${stage.decision.outcome}`,
              )
            }}
          </dd>
          <dt>{{ t("admin.requestTrace.detail.decision.source") }}</dt>
          <dd data-testid="trace-decision-source" class="break-all font-mono">
            {{
              t(
                `admin.requestTrace.detail.decision.sourceLabel.${stage.decision.source}`,
              )
            }}
          </dd>
          <dt>{{ t("admin.requestTrace.detail.decision.sequence") }}</dt>
          <dd data-testid="trace-decision-sequence" class="font-mono">
            {{ stage.decision.sequence }}
          </dd>
          <template v-if="stage.decision.model_from">
            <dt>{{ t("admin.requestTrace.detail.decision.modelFrom") }}</dt>
            <dd
              data-testid="trace-decision-model-from"
              class="break-all font-mono"
            >
              {{ stage.decision.model_from }}
            </dd>
          </template>
          <template v-if="stage.decision.model_to">
            <dt>{{ t("admin.requestTrace.detail.decision.modelTo") }}</dt>
            <dd
              data-testid="trace-decision-model-to"
              class="break-all font-mono"
            >
              {{ stage.decision.model_to }}
            </dd>
          </template>
          <template v-if="stage.decision.protocol_from">
            <dt>{{ t("admin.requestTrace.detail.decision.protocolFrom") }}</dt>
            <dd
              data-testid="trace-decision-protocol-from"
              class="break-all font-mono"
            >
              {{ stage.decision.protocol_from }}
            </dd>
          </template>
          <template v-if="stage.decision.protocol_to">
            <dt>{{ t("admin.requestTrace.detail.decision.protocolTo") }}</dt>
            <dd
              data-testid="trace-decision-protocol-to"
              class="break-all font-mono"
            >
              {{ stage.decision.protocol_to }}
            </dd>
          </template>
          <template v-if="stage.decision.account_id !== null">
            <dt>{{ t("admin.requestTrace.detail.decision.account") }}</dt>
            <dd data-testid="trace-decision-account" class="font-mono">
              #{{ stage.decision.account_id }}
            </dd>
          </template>
          <template v-if="stage.decision.decided_at">
            <dt>{{ t("admin.requestTrace.detail.decision.decidedAt") }}</dt>
            <dd data-testid="trace-decision-decided-at">
              {{ formatDate(stage.decision.decided_at) }}
            </dd>
          </template>
        </dl>
        <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
          {{ t("admin.requestTrace.detail.decision.note") }}
        </p>
      </div>
      <div
        v-if="stage.payload_text !== undefined"
        class="mt-3"
        data-testid="trace-retained-text"
      >
        <h5 class="text-xs text-gray-500 dark:text-dark-400">
          {{ t("admin.requestTrace.detail.body") }}
        </h5>
        <pre
          class="mt-1 max-h-96 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-gray-50 p-3 text-xs dark:bg-dark-900"
          >{{ stage.payload_text }}</pre>
      </div>
    </section>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import { getTrace } from "./api";
import {
  stageLabel,
  stageReasonLabel,
  stageStateLabel,
  stageViewLabel,
} from "./labels";
import type { RequestTraceDetail } from "./types";

const props = defineProps<{ show: boolean; traceId: string | null }>();
const { t } = useI18n();
const detail = ref<RequestTraceDetail | null>(null);
const metadataOnly = computed(
  () =>
    detail.value?.stages.some(
      (stage) => stage.reason === "capture_body_disabled",
    ) ?? false,
);
const loading = ref(false);
const failed = ref(false);
let revision = 0;
let controller: AbortController | null = null;

function identityLabel(id: number | null, name: string | null): string {
  if (id == null) return t("admin.requestTrace.list.unknownValue");
  return name ? `${name} (#${id})` : `#${id}`;
}

function formatDate(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}

/**
 * The Trace envelope carries a usage id but no token or cost figure: metering
 * facts belong to the usage record, not to a request payload, and the Trace must
 * not invent them from retained text. The id therefore links to the existing
 * admin usage page, which is the session-gated owner of those facts, and carries
 * the id so that page opens on that exact record instead of the unfiltered list.
 * Only the id travels with the link: no Trace body, no plaintext.
 */
const usageLink = computed(() => {
  const usageLogId = detail.value?.usage_log_id;
  return usageLogId
    ? { path: "/admin/usage", query: { usage_log_id: String(usageLogId) } }
    : { path: "/admin/usage" };
});

function hasHeaders(headers: Record<string, string[]>): boolean {
  return Object.keys(headers).length > 0;
}

/** Rendered as text inside a <pre>; header names and values are never markup. */
function headerText(headers: Record<string, string[]>): string {
  return Object.keys(headers)
    .map((name) => `${name}: ${headers[name].join(", ")}`)
    .join("\n");
}

/**
 * The stages that carry a client or upstream body. The unobserved-body note claims a
 * body was not observed, so only these stages may show it: `client_metadata` and
 * `wire_attempt` report observed facts instead of a body, and a decision stage reports
 * a gateway decision, so their `not_observed` describes a body they never carry. An
 * unknown stage is left unlabelled rather than told it is missing a body.
 */
const bodyStages = new Set([
  "client_entry",
  "wire_request",
  "upstream_response",
  "client_response",
]);

function isBodyStage(stage: string): boolean {
  return bodyStages.has(stage);
}

watch(
  () => [props.show, props.traceId] as const,
  ([show, id]) => {
    revision += 1;
    controller?.abort();
    controller = null;
    detail.value = null;
    failed.value = false;
    loading.value = false;
    if (!show || !id) return;
    const current = revision;
    const requestController = new AbortController();
    controller = requestController;
    loading.value = true;
    void getTrace(id, { signal: requestController.signal })
      .then((result) => {
        if (revision === current && props.show && props.traceId === id)
          detail.value = result;
      })
      .catch(() => {
        if (revision === current && props.show && props.traceId === id)
          failed.value = true;
      })
      .finally(() => {
        if (revision === current) loading.value = false;
      });
  },
  { immediate: true },
);

onBeforeUnmount(() => {
  revision += 1;
  controller?.abort();
  detail.value = null;
});
</script>
