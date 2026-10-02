<template>
  <section class="space-y-5" data-testid="request-trace-operator">
    <p v-if="loading && !status" data-testid="request-trace-loading">
      {{ t("common.loading") }}
    </p>
    <div
      v-else-if="!status"
      role="alert"
      class="card p-6 text-sm text-amber-700 dark:text-amber-300"
      data-testid="request-trace-unavailable"
    >
      <p>{{ t("admin.requestTrace.operator.unavailable") }}</p>
      <button
        type="button"
        class="btn btn-secondary btn-sm mt-3"
        data-testid="request-trace-status-retry"
        @click="emit('retry-status')"
      >
        {{ t("admin.requestTrace.list.candidates.retry") }}
      </button>
    </div>

    <template v-else>
      <!-- 采集状态：已保存开关与实际结论分开显示。 -->
      <div class="card p-6" data-testid="request-trace-status">
        <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
          {{ t("admin.requestTrace.operator.status.title") }}
        </h2>
        <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
          {{ t("admin.requestTrace.operator.status.description") }}
        </p>
        <dl class="mt-4 grid gap-x-6 gap-y-3 text-sm sm:grid-cols-2">
          <div>
            <dt class="text-xs text-gray-500 dark:text-gray-400">
              {{ t("admin.requestTrace.operator.status.stored") }}
            </dt>
            <dd
              data-testid="request-trace-stored"
              :data-state="status.enabled ? 'on' : 'off'"
              class="mt-0.5 font-medium text-gray-900 dark:text-white"
            >
              {{ boolLabel(status.enabled) }}
            </dd>
          </div>
          <div>
            <dt class="text-xs text-gray-500 dark:text-gray-400">
              {{ t("admin.requestTrace.operator.status.effective") }}
            </dt>
            <dd
              data-testid="request-trace-capture-state"
              :data-state="effectiveAllowed ? 'on' : 'off'"
              class="mt-0.5 font-medium text-gray-900 dark:text-white"
            >
              {{
                captureExpired
                  ? t("admin.requestTrace.operator.status.expired")
                  : effectiveAllowed
                    ? t("admin.requestTrace.operator.status.active")
                    : t("admin.requestTrace.operator.status.inactive")
              }}
            </dd>
          </div>
          <div>
            <dt class="text-xs text-gray-500 dark:text-gray-400">
              {{ t("admin.requestTrace.operator.deployment") }}
            </dt>
            <dd data-testid="request-trace-deployment" class="mt-0.5">
              {{
                t(
                  `admin.requestTrace.operator.support.${status.plaintext_capture_support_reason}`,
                )
              }}
            </dd>
          </div>
          <div>
            <dt class="text-xs text-gray-500 dark:text-gray-400">
              {{ t("admin.requestTrace.operator.status.expiresAt") }}
            </dt>
            <dd data-testid="request-trace-expires-at" class="mt-0.5">
              {{
                status.capture_until
                  ? formatDate(status.capture_until)
                  : t("admin.requestTrace.operator.status.forever")
              }}
            </dd>
          </div>
          <div>
            <dt class="text-xs text-gray-500 dark:text-gray-400">
              {{ t("admin.requestTrace.operator.status.remaining") }}
            </dt>
            <dd
              data-testid="request-trace-remaining"
              :data-expired="captureExpired ? 'true' : 'false'"
              class="mt-0.5"
            >
              {{ remainingLabel }}
            </dd>
          </div>
        </dl>

        <p
          v-if="!status.plaintext_capture_supported"
          role="alert"
          data-testid="request-trace-deployment-blocked"
          class="mt-3 text-sm text-amber-700 dark:text-amber-300"
        >
          {{ t("admin.requestTrace.operator.deploymentBlocked") }}
        </p>
        <p
          v-if="
            status.enabled &&
            !status.capture_allowed &&
            !status.risk_acknowledgement_current
          "
          role="alert"
          data-testid="request-trace-ack-stale"
          class="mt-3 text-sm text-amber-700 dark:text-amber-300"
        >
          {{ t("admin.requestTrace.operator.ackStale") }}
        </p>
        <p
          v-if="captureExpired"
          role="alert"
          data-testid="request-trace-expired"
          :data-source="expiryPendingServerRefresh ? 'derived' : 'server'"
          class="mt-3 text-sm text-amber-700 dark:text-amber-300"
        >
          {{
            expiryPendingServerRefresh
              ? t("admin.requestTrace.operator.status.expiredPendingNote")
              : t("admin.requestTrace.operator.status.expiredNote")
          }}
        </p>

        <!-- 已存范围：直接来自服务端，绝不来自草稿。 -->
        <div
          class="mt-4 space-y-2 border-t border-gray-100 pt-4 text-sm dark:border-dark-700"
        >
          <div class="flex flex-wrap gap-x-3 gap-y-1">
            <span class="shrink-0 text-gray-500 dark:text-gray-400">
              {{ t("admin.requestTrace.operator.scope.storedGroups") }}
            </span>
            <span
              class="min-w-0 break-words text-gray-900 dark:text-gray-100"
              data-testid="request-trace-scope-stored-groups"
              >{{ storedGroups }}</span
            >
          </div>
          <div class="flex flex-wrap gap-x-3 gap-y-1">
            <span class="shrink-0 text-gray-500 dark:text-gray-400">
              {{ t("admin.requestTrace.operator.scope.storedModels") }}
            </span>
            <span
              class="min-w-0 break-words text-gray-900 dark:text-gray-100"
              data-testid="request-trace-scope-stored-models"
              >{{ storedModels }}</span
            >
          </div>
          <div class="flex flex-wrap gap-x-3 gap-y-1">
            <span class="shrink-0 text-gray-500 dark:text-gray-400">
              {{ t("admin.requestTrace.operator.scope.storedPlatforms") }}
            </span>
            <span
              class="min-w-0 break-words text-gray-900 dark:text-gray-100"
              data-testid="request-trace-scope-stored-platforms"
              >{{ storedPlatforms }}</span
            >
          </div>
        </div>
      </div>

      <!-- 快捷预设：只改正文与 HTTP 200 两个草稿开关。 -->
      <div class="card p-6" data-testid="request-trace-presets">
        <div class="flex flex-wrap items-start justify-between gap-2">
          <div>
            <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
              {{ t("admin.requestTrace.operator.presets.title") }}
            </h2>
            <p class="mt-1 max-w-3xl text-sm text-gray-500 dark:text-gray-400">
              {{ t("admin.requestTrace.operator.presets.description") }}
            </p>
          </div>
          <span
            class="rounded-full border border-primary-200 bg-primary-50 px-3 py-1 text-xs font-medium text-primary-700 dark:border-primary-800 dark:bg-primary-900/30 dark:text-primary-300"
            data-testid="request-trace-preset-current"
          >
            {{ presetLabel }}
          </span>
        </div>
        <div class="mt-4 grid gap-3 sm:grid-cols-3">
          <button
            v-for="preset in presetButtons"
            :key="preset.id"
            type="button"
            class="rounded-xl border px-4 py-3 text-left transition-colors"
            :class="
              presetCurrent === preset.id
                ? 'border-primary-500 bg-primary-50 dark:border-primary-600 dark:bg-primary-900/20'
                : 'border-gray-200 hover:border-gray-300 dark:border-dark-600 dark:hover:border-dark-500'
            "
            :data-testid="`request-trace-preset-${preset.id}`"
            @click="applyPreset(preset.id)"
          >
            <span
              class="block text-sm font-medium text-gray-900 dark:text-white"
            >
              {{ preset.label }}
            </span>
            <span class="mt-1 block text-xs text-gray-500 dark:text-gray-400">
              {{ preset.hint }}
            </span>
          </button>
        </div>
      </div>

      <!--
        生效与草稿：默认折叠，避免对比表占满配置首屏；草稿是否有改动已在底部栏常驻显示。
      -->
      <details class="card px-6 py-4" data-testid="request-trace-summary">
        <summary
          class="cursor-pointer list-none text-sm text-gray-900 dark:text-white"
        >
          <span class="font-medium">{{
            t("admin.requestTrace.operator.summaryTitle")
          }}</span>
          <span class="ml-2 text-xs text-gray-500 dark:text-gray-400">{{
            summaryBrief
          }}</span>
        </summary>
        <p class="mt-2 text-xs text-gray-500 dark:text-gray-400">
          {{ t("admin.requestTrace.operator.summaryDescription") }}
        </p>
        <div class="mt-3 overflow-x-auto">
          <table class="w-full text-left text-sm">
            <thead>
              <tr class="text-xs text-gray-500 dark:text-gray-400">
                <th class="py-1 pr-4 font-normal">
                  {{ t("admin.requestTrace.operator.summaryField") }}
                </th>
                <th class="py-1 pr-4 font-normal">
                  {{ t("admin.requestTrace.operator.summaryApplied") }}
                </th>
                <th class="py-1 font-normal">
                  {{ t("admin.requestTrace.operator.summaryDraft") }}
                </th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="row in summaryRows"
                :key="row.key"
                :data-testid="`request-trace-summary-${row.key}`"
                :data-diff="row.applied === row.draft ? 'same' : 'diff'"
                class="border-t border-gray-100 dark:border-dark-700"
              >
                <td class="py-1.5 pr-4 text-gray-500 dark:text-gray-400">
                  {{ row.label }}
                </td>
                <td class="py-1.5 pr-4">{{ row.applied }}</td>
                <td
                  class="py-1.5"
                  :class="
                    row.applied === row.draft
                      ? ''
                      : 'font-medium text-primary-700 dark:text-primary-300'
                  "
                >
                  {{ row.draft }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </details>

      <!-- 采集内容：正文与 HTTP 200 两个开关。 -->
      <div class="card p-6" data-testid="request-trace-content">
        <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
          {{ t("admin.requestTrace.operator.content.title") }}
        </h2>
        <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
          {{ t("admin.requestTrace.operator.content.description") }}
        </p>
        <div class="mt-4 space-y-4">
          <div class="flex items-start justify-between gap-4">
            <div>
              <p class="text-sm font-medium text-gray-900 dark:text-white">
                {{ t("admin.requestTrace.operator.content.body") }}
              </p>
              <p
                class="mt-0.5 max-w-3xl text-xs text-gray-500 dark:text-gray-400"
              >
                {{ t("admin.requestTrace.operator.content.bodyHint") }}
              </p>
              <p
                v-if="!captureBody"
                class="mt-1 text-xs text-amber-700 dark:text-amber-300"
                data-testid="request-trace-body-metadata-only"
              >
                {{ t("admin.requestTrace.operator.content.metadataOnlyNote") }}
              </p>
            </div>
            <Toggle
              v-model="captureBody"
              data-testid="request-trace-body-toggle"
            />
          </div>
          <div
            class="flex items-start justify-between gap-4 border-t border-gray-100 pt-4 dark:border-dark-700"
          >
            <div>
              <p class="text-sm font-medium text-gray-900 dark:text-white">
                {{ t("admin.requestTrace.operator.content.http200") }}
              </p>
              <p
                class="mt-0.5 max-w-3xl text-xs text-gray-500 dark:text-gray-400"
              >
                {{ t("admin.requestTrace.operator.content.http200Hint") }}
              </p>
            </div>
            <Toggle
              v-model="captureHttp200"
              data-testid="request-trace-http200-toggle"
            />
          </div>
        </div>
      </div>

      <!-- 三维采集范围：分组、模型、平台。 -->
      <div class="card p-6" data-testid="request-trace-scope-form">
        <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
          {{ t("admin.requestTrace.operator.scope.title") }}
        </h2>
        <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
          {{ t("admin.requestTrace.operator.scope.description") }}
        </p>

        <p
          v-if="optionsLoading"
          class="mt-3 text-xs text-gray-500 dark:text-gray-400"
          data-testid="request-trace-options-loading"
        >
          {{ t("admin.requestTrace.operator.scope.optionsLoading") }}
        </p>
        <div
          v-else-if="optionsError"
          role="alert"
          class="mt-3 flex flex-wrap items-center gap-3 rounded-lg bg-amber-50 px-4 py-3 text-sm text-amber-800 dark:bg-amber-950/30 dark:text-amber-200"
          data-testid="request-trace-options-error"
        >
          <span>{{ t("admin.requestTrace.operator.scope.optionsError") }}</span>
          <button
            type="button"
            class="btn btn-secondary btn-sm"
            data-testid="request-trace-options-retry"
            @click="emit('retry-options')"
          >
            {{ t("admin.requestTrace.operator.scope.retryOptions") }}
          </button>
        </div>

        <!-- Groups: all or an explicit selection. -->
        <fieldset class="mt-4 space-y-3">
          <legend class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t("admin.requestTrace.operator.scope.groupsLabel") }}
          </legend>
          <label class="flex items-center gap-2 text-sm">
            <input
              v-model="allGroups"
              type="radio"
              name="request-trace-groups-mode"
              :value="true"
              data-testid="request-trace-scope-groups-mode-all"
            />
            <span>{{
              t("admin.requestTrace.operator.scope.groupsMode.all")
            }}</span>
          </label>
          <label class="flex items-center gap-2 text-sm">
            <input
              v-model="allGroups"
              type="radio"
              name="request-trace-groups-mode"
              :value="false"
              data-testid="request-trace-scope-groups-mode-selected"
            />
            <span>{{
              t("admin.requestTrace.operator.scope.groupsMode.selected")
            }}</span>
          </label>
          <GroupSelector
            v-if="!allGroups"
            v-model="groupIDs"
            :groups="groupOptions"
            :searchable="true"
            :preserve-all-groups="true"
          />
          <p
            class="text-xs text-gray-500 dark:text-gray-400"
            data-testid="request-trace-scope-groups-note"
          >
            {{ t("admin.requestTrace.operator.scope.groupsNote") }}
          </p>
        </fieldset>

        <!-- Models: all / only / except, chosen from candidates only. -->
        <fieldset
          class="mt-5 space-y-3 border-t border-gray-100 pt-4 dark:border-dark-700"
        >
          <legend class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t("admin.requestTrace.operator.scope.modelsLabel") }}
          </legend>
          <select
            v-model="modelScope"
            data-testid="request-trace-scope-model-mode"
            class="input"
          >
            <option value="all">
              {{ t("admin.requestTrace.operator.scope.modelAll") }}
            </option>
            <option value="include">
              {{ t("admin.requestTrace.operator.scope.modelInclude") }}
            </option>
            <option value="exclude">
              {{ t("admin.requestTrace.operator.scope.modelExclude") }}
            </option>
          </select>
          <ModelWhitelistSelector
            v-if="modelScope !== 'all'"
            v-model="models"
            :extra-options="modelOptions"
            :allow-custom="false"
            :platforms="[]"
          />
          <p
            class="text-xs text-gray-500 dark:text-gray-400"
            data-testid="request-trace-scope-models-note"
          >
            {{ t("admin.requestTrace.operator.scope.modelsNote") }}
          </p>
        </fieldset>

        <!-- Platforms: all / only / except over the concrete platform catalog. -->
        <fieldset
          class="mt-5 space-y-3 border-t border-gray-100 pt-4 dark:border-dark-700"
        >
          <legend class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t("admin.requestTrace.operator.scope.platformsLabel") }}
          </legend>
          <select
            v-model="platformScope"
            data-testid="request-trace-scope-platform-mode"
            class="input"
          >
            <option value="all">
              {{ t("admin.requestTrace.operator.scope.platformAll") }}
            </option>
            <option value="include">
              {{ t("admin.requestTrace.operator.scope.platformInclude") }}
            </option>
            <option value="exclude">
              {{ t("admin.requestTrace.operator.scope.platformExclude") }}
            </option>
          </select>
          <div
            v-if="platformScope !== 'all'"
            class="grid grid-cols-2 gap-1 rounded-lg border border-gray-200 bg-gray-50 p-3 dark:border-dark-600 dark:bg-dark-800 sm:grid-cols-3"
          >
            <label
              v-for="platform in platformCatalog"
              :key="platform.value"
              class="flex items-center gap-2 rounded px-2 py-1 text-sm"
            >
              <input
                type="checkbox"
                :checked="platforms.includes(platform.value)"
                :data-testid="`request-trace-scope-platform-${platform.value}`"
                @change="togglePlatform(platform.value)"
              />
              <span class="truncate">{{ platform.label }}</span>
            </label>
          </div>
          <p
            class="text-xs text-gray-500 dark:text-gray-400"
            data-testid="request-trace-scope-platforms-note"
          >
            {{ t("admin.requestTrace.operator.scope.platformsNote") }}
          </p>
          <p
            class="text-xs text-amber-700 dark:text-amber-300"
            data-testid="request-trace-scope-platform-risk"
          >
            {{ t("admin.requestTrace.operator.scope.excludeRisk") }}
          </p>
        </fieldset>

        <p
          v-if="staleOptionNotice"
          class="mt-4 text-xs text-amber-700 dark:text-amber-300"
          data-testid="request-trace-scope-stale"
        >
          {{ staleOptionNotice }}
        </p>
      </div>

      <!-- 高级采集：采样率、正文上限、停止时间。 -->
      <div class="card p-6" data-testid="request-trace-advanced">
        <div class="flex items-center justify-between gap-4">
          <div>
            <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
              {{ t("admin.requestTrace.operator.advanced.title") }}
            </h2>
            <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
              {{ t("admin.requestTrace.operator.advanced.description") }}
            </p>
          </div>
          <button
            type="button"
            class="btn btn-secondary btn-sm shrink-0"
            data-testid="request-trace-advanced-toggle"
            :aria-expanded="advancedOpen"
            @click="advancedOpen = !advancedOpen"
          >
            {{ advancedOpen ? t("common.collapse") : t("common.expand") }}
          </button>
        </div>

        <div
          v-show="advancedOpen"
          class="mt-4 space-y-4"
          data-testid="request-trace-advanced-panel"
        >
          <div class="grid gap-4 sm:grid-cols-2">
            <label class="space-y-1 text-sm">
              <span class="text-gray-600 dark:text-gray-400">{{
                t("admin.requestTrace.operator.advanced.sampleHttp200")
              }}</span>
              <input
                v-model.number="sampleRateHttp200"
                type="number"
                min="0"
                max="100"
                step="1"
                data-testid="request-trace-sample-http200"
                :disabled="!captureHttp200"
                class="input w-full disabled:cursor-not-allowed disabled:opacity-60"
              />
            </label>
            <label class="space-y-1 text-sm">
              <span class="text-gray-600 dark:text-gray-400">{{
                t("admin.requestTrace.operator.advanced.sampleOther")
              }}</span>
              <input
                v-model.number="sampleRateOther"
                type="number"
                min="0"
                max="100"
                step="1"
                data-testid="request-trace-sample-other"
                class="input w-full"
              />
            </label>
          </div>
          <p class="text-xs text-gray-500 dark:text-gray-400">
            {{ t("admin.requestTrace.operator.advanced.sampleHint") }}
          </p>
          <p
            v-if="!captureHttp200"
            class="text-xs text-amber-700 dark:text-amber-300"
            data-testid="request-trace-sample-http200-disabled"
          >
            {{ t("admin.requestTrace.operator.advanced.disabledKeepsValue") }}
          </p>

          <label class="block space-y-1 text-sm">
            <span class="text-gray-600 dark:text-gray-400">{{
              t("admin.requestTrace.operator.advanced.bodyLimit")
            }}</span>
            <select
              v-model.number="bodyMaxBytes"
              data-testid="request-trace-body-limit"
              :disabled="!captureBody"
              class="input w-full disabled:cursor-not-allowed disabled:opacity-60"
            >
              <option
                v-for="limit in bodyLimitOptions"
                :key="limit.value"
                :value="limit.value"
              >
                {{ limit.label }}
              </option>
            </select>
          </label>
          <p class="text-xs text-gray-500 dark:text-gray-400">
            {{ t("admin.requestTrace.operator.advanced.bodyLimitHint") }}
          </p>
          <p
            v-if="!captureBody"
            class="text-xs text-amber-700 dark:text-amber-300"
            data-testid="request-trace-body-limit-disabled"
          >
            {{ t("admin.requestTrace.operator.advanced.disabledKeepsValue") }}
          </p>

          <div
            class="space-y-2 border-t border-gray-100 pt-4 dark:border-dark-700"
          >
            <span class="block text-sm text-gray-600 dark:text-gray-400">{{
              t("admin.requestTrace.operator.advanced.duration")
            }}</span>
            <Select
              id="request-trace-duration"
              :model-value="captureDurationSeconds"
              :options="durationOptions"
              @update:model-value="captureDurationSeconds = Number($event ?? 0)"
            />
            <p class="text-xs text-gray-500 dark:text-gray-400">
              {{ t("admin.requestTrace.operator.advanced.durationHint") }}
            </p>
            <p
              v-if="captureExpired"
              class="text-xs text-amber-700 dark:text-amber-300"
              data-testid="request-trace-renew-required"
            >
              {{
                t(
                  "admin.requestTrace.operator.advanced.renewRequiredAfterExpiry",
                )
              }}
            </p>
            <p
              v-if="!status.enabled"
              class="text-xs text-gray-500 dark:text-gray-400"
              data-testid="request-trace-renew-needs-enabled"
            >
              {{ t("admin.requestTrace.operator.advanced.renewNeedsEnabled") }}
            </p>
            <p
              v-else-if="captureDurationSeconds === 0"
              class="text-xs text-gray-500 dark:text-gray-400"
              data-testid="request-trace-renew-needs-duration"
            >
              {{ t("admin.requestTrace.operator.advanced.renewNeedsDuration") }}
            </p>
            <div class="flex flex-wrap items-center gap-3">
              <button
                type="button"
                class="btn btn-secondary btn-sm"
                data-testid="request-trace-renew"
                :disabled="!canRenew"
                @click="renew"
              >
                {{ t("admin.requestTrace.operator.advanced.renew") }}
              </button>
              <span class="text-xs text-gray-500 dark:text-gray-400">
                {{ t("admin.requestTrace.operator.advanced.renewHint") }}
              </span>
            </div>
          </div>
        </div>
      </div>

      <!--
        风险确认放到范围与高级之后：开启与开启状态下的保存/续期都需要逐字输入，
        紧急关闭不需要；部署不支持时若已开启，字段仍可输入以便尝试并看到错误。
      -->
      <div
        v-if="status.plaintext_capture_supported || status.enabled"
        class="card p-6"
        data-testid="request-trace-risk"
      >
        <div class="space-y-3">
          <label
            class="block text-sm text-gray-600 dark:text-gray-400"
            for="request-trace-language"
            >{{ t("admin.requestTrace.operator.language") }}</label
          >
          <select
            id="request-trace-language"
            v-model="language"
            data-testid="request-trace-language"
            class="input"
          >
            <option value="en">{{ ackLanguageLabel(t, "en") }}</option>
            <option value="zh">{{ ackLanguageLabel(t, "zh") }}</option>
          </select>
          <p class="text-sm text-gray-600 dark:text-gray-400">
            {{ t("admin.requestTrace.operator.requiredPhrase") }}
          </p>
          <p
            class="rounded-lg border border-gray-200 bg-gray-50 p-3 text-xs dark:border-dark-600 dark:bg-dark-800"
            data-testid="request-trace-required-phrase"
          >
            {{ requiredPhrase }}
          </p>
          <label
            class="block text-sm text-gray-600 dark:text-gray-400"
            for="request-trace-phrase"
            >{{ t("admin.requestTrace.operator.typePhrase") }}</label
          >
          <textarea
            id="request-trace-phrase"
            v-model="typedPhrase"
            data-testid="request-trace-phrase-input"
            rows="3"
            autocomplete="off"
            spellcheck="false"
            class="w-full rounded-lg border border-gray-200 p-3 text-xs dark:border-dark-600 dark:bg-dark-900"
          />
        </div>
        <div class="mt-4 flex flex-wrap items-center gap-3">
          <button
            v-if="!status.enabled"
            type="button"
            class="btn btn-primary"
            data-testid="request-trace-enable"
            :disabled="!canEnable"
            @click="enable"
          >
            {{ t("admin.requestTrace.operator.enable") }}
          </button>
          <button
            v-else
            type="button"
            class="btn btn-secondary"
            data-testid="request-trace-disable"
            :disabled="submitting"
            @click="disable"
          >
            {{ t("admin.requestTrace.operator.disable") }}
          </button>
        </div>
      </div>

      <!-- 底部固定草稿栏：重置与保存，草稿状态常驻可见。 -->
      <div
        class="sticky bottom-0 z-20 -mx-1 flex flex-wrap items-center justify-between gap-3 rounded-t-xl border-t border-gray-200 bg-white/95 px-4 py-3 shadow-[0_-8px_24px_rgba(15,23,42,0.06)] backdrop-blur dark:border-dark-700/80 dark:bg-dark-900/95"
        data-testid="request-trace-draft-bar"
      >
        <span
          class="text-sm"
          :class="
            dirty
              ? 'text-amber-700 dark:text-amber-300'
              : 'text-gray-500 dark:text-gray-400'
          "
          data-testid="request-trace-draft-state"
        >
          {{
            dirty
              ? t("admin.requestTrace.operator.actions.dirty")
              : t("admin.requestTrace.operator.actions.synced")
          }}
        </span>
        <div class="flex items-center gap-3">
          <button
            type="button"
            class="btn btn-secondary"
            data-testid="request-trace-reset"
            :disabled="!dirty || submitting"
            @click="resetDraft"
          >
            {{ t("admin.requestTrace.operator.actions.reset") }}
          </button>
          <button
            type="button"
            class="btn btn-primary"
            data-testid="request-trace-save"
            :disabled="!canSave"
            @click="save"
          >
            {{
              submitting
                ? t("admin.requestTrace.operator.actions.saving")
                : t("admin.requestTrace.operator.actions.save")
            }}
          </button>
        </div>
      </div>
      <p
        v-if="savedNotice"
        role="status"
        data-testid="request-trace-saved"
        class="text-sm text-green-700 dark:text-green-300"
      >
        {{ t("admin.requestTrace.operator.actions.saved") }}
      </p>
      <p
        v-if="errorMessage"
        role="alert"
        data-testid="request-trace-error"
        class="text-sm text-amber-700 dark:text-amber-300"
      >
        {{ errorMessage }}
      </p>
    </template>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import { getLocale } from "@/i18n";
