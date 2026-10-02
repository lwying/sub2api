<template>
  <section
    class="py-6"
    data-testid="gateway-mock-settings"
    :aria-busy="loading || saving"
  >
    <header class="flex flex-wrap items-start justify-between gap-4">
      <div class="min-w-0">
        <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
          {{ t("admin.gatewayMock.title") }}
        </h2>
        <p class="mt-1 max-w-2xl text-sm text-gray-500 dark:text-dark-300">
          {{ t("admin.gatewayMock.description") }}
        </p>
      </div>
      <div v-if="status" class="flex flex-wrap items-center gap-4">
        <span
          class="inline-flex items-center gap-2 rounded-full px-3 py-1 text-xs font-medium"
          :class="
            status.enabled
              ? 'bg-primary-50 text-primary-700 dark:bg-primary-950/50 dark:text-primary-300'
              : 'bg-gray-100 text-gray-600 dark:bg-dark-800 dark:text-dark-300'
          "
          data-testid="gateway-mock-state"
          :data-state="status.enabled ? 'on' : 'off'"
        >
          <span
            class="h-1.5 w-1.5 rounded-full bg-current"
            aria-hidden="true"
          />
          {{ t("admin.gatewayMock.stateLabel") }}
          {{
            status.enabled
              ? t("admin.gatewayMock.on")
              : t("admin.gatewayMock.off")
          }}
        </span>
        <div
          class="flex items-center gap-2.5 text-sm text-gray-700 dark:text-dark-200"
        >
          <label for="gateway-mock-switch" class="cursor-pointer">{{
            t("admin.gatewayMock.switchLabel")
          }}</label>
          <Toggle
            id="gateway-mock-switch"
            v-model="enabled"
            :aria-label="t('admin.gatewayMock.switchLabel')"
            :disabled="saving || presetsLoading"
            data-testid="gateway-mock-switch"
          />
        </div>
      </div>
    </header>

    <p
      v-if="loading && !status"
      role="status"
      class="py-10 text-center text-sm text-gray-500 dark:text-dark-300"
      data-testid="gateway-mock-loading"
    >
      {{ t("admin.gatewayMock.loading") }}
    </p>
    <p
      v-else-if="!status"
      role="alert"
      class="mt-5 rounded-lg bg-red-50 p-4 text-sm text-red-700 dark:bg-red-950/30 dark:text-red-300"
      data-testid="gateway-mock-unavailable"
    >
      {{ t("admin.gatewayMock.unavailable") }}
    </p>
    <template v-else>
      <div
        class="mt-4 flex flex-wrap gap-x-6 gap-y-1 text-xs leading-5 text-gray-500 dark:text-dark-400"
      >
        <p data-testid="gateway-mock-state-note">
          {{ t("admin.gatewayMock.stateNote") }}
        </p>
        <p data-testid="gateway-mock-switch-note">
          {{ t("admin.gatewayMock.switchNote") }}
        </p>
      </div>

      <div class="mt-6">
        <div class="flex flex-wrap items-center justify-between gap-3">
          <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
            {{ t("admin.gatewayMock.rules.title") }}
          </h3>
          <p
            class="max-w-2xl text-xs text-gray-500 dark:text-dark-400"
            data-testid="gateway-mock-rules-note"
          >
            {{ t("admin.gatewayMock.rules.description") }}
          </p>
        </div>
        <p
          v-if="!rules.length"
          class="mt-4 rounded-xl border border-dashed border-gray-200 px-4 py-8 text-center text-sm text-gray-500 dark:border-dark-700 dark:text-dark-300"
          data-testid="gateway-mock-rules-empty"
        >
          {{ t("admin.gatewayMock.rules.empty") }}
        </p>
        <div
          v-else
          class="mt-3 divide-y divide-gray-100 border-y border-gray-100 dark:divide-dark-700 dark:border-dark-700"
        >
          <div
            class="hidden grid-cols-[minmax(10rem,1fr)_minmax(0,2fr)_6rem_4rem] gap-4 py-2.5 text-xs font-medium text-gray-500 dark:text-dark-400 lg:grid"
            aria-hidden="true"
          >
            <span>{{ t("admin.gatewayMock.rules.keyword") }}</span>
            <span>{{ t("admin.gatewayMock.rules.reply") }}</span>
            <span>{{ t("admin.gatewayMock.rules.enabled") }}</span>
            <span class="sr-only">{{
              t("admin.gatewayMock.rules.remove")
            }}</span>
          </div>
          <div
            v-for="(rule, index) in rules"
            :key="index"
            class="grid grid-cols-[minmax(0,1fr)_auto] gap-3 py-4 lg:grid-cols-[minmax(10rem,1fr)_minmax(0,2fr)_6rem_4rem] lg:items-start lg:gap-4"
            data-testid="gateway-mock-rule-row"
          >
            <label
              class="col-span-2 min-w-0 text-xs text-gray-600 dark:text-dark-300 lg:col-span-1"
            >
              <span class="mb-1.5 block lg:sr-only">{{
                t("admin.gatewayMock.rules.keyword")
              }}</span>
              <input
                v-model="rule.keyword"
                data-testid="gateway-mock-rule-keyword"
                class="input w-full"
                autocomplete="off"
                :disabled="saving || presetsLoading"
              />
            </label>
            <label
              class="col-span-2 min-w-0 text-xs text-gray-600 dark:text-dark-300 lg:col-span-1"
            >
              <span class="mb-1.5 block lg:sr-only">{{
                t("admin.gatewayMock.rules.reply")
              }}</span>
              <textarea
                v-model="rule.reply"
                data-testid="gateway-mock-rule-reply"
                rows="2"
                class="input block w-full resize-y text-sm leading-5"
                :disabled="saving || presetsLoading"
              />
            </label>
            <div class="flex items-center gap-2.5 lg:pt-2">
              <Toggle
                v-model="rule.enabled"
                :aria-label="t('admin.gatewayMock.rules.enabled')"
                :disabled="saving || presetsLoading"
                data-testid="gateway-mock-rule-enabled"
              />
              <span
                class="text-xs text-gray-500 dark:text-dark-300 lg:sr-only"
                data-testid="gateway-mock-rule-enabled-label"
              >
                {{
                  rule.enabled
                    ? t("admin.gatewayMock.rules.enabledOn")
                    : t("admin.gatewayMock.rules.enabledOff")
                }}
              </span>
            </div>
            <button
              type="button"
              class="btn btn-ghost btn-sm justify-self-end text-gray-500 hover:text-red-600 dark:text-dark-400 dark:hover:text-red-400"
              data-testid="gateway-mock-rule-remove"
              :disabled="saving || presetsLoading"
              @click="removeRule(index)"
            >
              <Icon name="trash" size="sm" aria-hidden="true" />
              <span class="sm:sr-only">{{
                t("admin.gatewayMock.rules.remove")
              }}</span>
            </button>
          </div>
        </div>

        <div class="mt-4 flex flex-wrap items-center justify-between gap-3">
          <div class="flex flex-wrap items-center gap-2">
            <button
              type="button"
              class="btn btn-secondary btn-sm"
              data-testid="gateway-mock-rule-add"
              :disabled="saving || presetsLoading"
              @click="addRule"
            >
              <Icon name="plus" size="sm" aria-hidden="true" />
              {{ t("admin.gatewayMock.rules.add") }}
            </button>
            <button
              type="button"
              class="btn btn-ghost btn-sm text-gray-600 dark:text-dark-300"
              data-testid="gateway-mock-presets"
              :disabled="saving || presetsLoading"
              @click="loadPresets"
            >
              {{
                presetsLoading
                  ? t("admin.gatewayMock.actions.loadingPresets")
                  : t("admin.gatewayMock.actions.loadPresets")
              }}
            </button>
          </div>
          <div class="flex w-full items-center justify-end gap-3 sm:w-auto">
            <p
              v-if="dirty"
              class="text-xs font-medium text-amber-700 dark:text-amber-300"
              role="status"
              data-testid="gateway-mock-unsaved"
            >
              {{ t("admin.gatewayMock.unsaved") }}
            </p>
            <button
              type="button"
              class="btn btn-primary btn-sm"
              data-testid="gateway-mock-save"
              :disabled="saving || presetsLoading"
              @click="save"
            >
              {{
                saving
                  ? t("admin.gatewayMock.actions.saving")
                  : t("admin.gatewayMock.actions.save")
              }}
            </button>
          </div>
        </div>
        <p
          v-if="notice"
          role="status"
          class="mt-3 text-sm text-primary-700 dark:text-primary-300"
          data-testid="gateway-mock-notice"
        >
          {{ notice }}
        </p>
        <p
          v-if="errorMessage"
          role="alert"
          class="mt-3 rounded-lg bg-red-50 px-4 py-3 text-sm text-red-700 dark:bg-red-950/30 dark:text-red-300"
          data-testid="gateway-mock-error"
        >
          {{ errorMessage }}
        </p>
      </div>
    </template>
  </section>
</template>

<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import Toggle from "@/components/common/Toggle.vue";
import Icon from "@/components/icons/Icon.vue";
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
