<template>
  <section class="card" data-testid="gateway-mock-settings">
    <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
        {{ t("admin.gatewayMock.title") }}
      </h2>
      <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
        {{ t("admin.gatewayMock.description") }}
      </p>
    </div>
    <div class="space-y-4 p-6">
      <p v-if="loading && !status" data-testid="gateway-mock-loading">
        {{ t("admin.gatewayMock.loading") }}
      </p>
      <p
        v-else-if="!status"
        role="alert"
        data-testid="gateway-mock-unavailable"
      >
        {{ t("admin.gatewayMock.unavailable") }}
      </p>
      <template v-else>
        <dl class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-2 text-sm">
          <dt>{{ t("admin.gatewayMock.stateLabel") }}</dt>
          <dd
            data-testid="gateway-mock-state"
            :data-state="status.enabled ? 'on' : 'off'"
          >
            {{
              status.enabled
                ? t("admin.gatewayMock.on")
                : t("admin.gatewayMock.off")
            }}
          </dd>
        </dl>
        <p
          class="text-xs text-gray-500 dark:text-gray-400"
          data-testid="gateway-mock-state-note"
        >
          {{ t("admin.gatewayMock.stateNote") }}
        </p>

        <label
          class="flex items-center gap-3 text-sm text-gray-700 dark:text-gray-300"
          for="gateway-mock-switch"
        >
          <input
            id="gateway-mock-switch"
            v-model="enabled"
            type="checkbox"
            data-testid="gateway-mock-switch"
          />
          <span>{{ t("admin.gatewayMock.switchLabel") }}</span>
        </label>
        <p
          class="text-xs text-gray-500 dark:text-gray-400"
          data-testid="gateway-mock-switch-note"
        >
          {{ t("admin.gatewayMock.switchNote") }}
        </p>

        <div class="border-t pt-4 dark:border-dark-700">
          <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
            {{ t("admin.gatewayMock.rules.title") }}
          </h3>
          <p
            class="mt-1 text-xs text-gray-500 dark:text-gray-400"
            data-testid="gateway-mock-rules-note"
          >
            {{ t("admin.gatewayMock.rules.description") }}
          </p>
          <p
            v-if="!rules.length"
            class="mt-3 text-sm text-gray-500 dark:text-gray-400"
            data-testid="gateway-mock-rules-empty"
          >
            {{ t("admin.gatewayMock.rules.empty") }}
          </p>
          <div v-else class="mt-3 space-y-3">
            <div
              v-for="(rule, index) in rules"
              :key="index"
              class="rounded-lg border border-gray-200 p-3 dark:border-dark-700"
              data-testid="gateway-mock-rule-row"
            >
              <div class="flex flex-wrap items-end gap-3">
                <label
                  class="min-w-[12rem] flex-1 text-xs text-gray-600 dark:text-dark-300"
                >
                  <span>{{ t("admin.gatewayMock.rules.keyword") }}</span>
                  <input
                    v-model="rule.keyword"
                    data-testid="gateway-mock-rule-keyword"
                    class="input mt-1 w-full"
                    autocomplete="off"
                  />
                </label>
                <label
                  class="flex items-center gap-2 text-xs text-gray-600 dark:text-dark-300"
                >
                  <input
                    v-model="rule.enabled"
                    type="checkbox"
                    data-testid="gateway-mock-rule-enabled"
                  />
                  <span data-testid="gateway-mock-rule-enabled-label">
                    {{
                      rule.enabled
                        ? t("admin.gatewayMock.rules.enabledOn")
                        : t("admin.gatewayMock.rules.enabledOff")
                    }}
                  </span>
                </label>
                <button
                  type="button"
                  class="btn btn-secondary btn-sm"
                  data-testid="gateway-mock-rule-remove"
                  @click="removeRule(index)"
                >
                  {{ t("admin.gatewayMock.rules.remove") }}
                </button>
              </div>
              <label
                class="mt-2 block text-xs text-gray-600 dark:text-dark-300"
              >
                <span>{{ t("admin.gatewayMock.rules.reply") }}</span>
                <textarea
                  v-model="rule.reply"
                  data-testid="gateway-mock-rule-reply"
                  rows="2"
                  class="mt-1 w-full rounded-lg border border-gray-200 p-2 text-xs dark:border-dark-700 dark:bg-dark-900"
                />
              </label>
            </div>
          </div>
          <div class="mt-3 flex flex-wrap gap-2">
            <button
              type="button"
              class="btn btn-secondary btn-sm"
              data-testid="gateway-mock-rule-add"
              @click="addRule"
            >
              {{ t("admin.gatewayMock.rules.add") }}
            </button>
            <button
              type="button"
              class="btn btn-secondary btn-sm"
              data-testid="gateway-mock-presets"
              :disabled="presetsLoading"
              @click="loadPresets"
            >
              {{
                presetsLoading
                  ? t("admin.gatewayMock.actions.loadingPresets")
                  : t("admin.gatewayMock.actions.loadPresets")
              }}
            </button>
            <button
              type="button"
              class="btn btn-primary btn-sm"
              data-testid="gateway-mock-save"
              :disabled="saving"
              @click="save"
            >
              {{
                saving
                  ? t("admin.gatewayMock.actions.saving")
                  : t("admin.gatewayMock.actions.save")
              }}
            </button>
          </div>
          <p
            v-if="dirty"
            class="mt-2 text-xs text-amber-700 dark:text-amber-300"
            data-testid="gateway-mock-unsaved"
          >
            {{ t("admin.gatewayMock.unsaved") }}
          </p>
          <p
            v-if="notice"
            class="mt-2 text-xs text-gray-600 dark:text-dark-300"
            data-testid="gateway-mock-notice"
          >
            {{ notice }}
          </p>
          <p
            v-if="errorMessage"
            role="alert"
            class="mt-2 text-sm text-red-700 dark:text-red-300"
            data-testid="gateway-mock-error"
          >
            {{ errorMessage }}
          </p>
        </div>
      </template>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import { seedPresets, updateOperatorSettings } from "./api";