import Toggle from "@/components/common/Toggle.vue";
import Select from "@/components/common/Select.vue";
import GroupSelector from "@/components/common/GroupSelector.vue";
import ModelWhitelistSelector from "@/components/account/ModelWhitelistSelector.vue";
import { updateOperatorSettings } from "./api";
import { ackLanguageLabel } from "./labels";
import type { AdminGroup } from "@/types";
import type {
  RequestTraceOperatorStatus,
  RequestTraceScope,
  TraceAckLanguage,
} from "./types";
import { CONCRETE_PLATFORM_OPTIONS } from "@/constants/platforms";

const props = withDefaults(
  defineProps<{
    status: RequestTraceOperatorStatus | null;
    loading?: boolean;
    groups?: AdminGroup[];
    modelCandidates?: string[];
    optionsLoading?: boolean;
    optionsError?: string;
  }>(),
  { loading: false },
);
const emit = defineEmits<{
  (e: "updated", status: RequestTraceOperatorStatus): void;
  (e: "retry-options"): void;
  (e: "retry-status"): void;
}>();
const { t, locale } = useI18n();

// 服务端自身的边界，前端同步校验，避免提交会被后端拒绝或静默收敛的取值。
const bodyLimitValues = [65536, 262144, 1048576] as const;
const durationValues = [0, 900, 3600, 86400] as const;

const language = ref<TraceAckLanguage>(getLocale() === "zh" ? "zh" : "en");
const typedPhrase = ref("");
const submitting = ref(false);
const errorMessage = ref("");
const savedNotice = ref(false);
const advancedOpen = ref(false);
let syncing = false;

// 草稿：它不等于已存设置；上方摘要才显示服务端自己的值。切换选项卡不能丢失
// 未保存的编辑（父级用 v-show 保持本面板挂载）。
const captureBody = ref(true);
const captureHttp200 = ref(true);
const sampleRateHttp200 = ref(100);
const sampleRateOther = ref(100);
const bodyMaxBytes = ref(1048576);
const captureDurationSeconds = ref(0);
const allGroups = ref(true);
const groupIDs = ref<number[]>([]);
const modelScope = ref<RequestTraceScope>("all");
const models = ref<string[]>([]);
const platformScope = ref<RequestTraceScope>("all");
const platforms = ref<string[]>([]);

function syncDraft(status: RequestTraceOperatorStatus) {
  syncing = true;
  captureBody.value = status.capture_body;
  captureHttp200.value = status.capture_http_200;
  sampleRateHttp200.value = status.sample_rate_http_200;
  sampleRateOther.value = status.sample_rate_other;
  bodyMaxBytes.value = status.body_max_bytes;
  captureDurationSeconds.value = status.capture_duration_seconds;
  allGroups.value = status.all_groups;
  groupIDs.value = [...status.group_ids];
  modelScope.value = status.model_scope;
  models.value = [...status.models];
  platformScope.value = status.platform_scope;
  platforms.value = [...status.platforms];
  syncing = false;
}