import { gatewayMockFailureKey } from "./labels";
import type { GatewayMockOperatorStatus, GatewayMockRuleInput } from "./types";

/**
 * The rule set is edited as a whole because the API replaces it as a whole: the
 * draft is only ever what the operator typed, and saving sends a complete set.
 * The stored status stays the parent's, so the displayed effective state is the
 * one the server last answered with, never the draft.
 */
const props = defineProps<{
  status: GatewayMockOperatorStatus | null;
  loading: boolean;
}>();
const emit = defineEmits<{
  (e: "updated", status: GatewayMockOperatorStatus): void;
}>();
const { t } = useI18n();
const enabled = ref(false);
const rules = ref<GatewayMockRuleInput[]>([]);
const saving = ref(false);
const presetsLoading = ref(false);
const notice = ref("");
const errorMessage = ref("");

/** A server answer adopts over the draft: it is the set the server now holds. */
function adopt(next: GatewayMockOperatorStatus | null) {
  enabled.value = next?.enabled ?? false;
  rules.value = (next?.rules ?? []).map((rule) => ({
    id: rule.id,
    keyword: rule.keyword,
    reply: rule.reply,
    enabled: rule.enabled,
  }));
}
watch(() => props.status, adopt, { immediate: true });

/** The gateway compares keywords after trimming and lower-casing; the draft does too. */
function normalizedKeyword(keyword: string): string {
  return keyword.trim().toLowerCase();
}

/**
 * Whether saving would change anything. A row without an id is a new rule and is
 * always a change; a saved row is compared by id, so the server's own ordering
 * and keyword normalization do not read as an edit the operator never made.
 */
const dirty = computed(() => {
  const current = props.status;
  if (!current) return false;
  if (current.enabled !== enabled.value) return true;
  const stored = new Map(current.rules.map((rule) => [rule.id, rule]));
  if (stored.size !== rules.value.length) return true;
  return rules.value.some((rule) => {
    if (rule.id === "") return true;
    const saved = stored.get(rule.id);
    return (
      !saved ||
      saved.enabled !== rule.enabled ||
      saved.reply !== rule.reply ||
      normalizedKeyword(saved.keyword) !== normalizedKeyword(rule.keyword)
    );
  });
});

function addRule() {
  rules.value = [
    ...rules.value,
    { id: "", keyword: "", reply: "", enabled: true },
  ];
}

function removeRule(index: number) {
  rules.value = rules.value.filter((_, current) => current !== index);
}

async function save() {
  if (!props.status || saving.value) return;
  saving.value = true;
  notice.value = "";
  errorMessage.value = "";
  try {
    const next = await updateOperatorSettings({
      enabled: enabled.value,
      rules: rules.value.map((rule) => ({
        id: rule.id,
        keyword: rule.keyword,
        reply: rule.reply,
        enabled: rule.enabled,
      })),
    });
    notice.value = t("admin.gatewayMock.saved");
    emit("updated", next);
  } catch (error) {
    // 校验留在服务端：这里显示的是它给出的有界原因，而不是前端自己的判断。
    errorMessage.value = t(gatewayMockFailureKey(error));
  } finally {
    saving.value = false;
  }
}

async function loadPresets() {
  if (!props.status || presetsLoading.value) return;
  presetsLoading.value = true;
  notice.value = "";
  errorMessage.value = "";
  try {
    const result = await seedPresets();
    notice.value =
      result.created > 0
        ? t("admin.gatewayMock.presetsLoaded", { written: result.created })
        : t("admin.gatewayMock.presetsUnchanged");
    emit("updated", result.status);
  } catch (error) {
    errorMessage.value = t(gatewayMockFailureKey(error));
  } finally {
    presetsLoading.value = false;
  }
}
</script>