watch(
  () => props.status,
  (status) => {
    if (status) syncDraft(status);
  },
  { immediate: true },
);

watch(
  [
    captureBody,
    captureHttp200,
    sampleRateHttp200,
    sampleRateOther,
    bodyMaxBytes,
    captureDurationSeconds,
    allGroups,
    groupIDs,
    modelScope,
    models,
    platformScope,
    platforms,
  ],
  () => {
    if (!syncing) {
      savedNotice.value = false;
      errorMessage.value = "";
    }
  },
  { flush: "sync" },
);

watch([() => props.status, language], () => {
  typedPhrase.value = "";
});

const requiredPhrase = computed(() =>
  language.value === "zh"
    ? (props.status?.risk_phrase_zh ?? "")
    : (props.status?.risk_phrase_en ?? ""),
);
const phraseReady = computed(
  () =>
    requiredPhrase.value !== "" &&
    typedPhrase.value.trim() === requiredPhrase.value,
);

function boolLabel(value: boolean): string {
  return value
    ? t("admin.requestTrace.operator.on")
    : t("admin.requestTrace.operator.off");
}

function formatDate(value: string): string {
  return new Intl.DateTimeFormat(locale.value, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(new Date(value));
}

// 到期倒计时与结论必须随时钟推进，因此用响应式的 now 驱动，而不是在 computed
// 里读取一次性的 Date.now()。面板即使被 v-show 隐藏也保持低频轮询；卸载时清理。
const now = ref(Date.now());
let deadlineTicker: ReturnType<typeof setInterval> | null = null;
onMounted(() => {
  deadlineTicker = setInterval(() => {
    now.value = Date.now();
  }, 1000);
});
onBeforeUnmount(() => {
  if (deadlineTicker !== null) {
    clearInterval(deadlineTicker);
    deadlineTicker = null;
  }
});

/**
 * 截止时间的本地判读。null 表示服务端明确“无时限”，不参与本地到期判断；
 * 无法核对的时间戳按已到期处理（fail-closed），但绝不冒充服务端自己的结论。
 */
const captureDeadlineMs = computed<number | null>(() => {
  const until = props.status?.capture_until;
  if (!until) return null;
  const parsed = Date.parse(until);
  return Number.isFinite(parsed) ? parsed : 0;
});
/** 本地时钟判定已过截止时间。服务端未刷新时也必须按已停止呈现。 */
const derivedExpiry = computed(
  () =>
    captureDeadlineMs.value !== null && now.value >= captureDeadlineMs.value,
);
/** 服务端确认到期，或本地时钟先判定到期；任一成立都不再声称正在采集。 */
const captureExpired = computed(
  () => props.status?.capture_expired === true || derivedExpiry.value,
);
/** 本地已到期但服务端尚未确认：状态待刷新，来源应标注为本地推导。 */
const expiryPendingServerRefresh = computed(
  () => derivedExpiry.value && props.status?.capture_expired !== true,
);
/** 实际采集结论：服务端允许且本地未判定到期。 */
const effectiveAllowed = computed(
  () => props.status?.capture_allowed === true && !captureExpired.value,
);

// 本地判定到期时请求一次新的服务端状态（父级可据此重新拉取）。闸门是“已请求过
// 的截止时间字符串”：同一截止时间只请求一次，窗口重开得到新截止时间后可再请求。
const refreshRequestedForDeadline = ref<string | null>(null);
watch(
  [derivedExpiry, () => props.status?.capture_until ?? null],
  ([derived, deadline]) => {
    if (!derived) {
      // 无时限或窗口尚在：清掉闸门，允许同一截止时间下次到期时重新请求。
      refreshRequestedForDeadline.value = null;
      return;
    }
    if (deadline === null || refreshRequestedForDeadline.value === deadline)
      return;
    refreshRequestedForDeadline.value = deadline;
    emit("retry-status");
  },
  { immediate: true },
);

const remainingLabel = computed(() => {
  const until = props.status?.capture_until;
  if (!until) return t("admin.requestTrace.operator.status.forever");
  const ms = (captureDeadlineMs.value ?? 0) - now.value;
  if (!(ms > 0)) return t("admin.requestTrace.operator.status.expired");
  const minutes = Math.floor(ms / 60000);
  const days = Math.floor(minutes / 1440);
  const hours = Math.floor((minutes % 1440) / 60);
  const mins = minutes % 60;
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${mins}m`;
  return `${mins}m`;
});

// --- 生效（服务端）摘要 ---
function scopeValues(
  kind: RequestTraceScope,
  entries: readonly (string | number)[],
): string {
  if (kind === "all") return t("admin.requestTrace.operator.scope.allValues");
  if (entries.length === 0)
    return t("admin.requestTrace.operator.scope.emptyValues");
  const values = entries.join(", ");
  return kind === "include"
    ? t("admin.requestTrace.operator.scope.onlyValues", { values })
    : t("admin.requestTrace.operator.scope.exceptValues", { values });
}

/** 分组标签：目录里已知的用名称，缺失目录的才回退为 #ID，便于换行阅读。 */
function groupScopeValues(all: boolean, ids: readonly number[]): string {
  if (all) return t("admin.requestTrace.operator.scope.allValues");
  if (ids.length === 0)
    return t("admin.requestTrace.operator.scope.emptyValues");
  const names = new Map(
    (props.groups ?? []).map((group) => [group.id, group.name]),
  );
  const values = ids.map((id) => names.get(id) ?? `#${id}`).join(", ");
  return t("admin.requestTrace.operator.scope.onlyValues", { values });
}

const storedGroups = computed(() =>
  props.status
    ? groupScopeValues(props.status.all_groups, props.status.group_ids)
    : "",
);
const storedModels = computed(() =>
  props.status
    ? scopeValues(props.status.model_scope, props.status.models)
    : "",
);
const storedPlatforms = computed(() =>
  props.status
    ? scopeValues(props.status.platform_scope, props.status.platforms)
    : "",
);

// --- 草稿摘要 ---
const draftGroups = computed(() =>
  groupScopeValues(allGroups.value, groupIDs.value),
);
const draftModels = computed(() => scopeValues(modelScope.value, models.value));
const draftPlatforms = computed(() =>
  scopeValues(platformScope.value, platforms.value),
);

function durationLabel(value: number): string {
  if (value === 900)
    return t("admin.requestTrace.operator.advanced.duration15m");
  if (value === 3600)
    return t("admin.requestTrace.operator.advanced.duration1h");
  if (value === 86400)
    return t("admin.requestTrace.operator.advanced.duration24h");
  return t("admin.requestTrace.operator.advanced.durationForever");
}

function bodyLimitLabel(bytes: number): string {
  return bytes >= 1048576 ? `${bytes / 1048576} MiB` : `${bytes / 1024} KiB`;
}

const durationOptions = computed(() =>
  durationValues.map((value) => ({ value, label: durationLabel(value) })),
);
const bodyLimitOptions = computed(() =>
  bodyLimitValues.map((value) => ({ value, label: bodyLimitLabel(value) })),
);

const summaryRows = computed(() => {
  if (!props.status) return [];
  return [
    {
      key: "body",
      label: t("admin.requestTrace.operator.content.body"),
      applied: boolLabel(props.status.capture_body),
      draft: boolLabel(captureBody.value),
    },
    {
      key: "http200",
      label: t("admin.requestTrace.operator.content.http200"),
      applied: boolLabel(props.status.capture_http_200),
      draft: boolLabel(captureHttp200.value),
    },
    {
      key: "groups",
      label: t("admin.requestTrace.operator.scope.storedGroups"),
      applied: storedGroups.value,
      draft: draftGroups.value,
    },
    {
      key: "models",
      label: t("admin.requestTrace.operator.scope.storedModels"),
      applied: storedModels.value,
      draft: draftModels.value,
    },
    {
      key: "platforms",
      label: t("admin.requestTrace.operator.scope.storedPlatforms"),
      applied: storedPlatforms.value,
      draft: draftPlatforms.value,
    },
    {
      key: "sample-http200",
      label: t("admin.requestTrace.operator.advanced.sampleHttp200"),
      applied: `${props.status.sample_rate_http_200}%`,
      draft: `${sampleRateHttp200.value}%`,
    },
    {
      key: "sample-other",
      label: t("admin.requestTrace.operator.advanced.sampleOther"),
      applied: `${props.status.sample_rate_other}%`,
      draft: `${sampleRateOther.value}%`,
    },
    {
      key: "body-limit",
      label: t("admin.requestTrace.operator.advanced.bodyLimit"),
      applied: bodyLimitLabel(props.status.body_max_bytes),
      draft: bodyLimitLabel(bodyMaxBytes.value),
    },
    {
      key: "duration",
      label: t("admin.requestTrace.operator.advanced.duration"),
      applied: durationLabel(props.status.capture_duration_seconds),
      draft: durationLabel(captureDurationSeconds.value),
    },
  ];
});

// --- 预设：只改两个内容开关 ---
type PresetID = "lite" | "chain" | "detailed";
const presetButtons = computed(() => [
  {
    id: "lite" as const,
    label: t("admin.requestTrace.operator.presets.lite"),
    hint: t("admin.requestTrace.operator.presets.liteHint"),
  },
  {
    id: "chain" as const,
    label: t("admin.requestTrace.operator.presets.chain"),
    hint: t("admin.requestTrace.operator.presets.chainHint"),
  },
  {
    id: "detailed" as const,
    label: t("admin.requestTrace.operator.presets.detailed"),
    hint: t("admin.requestTrace.operator.presets.detailedHint"),
  },
]);
function presetFor(body: boolean, http200: boolean): PresetID | "custom" {
  if (!body && !http200) return "lite";
  if (!body && http200) return "chain";
  if (body && http200) return "detailed";
  return "custom";
}
const presetCurrent = computed(() =>
  presetFor(captureBody.value, captureHttp200.value),
);
const presetLabel = computed(() =>
  presetCurrent.value === "custom"
    ? t("admin.requestTrace.operator.presets.custom")
    : t(`admin.requestTrace.operator.presets.${presetCurrent.value}`),
);
function applyPreset(id: PresetID) {
  // 预设只填充这两个草稿字段，不触碰范围、采样、体积、时间或总开关，也不保存。
  if (id === "lite") {
    captureBody.value = false;
    captureHttp200.value = false;
  } else if (id === "chain") {
    captureBody.value = false;
    captureHttp200.value = true;
  } else {
    captureBody.value = true;
    captureHttp200.value = true;
  }
}

// --- 选项目录：保留已存/失效条目可选 ---
const groupOptions = computed<AdminGroup[]>(() => {
  const list: AdminGroup[] = [...(props.groups ?? [])];
  const known = new Set(list.map((group) => group.id));
  for (const id of [...(props.status?.group_ids ?? []), ...groupIDs.value]) {
    if (known.has(id)) continue;
    known.add(id);
    list.push({
      id,
      name: `#${id}`,
      platform: "composite",
      status: "inactive",
    } as unknown as AdminGroup);
  }
  return list;
});
const staleGroupIDs = computed(() => {
  const known = new Set((props.groups ?? []).map((group) => group.id));
  return [
    ...new Set([...(props.status?.group_ids ?? []), ...groupIDs.value]),
  ].filter((id) => !known.has(id));
});

const modelOptions = computed(() => {
  const out: string[] = [];
  const seen = new Set<string>();
  for (const model of [
    ...(props.modelCandidates ?? []),
    ...(props.status?.models ?? []),
    ...models.value,
  ]) {
    const value = model.trim();
    if (!value || seen.has(value)) continue;
    seen.add(value);
    out.push(value);
  }
  return out;
});
const staleModels = computed(() => {
  const known = new Set(
    (props.modelCandidates ?? []).map((model) => model.trim()),
  );
  return [
    ...new Set([...(props.status?.models ?? []), ...models.value]),
  ].filter((model) => !known.has(model));
});

const platformCatalog = computed(() => {
  const list = CONCRETE_PLATFORM_OPTIONS.map((platform) => ({
    value: platform.value as string,
    label: platform.label as string,
  }));
  const known = new Set(list.map((platform) => platform.value));
  for (const name of [...(props.status?.platforms ?? []), ...platforms.value]) {
    if (known.has(name)) continue;
    known.add(name);
    list.push({ value: name, label: name });
  }
  return list;
});
const stalePlatforms = computed(() => {
  const known = new Set(
    CONCRETE_PLATFORM_OPTIONS.map((platform) => platform.value as string),
  );
  return [
    ...new Set([...(props.status?.platforms ?? []), ...platforms.value]),
  ].filter((name) => !known.has(name));
});

const staleOptionNotice = computed(() => {
  const count =
    staleGroupIDs.value.length +
    staleModels.value.length +
    stalePlatforms.value.length;
  return count > 0 ? t("admin.requestTrace.operator.scope.staleNote") : "";
});

function togglePlatform(name: string) {
  platforms.value = platforms.value.includes(name)
    ? platforms.value.filter((item) => item !== name)
    : [...platforms.value, name];
}

// --- 校验、脏状态与提交载荷 ---
const draftValid = computed(() => {
  const sampleValid = (value: number) =>
    Number.isInteger(value) && value >= 0 && value <= 100;
  return (
    sampleValid(sampleRateHttp200.value) &&
    sampleValid(sampleRateOther.value) &&
    (bodyLimitValues as readonly number[]).includes(bodyMaxBytes.value) &&
    (durationValues as readonly number[]).includes(
      captureDurationSeconds.value,
    ) &&
    (allGroups.value || groupIDs.value.length > 0) &&
    (modelScope.value === "all" || models.value.length > 0) &&
    (platformScope.value === "all" || platforms.value.length > 0)
  );
});

function draftFingerprint(): string {
  return JSON.stringify({
    captureBody: captureBody.value,
    captureHttp200: captureHttp200.value,
    sampleRateHttp200: sampleRateHttp200.value,
    sampleRateOther: sampleRateOther.value,
    bodyMaxBytes: bodyMaxBytes.value,
    captureDurationSeconds: captureDurationSeconds.value,
    allGroups: allGroups.value,
    // Inactive dimensions are emptied exactly as `fullPayload` writes them.
    // Otherwise a stale selection left behind after switching back to "all"
    // would report "unsaved changes" for a save that would write nothing.
    groupIDs: allGroups.value ? [] : [...groupIDs.value].sort((a, b) => a - b),
    modelScope: modelScope.value,
    models: modelScope.value === "all" ? [] : [...models.value].sort(),
    platformScope: platformScope.value,
    platforms: platformScope.value === "all" ? [] : [...platforms.value].sort(),
  });
}
function appliedFingerprint(status: RequestTraceOperatorStatus): string {
  return JSON.stringify({
    captureBody: status.capture_body,
    captureHttp200: status.capture_http_200,
    sampleRateHttp200: status.sample_rate_http_200,
    sampleRateOther: status.sample_rate_other,
    bodyMaxBytes: status.body_max_bytes,
    captureDurationSeconds: status.capture_duration_seconds,
    allGroups: status.all_groups,
    groupIDs: [...status.group_ids].sort((a, b) => a - b),
    modelScope: status.model_scope,
    models: [...status.models].sort(),
    platformScope: status.platform_scope,
    platforms: [...status.platforms].sort(),
  });
}
const dirty = computed(
  () =>
    !!props.status && draftFingerprint() !== appliedFingerprint(props.status),
);
const summaryBrief = computed(() =>
  dirty.value
    ? t("admin.requestTrace.operator.actions.dirty")
    : t("admin.requestTrace.operator.actions.synced"),
);

const phraseRequiredForSave = computed(() => props.status?.enabled === true);
const canSave = computed(
  () =>
    !!props.status &&
    !submitting.value &&
    draftValid.value &&
    (!phraseRequiredForSave.value || phraseReady.value),
);
const canEnable = computed(
  () =>
    !!props.status &&
    !submitting.value &&
    props.status.plaintext_capture_supported &&
    !props.status.enabled &&
    draftValid.value &&
    phraseReady.value,
);
/**
 * 重开窗口只对“已开启、部署支持且已有有限时限”的门控有意义：关闭时没有窗口可
 * 重开，“无时限”也没有。开启状态下重开仍需重新逐字输入声明。
 */
const canRenew = computed(
  () =>
    !!props.status &&
    !submitting.value &&
    props.status.enabled === true &&
    props.status.plaintext_capture_supported &&
    captureDurationSeconds.value > 0 &&
    draftValid.value &&
    phraseReady.value,
);

function fullPayload(enabled: boolean) {
  return {
    enabled,
    language: language.value,
    phrase: typedPhrase.value.trim(),
    scope_provided: true as const,
    all_groups: allGroups.value,
    group_ids: allGroups.value ? [] : [...groupIDs.value],
    model_scope: modelScope.value,
    models: modelScope.value === "all" ? [] : [...models.value],
    platform_scope: platformScope.value,
    platforms: platformScope.value === "all" ? [] : [...platforms.value],
    capture_body: captureBody.value,
    capture_http_200: captureHttp200.value,
    sample_rate_http_200: sampleRateHttp200.value,
    sample_rate_other: sampleRateOther.value,
    body_max_bytes: bodyMaxBytes.value,
    capture_duration_seconds: captureDurationSeconds.value,
  };
}

function resetDraft() {
  if (props.status) syncDraft(props.status);
  typedPhrase.value = "";
  savedNotice.value = false;
  errorMessage.value = "";
}

async function submit(payload: Parameters<typeof updateOperatorSettings>[0]) {
  submitting.value = true;
  errorMessage.value = "";
  try {
    const next = await updateOperatorSettings(payload);
    typedPhrase.value = "";
    syncDraft(next);
    savedNotice.value = true;
    emit("updated", next);
  } catch {
    errorMessage.value = t("admin.requestTrace.operator.actions.saveFailed");
  } finally {
    submitting.value = false;
  }
}

/** 保存整个草稿，但保持总开关为已存值。 */
async function save() {
  if (!props.status || !canSave.value) return;
  await submit(fullPayload(props.status.enabled));
}

/** 开启会把草稿一并应用，并由服务端重新计时。 */
async function enable() {
  if (!canEnable.value) return;
  await submit(fullPayload(true));
}

/** 延长窗口是显式动作；普通保存不重新计时。 */
async function renew() {
  if (!props.status || !canRenew.value) return;
  await submit({
    ...fullPayload(props.status.enabled),
    renew_capture_window: true,
  });
}

/**
 * 紧急停止：不依赖无效草稿、候选数据或确认服务，因此只发送开关本身，
 * 不需要风险语句。
 */
async function disable() {
  if (!props.status || submitting.value) return;
  await submit({
    enabled: false,
    language: language.value,
    phrase: "",
  });
}
</script>
