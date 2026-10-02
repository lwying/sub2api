<template>
  <AppLayout>
    <div class="mx-auto max-w-[1400px] space-y-5 pb-8">
      <header class="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 class="text-2xl font-semibold text-gray-950 dark:text-white">
            {{ t("admin.requestTrace.title") }}
          </h1>
          <p class="mt-2 text-sm text-gray-500 dark:text-dark-300">
            {{ t("admin.requestTrace.description") }}
          </p>
        </div>
        <div class="flex items-center gap-2">
          <button
            type="button"
            class="btn btn-secondary"
            data-testid="request-trace-refresh"
            @click="load(page)"
          >
            {{ t("admin.requestTrace.list.refresh") }}
          </button>
          <button
            type="button"
            class="btn btn-primary"
            data-testid="request-trace-settings-open"
            @click="settingsOpen = true"
          >
            {{ t("admin.requestTrace.export.settings.title") }}
          </button>
        </div>
      </header>
      <nav
        class="tabs w-fit"
        role="tablist"
        :aria-label="t('admin.requestTrace.title')"
      >
        <button
          id="trace-records-tab"
          type="button"
          role="tab"
          class="tab"
          :class="{ 'tab-active': activeTab === 'records' }"
          :aria-selected="activeTab === 'records'"
          aria-controls="trace-records-panel"
          data-testid="request-trace-tab-records"
          @click="activeTab = 'records'"
        >
          {{ t("admin.requestTrace.tabs.records") }}
        </button>
        <button
          id="trace-config-tab"
          type="button"
          role="tab"
          class="tab"
          :class="{ 'tab-active': activeTab === 'config' }"
          :aria-selected="activeTab === 'config'"
          aria-controls="trace-config-panel"
          data-testid="request-trace-tab-config"
          @click="activeTab = 'config'"
        >
          {{ t("admin.requestTrace.tabs.config") }}
        </button>
      </nav>
      <section
        id="trace-config-panel"
        v-show="activeTab === 'config'"
        role="tabpanel"
        aria-labelledby="trace-config-tab"
      >
        <RequestTraceOperatorSettings
          :status="captureStatus"
          :loading="captureStatusLoading"
          :groups="groups"
          :model-candidates="modelCandidates"
          :options-loading="optionsLoading"
          :options-error="optionsError"
          @retry-options="loadOptions"
          @retry-status="loadCaptureStatus"
          @updated="captureStatus = $event"
        />
      </section>
      <section
        id="trace-records-panel"
        v-show="activeTab === 'records'"
        role="tabpanel"
        aria-labelledby="trace-records-tab"
        data-testid="request-trace-records-panel"
        class="space-y-5"
      >
        <div
          class="rounded-xl border border-gray-200 bg-white p-4 dark:border-dark-700 dark:bg-dark-800"
        >
          <div class="grid gap-3 md:grid-cols-2 xl:grid-cols-4">
            <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
              <span>{{ t("admin.requestTrace.list.traceId") }}</span>
              <input
                v-model.trim="filters.trace_id"
                data-testid="request-trace-id-filter"
                class="input w-full font-mono"
                maxlength="32"
                autocomplete="off"
              />
            </label>
            <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
              <span>{{ t("admin.requestTrace.list.keyword") }}</span>
              <input
                v-model.trim="filters.q"
                data-testid="request-trace-keyword"
                class="input w-full"
                maxlength="128"
                autocomplete="off"
                :title="t('admin.requestTrace.list.keywordHint')"
              />
            </label>
            <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
              <span>{{ t("admin.requestTrace.list.routeFamily") }}</span>
              <select v-model="filters.route_family" class="input w-full">
                <option value="">{{ t("admin.requestTrace.list.any") }}</option>
                <option value="messages">
                  {{ t("admin.requestTrace.list.messages") }}
                </option>
                <option value="chat_completions">
                  {{ t("admin.requestTrace.list.chat_completions") }}
                </option>
                <option value="responses">
                  {{ t("admin.requestTrace.list.responses") }}
                </option>
              </select>
            </label>
            <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
              <span>{{ t("admin.requestTrace.list.status") }}</span>
              <input
                v-model="filters.client_status"
                class="input w-full"
                type="number"
                min="0"
                max="599"
              />
            </label>
            <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
              <span>{{ t("admin.requestTrace.list.usageLinked") }}</span>
              <select v-model="filters.usage_linked" class="input w-full">
                <option value="">{{ t("admin.requestTrace.list.any") }}</option>
                <option value="true">
                  {{ t("admin.requestTrace.list.linked") }}
                </option>
                <option value="false">
                  {{ t("admin.requestTrace.list.unlinked") }}
                </option>
              </select>
            </label>
            <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
              <span>{{ t("admin.requestTrace.list.usageLogId") }}</span>
              <input
                v-model.trim="filters.usage_log_id"
                data-testid="request-trace-usage-filter"
                class="input w-full font-mono"
                inputmode="numeric"
                autocomplete="off"
              />
            </label>
            <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
              <span>{{ t("admin.requestTrace.list.accountId") }}</span>
              <input
                v-model.trim="filters.account_id"
                data-testid="request-trace-account-filter"
                class="input w-full font-mono"
                inputmode="numeric"
                autocomplete="off"
              />
            </label>
            <!--
            The three request-time facts below are each queried as one concrete
            value **or** as "not observed", never both: a request whose fact was
            never determined is not equal to any value, so the other option would
            silently change the question.
          -->
            <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
              <span>{{ t("admin.requestTrace.list.group") }}</span>
              <select
                v-model="filters.group_mode"
                data-testid="request-trace-group-filter-mode"
                class="input w-full"
              >
                <option value="">{{ t("admin.requestTrace.list.any") }}</option>
                <option value="id">
                  {{ t("admin.requestTrace.list.specificValue") }}
                </option>
                <option value="unknown">
                  {{ t("admin.requestTrace.list.unknownValue") }}
                </option>
              </select>
            </label>
            <label
              v-if="filters.group_mode === 'id'"
              class="space-y-1 text-xs text-gray-600 dark:text-dark-300"
            >
              <span>{{ t("admin.requestTrace.list.groupId") }}</span>
              <Select
                v-model="filters.group_id"
                data-testid="request-trace-group-filter"
                :options="groupOptions"
                :searchable="true"
                :loading="optionsLoading"
                :placeholder="
                  t('admin.requestTrace.list.filterPlaceholder.group')
                "
                :aria-label="t('admin.requestTrace.list.group')"
              />
            </label>
            <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
              <span>{{ t("admin.requestTrace.list.requestedModel") }}</span>
              <select
                v-model="filters.model_mode"
                data-testid="request-trace-model-filter-mode"
                class="input w-full"
              >
                <option value="">{{ t("admin.requestTrace.list.any") }}</option>
                <option value="value">
                  {{ t("admin.requestTrace.list.specificValue") }}
                </option>
                <option value="unknown">
                  {{ t("admin.requestTrace.list.unknownValue") }}
                </option>
              </select>
            </label>
            <label
              v-if="filters.model_mode === 'value'"
              class="space-y-1 text-xs text-gray-600 dark:text-dark-300"
            >
              <span>{{ t("admin.requestTrace.list.modelName") }}</span>
              <Select
                v-model="filters.requested_model"
                data-testid="request-trace-model-filter"
                :options="modelOptions"
                :searchable="true"
                :loading="optionsLoading"
                :placeholder="
                  t('admin.requestTrace.list.filterPlaceholder.model')
                "
                :aria-label="t('admin.requestTrace.list.requestedModel')"
              />
            </label>
            <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
              <span>{{ t("admin.requestTrace.list.platform") }}</span>
              <select
                v-model="filters.platform_mode"
                data-testid="request-trace-platform-filter-mode"
                class="input w-full"
              >
                <option value="">{{ t("admin.requestTrace.list.any") }}</option>
                <option value="value">
                  {{ t("admin.requestTrace.list.specificValue") }}
                </option>
                <option value="unknown">
                  {{ t("admin.requestTrace.list.unknownValue") }}
                </option>
              </select>
            </label>
            <label
              v-if="filters.platform_mode === 'value'"
              class="space-y-1 text-xs text-gray-600 dark:text-dark-300"
            >
              <span>{{ t("admin.requestTrace.list.platformName") }}</span>
              <Select
                v-model="filters.platform_name"
                data-testid="request-trace-platform-filter"
                :options="platformOptions"
                :searchable="true"
                :placeholder="
                  t('admin.requestTrace.list.filterPlaceholder.platform')
                "
                :aria-label="t('admin.requestTrace.list.platform')"
              />
            </label>
            <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
              <span>{{ t("admin.requestTrace.list.user") }}</span>
              <select
                v-model="filters.user_mode"
                data-testid="request-trace-user-mode"
                class="input w-full"
              >
                <option value="">{{ t("admin.requestTrace.list.any") }}</option>
                <option value="id">
                  {{ t("admin.requestTrace.list.specificValue") }}
                </option>
                <option value="unknown">
                  {{ t("admin.requestTrace.list.unknownValue") }}
                </option>
              </select>
            </label>
            <label
              v-if="filters.user_mode === 'id'"
              class="space-y-1 text-xs text-gray-600 dark:text-dark-300"
            >
              <span>{{ t("admin.requestTrace.list.userId") }}</span>
              <input
                v-model.trim="filters.user_id"
                class="input w-full font-mono"
                data-testid="request-trace-user-id"
                inputmode="numeric"
              />
            </label>
            <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
              <span>{{ t("admin.requestTrace.list.apiKey") }}</span>
              <select
                v-model="filters.api_key_mode"
                data-testid="request-trace-key-mode"
                class="input w-full"
              >
                <option value="">{{ t("admin.requestTrace.list.any") }}</option>
                <option value="id">
                  {{ t("admin.requestTrace.list.specificValue") }}
                </option>
                <option value="unknown">
                  {{ t("admin.requestTrace.list.unknownValue") }}
                </option>
              </select>
            </label>
            <label
              v-if="filters.api_key_mode === 'id'"
              class="space-y-1 text-xs text-gray-600 dark:text-dark-300"
            >
              <span>{{ t("admin.requestTrace.list.apiKeyId") }}</span>
              <input
                v-model.trim="filters.api_key_id"
                class="input w-full font-mono"
                data-testid="request-trace-key-id"
                inputmode="numeric"
              />
            </label>
            <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
              <span>{{ t("admin.requestTrace.list.from") }}</span>
              <input
                v-model="filters.created_from"
                class="input w-full"
                type="datetime-local"
              />
            </label>
            <label class="space-y-1 text-xs text-gray-600 dark:text-dark-300">
              <span>{{ t("admin.requestTrace.list.to") }}</span>
              <input
                v-model="filters.created_to"
                class="input w-full"
                type="datetime-local"
              />
            </label>
          </div>
          <p
            v-if="optionsLoading"
            role="status"
            class="mt-3 text-xs text-gray-500 dark:text-dark-300"
          >
            {{ t("admin.requestTrace.list.candidates.loading") }}
          </p>
          <p
            v-else-if="optionsError"
            role="alert"
            class="mt-3 text-xs text-amber-700 dark:text-amber-300"
          >
            {{ optionsError }}
            <button
              type="button"
              class="underline underline-offset-2"
              @click="loadOptions"
            >
              {{ t("admin.requestTrace.list.candidates.retry") }}
            </button>
          </p>
          <div class="mt-3 flex flex-wrap items-center gap-3">
            <button
              type="button"
              class="btn btn-primary"
              data-testid="request-trace-search"
              @click="search"
            >
              {{ t("admin.requestTrace.list.search") }}
            </button>
            <!--
            The export actions sit beside the query, because they export the
            query: there is no second filter form to fill in, and no way to
            export a scope that was never executed.
          -->
            <button
              type="button"
              class="btn btn-secondary"
              data-testid="request-trace-export-selected"
              :disabled="
                creating ||
                selectionOverBound ||
                selectedCount === 0 ||
                exportBlocked
              "
              @click="exportSelected"
            >
              {{
                creating
                  ? t("admin.requestTrace.export.action.creating")
                  : t("admin.requestTrace.export.action.selected", {
                      count: selectedCount,
                    })
              }}
            </button>
            <button
              type="button"
              class="btn btn-secondary"
              data-testid="request-trace-export-all"
              :disabled="creating || exportBlocked"
              @click="exportAll"
            >
              {{
                creating
                  ? t("admin.requestTrace.export.action.creating")
                  : t("admin.requestTrace.export.action.all")
              }}
            </button>
            <button
              type="button"
              class="btn btn-secondary text-red-700 dark:text-red-300"
              data-testid="request-trace-delete-selected"
              :disabled="
                deleting ||
                cleanupPreviewing ||
                selectionOverBound ||
                selectedCount === 0
              "
              @click="prepareSelectedCleanup"
            >
              {{ t("admin.requestTrace.list.cleanupActions.selectedAction") }}
            </button>
            <button
              type="button"
              class="btn btn-secondary text-red-700 dark:text-red-300"
              data-testid="request-trace-delete-filtered"
              :disabled="deleting || cleanupPreviewing || !canCleanupFilter"
              @click="prepareFilterCleanup"
            >
              {{ t("admin.requestTrace.list.cleanupActions.filteredAction") }}
            </button>
            <span
              class="text-xs text-gray-500 dark:text-dark-300"
              data-testid="request-trace-export-selection-count"
            >
              {{
                t("admin.requestTrace.export.action.selectionCount", {
                  count: selectedCount,
                })
              }}
            </span>
            <button
              v-if="selectedCount > 0"
              type="button"
              class="btn btn-secondary btn-sm"
              data-testid="request-trace-export-clear-selection"
              @click="clearSelection"
            >
              {{ t("admin.requestTrace.export.action.clearSelection") }}
            </button>
          </div>
          <p
            v-if="cleanupMessage"
            role="status"
            data-testid="request-trace-cleanup-feedback"
            class="mt-3 text-sm"
            :class="
              cleanupFailed
                ? 'text-red-700 dark:text-red-300'
                : 'text-primary-700 dark:text-primary-300'
            "
          >
            {{ cleanupMessage }}
          </p>
          <!--
          The scope line states the filter set the export will carry: the last one
          that actually ran. Edits made in the form since then are not a scope
          until a query succeeds, and the note below says so out loud.
        -->
          <p
            class="mt-3 text-xs text-gray-600 dark:text-dark-300"
            data-testid="request-trace-export-scope"
          >
            {{ t("admin.requestTrace.export.scope.heading") }}:
            {{ executedScopeSummary }}
          </p>
          <p
            v-if="draftDiffers"
            class="mt-1 text-xs text-amber-700 dark:text-amber-300"
            data-testid="request-trace-export-draft-note"
          >
            {{ t("admin.requestTrace.export.scope.draftPending") }}
          </p>
          <p
            v-if="selectionOverBound"
            class="mt-1 text-xs text-amber-700 dark:text-amber-300"
            data-testid="request-trace-export-selection-over-bound"
          >
            {{
              t("admin.requestTrace.export.action.overBound", {
                count: selectedCount,
                max: maxSelectedTraces,
              })
            }}
          </p>
          <p
            v-if="riskAcknowledged === false"
            class="mt-1 text-xs text-amber-700 dark:text-amber-300"
            data-testid="request-trace-export-risk-note"
          >
            {{ t("admin.requestTrace.export.action.riskRequired") }}
            <button
              type="button"
              class="underline"
              data-testid="request-trace-export-risk-link"
              @click="settingsOpen = true"
            >
              {{ t("admin.requestTrace.export.action.riskLink") }}
            </button>
            <button
              type="button"
              class="ml-2 underline"
              data-testid="request-trace-export-risk-recheck"
              @click="loadExportRisk"
            >
              {{ t("admin.requestTrace.export.action.riskRecheck") }}
            </button>
          </p>
          <p
            v-if="refusal"
            role="alert"
            class="mt-1 text-xs text-red-700 dark:text-red-300"
            data-testid="request-trace-export-refusal"
          >
            {{ t(`admin.requestTrace.export.refusal.${refusal}`) }}
          </p>
          <!--
          The task handle must outlive the page, not the tab. This affordance asks
          the server for the export tasks **this admin login session** created, so
          leaving or refreshing the page still finds them; nothing is kept in the
          browser, no id is cached, and another session asks and gets nothing.
        -->
          <div class="mt-3 border-t border-gray-200 pt-3 dark:border-dark-700">
            <button
              type="button"
              class="btn btn-secondary btn-sm"
              data-testid="request-trace-export-recall-toggle"
              @click="toggleRecall"
            >
              {{ t("admin.requestTrace.export.task.title") }}
              <!--
              The count is how many tasks are **loaded**, not how many exist: one
              page is bounded, so while the server still offers a next page the
              number is prefixed with a plus sign. A bare "20" would read as
              "you have twenty tasks" and hide the rest.
            -->
              <span
                v-if="recallTasks.length"
                class="ml-1"
                data-testid="request-trace-export-recall-count"
                >{{ recallTasks.length }}{{ recallCursor ? "+" : "" }}</span
              >
            </button>
            <template v-if="recallOpen">
              <p
                v-if="recallLoading"
                role="status"
                class="mt-2 text-xs text-gray-500 dark:text-dark-300"
                data-testid="request-trace-export-recall-loading"
              >
                {{ t("admin.requestTrace.export.task.loading") }}
              </p>
              <template v-else-if="recallFailed">
                <p
                  role="alert"
                  class="mt-2 text-xs text-amber-700 dark:text-amber-300"
                  data-testid="request-trace-export-recall-failed"
                >
                  {{
                    t(
                      `admin.requestTrace.export.refusal.${recallRefusal ?? "unavailable"}`,
                    )
                  }}
                </p>
                <button
                  type="button"
                  class="mt-2 btn btn-secondary btn-sm"
                  data-testid="request-trace-export-recall-retry"
                  @click="loadRecall"
                >
                  {{ t("admin.requestTrace.export.task.retry") }}
                </button>
              </template>
              <template v-else-if="recallTasks.length">
                <ul
                  class="mt-2 space-y-1"
                  data-testid="request-trace-export-recall-list"
                >
                  <li v-for="recalled in recallTasks" :key="recalled.id">
                    <button
                      type="button"
                      class="w-full rounded-lg px-2 py-1 text-left text-xs hover:bg-gray-100 dark:hover:bg-dark-800"
                      :data-testid="`request-trace-export-recall-task-${recalled.id}`"
                      @click="openExportTask(recalled.id)"
                    >
                      <span class="font-mono">{{
                        recalled.id.slice(0, 8)
                      }}</span>
                      <span class="ml-2">{{
                        t(recallStateKey(recallState(recalled)))
                      }}</span>
                      <span class="ml-2 text-gray-500 dark:text-dark-300"
                        >{{ t("admin.requestTrace.export.progress.created") }}:
                        {{ formatRecallDate(recalled.created_at) }}</span
                      >
                      <span
                        v-if="recalled.download_until"
                        class="ml-2 text-gray-500 dark:text-dark-300"
                        >{{
                          t("admin.requestTrace.export.progress.downloadUntil")
                        }}:
                        {{ formatRecallDate(recalled.download_until) }}</span
                      >
                    </button>
                  </li>
                </ul>
                <!--
                A page holds a bounded number of tasks, so "one page" is not
                "all of them": the server hands out an opaque token for the page
                after this one, and it is the only way the oldest task stays
                reachable. The token is never parsed or stored here; it is passed
                straight back, so a row deleted in the meantime cannot make the
                walk repeat or skip.
              -->
                <button
                  v-if="recallCursor"
                  type="button"
                  class="mt-2 btn btn-secondary btn-sm"
                  data-testid="request-trace-export-recall-more"
                  :disabled="recallMoreLoading"
                  @click="loadMoreRecall"
                >
                  {{ t("admin.requestTrace.export.task.loadMore") }}
                </button>
                <!--
                A failed next page is not a refusal and not "no tasks": the rows
                already listed stay exactly as they are, and the token is kept so
                the same page can be asked for again.
              -->
                <p
                  v-if="recallMoreFailed"
                  role="alert"
                  class="mt-1 text-xs text-amber-700 dark:text-amber-300"
                  data-testid="request-trace-export-recall-more-failed"
                >
                  {{ t("admin.requestTrace.export.task.loadMoreFailed") }}
                </p>
              </template>
              <p
                v-else
                class="mt-2 text-xs text-gray-500 dark:text-dark-300"
                data-testid="request-trace-export-recall-empty"
              >
                {{ t("admin.requestTrace.export.task.absent") }}
              </p>
            </template>
          </div>
        </div>
        <p
          v-if="captureStatus && !captureStatus.capture_allowed"
          class="rounded-lg border border-amber-200 bg-amber-50 p-3 text-sm text-amber-800 dark:border-amber-900 dark:bg-amber-950/20 dark:text-amber-200"
          data-testid="request-trace-capture-disabled"
        >
          {{ t("admin.requestTrace.list.captureDisabled") }}
        </p>
        <p
          v-else-if="captureStatus === null"
          class="text-xs text-gray-500 dark:text-dark-300"
          data-testid="request-trace-capture-unknown"
        >
          {{ t("admin.requestTrace.list.captureUnknown") }}
        </p>
        <section
          v-if="queryStats"
          class="rounded-xl border border-gray-200 bg-white p-4 dark:border-dark-700 dark:bg-dark-800"
          data-testid="request-trace-query-stats"
        >
          <div class="mb-3 flex items-baseline justify-between gap-3">
            <h2 class="text-sm font-semibold text-gray-900 dark:text-white">
              {{ t("admin.requestTrace.list.queryStats") }}
            </h2>
            <span class="text-xs text-gray-500 dark:text-dark-300">{{
              executedScopeSummary
            }}</span>
          </div>
          <div class="grid gap-3 text-sm sm:grid-cols-2 lg:grid-cols-4">
            <div class="rounded-lg bg-gray-50 p-3 dark:bg-dark-900">
              <span class="block text-xs text-gray-500">{{
                t("admin.requestTrace.list.statsTotal")
              }}</span
              ><strong class="text-xl">{{ queryStats.matched_total }}</strong>
            </div>
            <div class="rounded-lg bg-gray-50 p-3 dark:bg-dark-900">
              <span class="block text-xs text-gray-500">{{
                t("admin.requestTrace.list.statsStatus")
              }}</span
              ><span class="block"
                >2xx {{ queryStats.status["2xx"] }} · 3xx
                {{ queryStats.status["3xx"] }} · 4xx
                {{ queryStats.status["4xx"] }} · 5xx
                {{ queryStats.status["5xx"] }} ·
                {{ t("admin.requestTrace.list.statsOther") }}
                {{ queryStats.status.other }}</span
              >
            </div>
            <div class="rounded-lg bg-gray-50 p-3 dark:bg-dark-900">
              <span class="block text-xs text-gray-500">{{
                t("admin.requestTrace.list.statsCapture")
              }}</span
              ><span class="block"
                >{{ captureStateLabel(t, "stored") }}
                {{ queryStats.capture.stored }} ·
                {{ captureStateLabel(t, "partial") }}
                {{ queryStats.capture.partial }} ·
                {{ captureStateLabel(t, "not_observed") }}
                {{ queryStats.capture.not_observed }} ·
                {{ captureStateLabel(t, "write_failed") }}
                {{ queryStats.capture.write_failed }}</span
              >
            </div>
            <div class="rounded-lg bg-gray-50 p-3 dark:bg-dark-900">
              <span class="block text-xs text-gray-500">{{
                t("admin.requestTrace.list.statsUsage")
              }}</span
              ><span class="block"
                >{{ t("admin.requestTrace.list.linked") }}
                {{ queryStats.usage.linked }} ·
                {{ t("admin.requestTrace.list.unlinked") }}
                {{ queryStats.usage.unlinked }}</span
              >
            </div>
          </div>
        </section>
        <p
          v-if="filterError"
          role="alert"
          class="rounded-lg border border-amber-200 bg-amber-50 p-4 text-sm text-amber-700 dark:border-amber-900 dark:bg-amber-950/20"
          data-testid="request-trace-filter-error"
        >
          {{ t("admin.requestTrace.list.invalidFilter") }}
        </p>
        <p
          v-if="failed"
          role="alert"
          class="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700 dark:border-red-900 dark:bg-red-950/20"
          data-testid="request-trace-error"
        >
          {{ t("admin.requestTrace.list.failed") }}
        </p>
        <p
          v-else-if="loading && !rows.length"
          role="status"
          class="py-8 text-center text-sm"
          data-testid="request-trace-loading"
        >
          {{ t("admin.requestTrace.list.loading") }}
        </p>
        <p
          v-else-if="!rows.length"
          role="status"
          class="rounded-lg border border-gray-200 bg-gray-50 p-4 text-sm dark:border-dark-700 dark:bg-dark-900"
          data-testid="request-trace-empty"
        >
          {{ t("admin.requestTrace.list.empty") }}
        </p>
        <template v-else>
          <div
            class="overflow-x-auto rounded-xl border border-gray-200 dark:border-dark-700"
          >
            <table
              class="min-w-full divide-y divide-gray-200 text-sm dark:divide-dark-700"
            >
              <thead
                class="bg-gray-50 text-left text-xs text-gray-500 dark:bg-dark-900"
              >
                <tr>
                  <th class="px-3 py-3">
                    {{ t("admin.requestTrace.list.select") }}
                  </th>
                  <th class="px-3 py-3">
                    {{ t("admin.requestTrace.list.createdAt") }}
                  </th>
                  <th class="px-3 py-3">
                    {{ t("admin.requestTrace.list.traceId") }}
                  </th>
                  <th class="px-3 py-3">
                    {{ t("admin.requestTrace.list.route") }}
                  </th>
                  <th class="px-3 py-3">
                    {{ t("admin.requestTrace.list.user") }}
                  </th>
                  <th class="px-3 py-3">
                    {{ t("admin.requestTrace.list.apiKey") }}
                  </th>
                  <th class="px-3 py-3">
                    {{ t("admin.requestTrace.list.group") }}
                  </th>
                  <th class="px-3 py-3">
                    {{ t("admin.requestTrace.list.requestedModel") }}
                  </th>
                  <th class="px-3 py-3">
                    {{ t("admin.requestTrace.list.status") }}
                  </th>
                  <th class="px-3 py-3">
                    {{ t("admin.requestTrace.list.state") }}
                  </th>
                  <th class="px-3 py-3">
                    {{ t("admin.requestTrace.list.cleanup") }}
                  </th>
                  <th class="px-3 py-3">
                    {{ t("admin.requestTrace.list.usage") }}
                  </th>
                </tr>
              </thead>
              <tbody
                class="divide-y divide-gray-200 bg-white dark:divide-dark-700 dark:bg-dark-800"
              >
                <tr
                  v-for="row in rows"
                  :key="row.trace_id"
                  data-testid="request-trace-row"
                  class="cursor-pointer transition-colors hover:bg-gray-50 focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500 dark:hover:bg-dark-700"
                  tabindex="0"
                  :aria-label="`${t('admin.requestTrace.detail.title')} ${row.trace_id}`"
                  @click="openDetail(row.trace_id)"
                  @keydown.enter="openDetail(row.trace_id)"
                  @keydown.space.prevent="openDetail(row.trace_id)"
                >
                  <!--
                  Selection is by Trace ID, not by row position, so a checked row
                  stays checked when the same query is paged through.
                -->
                  <td class="px-3 py-3">
                    <input
                      type="checkbox"
                      :checked="isSelected(row.trace_id)"
                      :data-testid="`request-trace-select-${row.trace_id}`"
                      :aria-label="t('admin.requestTrace.list.select')"
                      @click.stop
                      @keydown.stop
                      @change.stop="toggleSelected(row.trace_id)"
                    />
                  </td>
                  <td class="whitespace-nowrap px-3 py-3 text-xs">
                    {{ formatDate(row.created_at) }}
                  </td>
                  <td class="px-3 py-3 font-mono text-xs">
                    {{ row.trace_id }}
                  </td>
                  <td class="px-3 py-3 text-xs">{{ row.inbound_endpoint }}</td>
                  <td
                    class="px-3 py-3 text-xs"
                    data-testid="request-trace-user"
                  >
                    {{ identityLabel(row.user_id, row.user_email) }}
                  </td>
                  <td class="px-3 py-3 text-xs" data-testid="request-trace-key">
                    {{ identityLabel(row.api_key_id, row.api_key_name) }}
                  </td>
                  <!-- Request-time facts. Absent means not observed; it is never a blank cell or a guess. -->
                  <td
                    class="px-3 py-3 font-mono text-xs"
                    data-testid="request-trace-row-group"
                  >
                    {{
                      row.group_id == null
                        ? t("admin.requestTrace.list.unknownValue")
                        : `#${row.group_id}`
                    }}
                  </td>
                  <td
                    class="px-3 py-3 font-mono text-xs"
                    data-testid="request-trace-row-model"
                  >
                    {{
                      row.requested_model
                        ? row.requested_model
                        : t("admin.requestTrace.list.unknownValue")
                    }}
                  </td>
                  <td class="px-3 py-3 font-mono">
                    {{ row.client_status || "—" }}
                  </td>
                  <td
                    class="px-3 py-3 text-xs"
                    data-testid="request-trace-capture-state"
                  >
                    {{ captureStateLabel(t, row.capture_state) }}
                  </td>
                  <td
                    class="max-w-xs px-3 py-3 text-xs"
                    data-testid="request-trace-cleanup-rule"
                  >
                    {{
                      row.usage_log_id
                        ? t("admin.requestTrace.list.followsUsage")
                        : row.cleanup_after
                          ? t("admin.requestTrace.list.plannedCleanup", {
                              date: formatDate(row.cleanup_after),
                            })
                          : "—"
                    }}
                  </td>
                  <td class="px-3 py-3 font-mono text-xs">
                    {{
                      row.usage_log_id
                        ? `#${row.usage_log_id}`
                        : t("admin.requestTrace.list.usageAbsent")
                    }}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <Pagination
            :total="total"
            :page="page"
            :page-size="pageSize"
            @update:page="load"
            @update:page-size="changePageSize"
          />
        </template>
        <RequestTraceDetailDrawer
          :show="detailOpen"
          :trace-id="selectedID"
          @update:show="onDetailVisibility"
        />
        <h2 class="text-sm font-semibold text-gray-900 dark:text-white">
          {{ t("admin.requestTrace.list.runtimeStats") }}
        </h2>
        <RequestTraceOpsStatusPanel />
      </section>
      <ConfirmDialog
        :show="cleanupOpen"
        :title="
          t(
            cleanupMode === 'selected'
              ? 'admin.requestTrace.list.cleanupActions.selectedTitle'
              : 'admin.requestTrace.list.cleanupActions.previewTitle',
          )
        "
        :message="cleanupConfirmationMessage"
        :confirm-text="
          t(
            deleting
              ? 'admin.requestTrace.list.cleanupActions.deleting'
              : 'admin.requestTrace.list.cleanupActions.confirm',
          )
        "
        :cancel-text="t('admin.requestTrace.list.cleanupActions.cancel')"
        danger
        @confirm="confirmCleanup"
        @cancel="closeCleanup"
      >
        <p
          v-if="cleanupMode === 'filter'"
          class="text-xs text-gray-600 dark:text-dark-300"
        >
          {{ cleanupScopeSummary }}
        </p>
        <p class="text-xs text-gray-500 dark:text-dark-400">
          {{ t("admin.requestTrace.list.cleanupActions.immutableNote") }}
        </p>
        <p
          v-if="cleanupMode === 'filter'"
          class="text-xs text-gray-500 dark:text-dark-400"
        >
          {{ t("admin.requestTrace.list.cleanupActions.snapshotNote") }}
        </p>
      </ConfirmDialog>
      <RequestTraceSettingsDialog
        :show="settingsOpen"
        @update:show="onSettingsVisibility"
      />
      <RequestTraceExportDrawer
        :show="exportOpen"
        :task-id="exportID"
        @update:show="onExportVisibility"
      />
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from "vue";
import { useI18n } from "vue-i18n";
import { useRoute, useRouter } from "vue-router";
import AppLayout from "@/components/layout/AppLayout.vue";
import Pagination from "@/components/common/Pagination.vue";
import Select from "@/components/common/Select.vue";
import ConfirmDialog from "@/components/common/ConfirmDialog.vue";
import { getAllIncludingInactive } from "@/api/admin/groups";
import { CONCRETE_PLATFORM_OPTIONS } from "@/constants/platforms";
import type { AdminGroup } from "@/types";
import RequestTraceOperatorSettings from "./RequestTraceOperatorSettings.vue";
import RequestTraceDetailDrawer from "./RequestTraceDetailDrawer.vue";
import RequestTraceExportDrawer from "./RequestTraceExportDrawer.vue";
import RequestTraceOpsStatusPanel from "./RequestTraceOpsStatusPanel.vue";
import RequestTraceSettingsDialog from "./RequestTraceSettingsDialog.vue";
import {
  TraceExportRefusedError,
  createTraceExport,
  getOperatorSettings,
  getTraceModelCandidates,
  previewTraceDelete,
  deleteSelectedTraces,
  deleteTracesByFilter,
  getTraceExportRisk,
  listTraceExports,
  listTraces,
} from "./api";
import { captureStateLabel, exportScopeSummary } from "./labels";
import {
  requestTraceExportDisplayStates,
  requestTraceExportIDPattern,
  requestTraceExportMaxSelectedTraces,
  type RequestTraceExportDisplayState,
  type RequestTraceExportFilter,
  type RequestTraceExportRefusal,
  type RequestTraceExportTask,
  type RequestTraceListParams,
  type RequestTraceSummary,
  type RequestTraceQueryStats,
  type RequestTraceOperatorStatus,
  type RequestTraceDeletePreview,
} from "./types";

const { t } = useI18n();
const route = useRoute();
const router = useRouter();
const rows = ref<RequestTraceSummary[]>([]);
const page = ref(1);
const pageSize = ref(20);
const total = ref(0);
const queryStats = ref<RequestTraceQueryStats | null>(null);
const captureStatus = ref<RequestTraceOperatorStatus | null>(null);
const captureStatusLoading = ref(true);
const settingsOpen = ref(false);
const activeTab = ref<"records" | "config">("records");
const groups = ref<AdminGroup[]>([]);
const modelCandidates = ref<string[]>([]);
const optionsLoading = ref(false);
const optionsError = ref("");
let optionsRevision = 0;
const groupOptions = computed(() => {
  const options = new Map(
    groups.value.map((group) => [
      String(group.id),
      { value: String(group.id), label: `${group.name} (${group.platform})` },
    ]),
  );
  for (const id of [
    ...rows.value.map((row) => row.group_id),
    ...(captureStatus.value?.group_ids ?? []),
    Number(filters.group_id),
  ]) {
    if (id && !options.has(String(id)))
      options.set(String(id), { value: String(id), label: `#${id}` });
  }
  return [...options.values()];
});
const modelOptions = computed(() =>
  [
    ...new Set(
      [
        ...modelCandidates.value,
        ...rows.value.map((row) => row.requested_model ?? ""),
        ...(captureStatus.value?.models ?? []),
        filters.requested_model,
      ].filter(Boolean),
    ),
  ].map((value) => ({ value, label: value })),
);
const platformOptions = computed(() => {
  const options = new Map<string, { value: string; label: string }>(
    CONCRETE_PLATFORM_OPTIONS.map((option) => [
      option.value as string,
      { value: option.value, label: option.label },
    ]),
  );
  for (const value of [
    ...rows.value.flatMap((row) => row.observed_platforms ?? []),
    ...(captureStatus.value?.platforms ?? []),
    filters.platform_name,
  ].filter(Boolean)) {
    if (!options.has(value)) options.set(value, { value, label: value });
  }
  return [...options.values()];
});
const failed = ref(false);
const filterError = ref(false);
const loading = ref(false);
const detailOpen = ref(false);
const selectedID = ref<string | null>(null);
const filters = reactive({
  trace_id: "",
  q: "",
  route_family: "",
  client_status: "",
  usage_linked: "",
  account_id: "",
  usage_log_id: "",
  user_mode: "",
  user_id: "",
  api_key_mode: "",
  api_key_id: "",
  // Each request-time fact is chosen as "any" (no condition), one concrete value,
  // or "not observed". One mode per fact keeps the two mutually exclusive
  // conditions the server refuses to be selected at the same time.
  group_mode: "",
  group_id: "",
  model_mode: "",
  requested_model: "",
  platform_mode: "",
  platform_name: "",
  created_from: "",
  created_to: "",
});
/**
 * `executedFilters` is the set that actually ran and produced what is on screen:
 * it is what an export carries, and what the scope line shows. `queryFilters` is
 * the set the table is currently asking for, which only becomes executed once a
 * load succeeds — a form edit, or a query that failed, is not a scope.
 */
const executedFilters = ref<Partial<RequestTraceListParams>>({});
const queryHasRun = ref(false);
const cleanupOpen = ref(false);
const cleanupMode = ref<"selected" | "filter">("selected");
const cleanupIDs = ref<string[]>([]);
const cleanupFilter = ref<RequestTraceExportFilter>({});
const cleanupPreview = ref<RequestTraceDeletePreview | null>(null);
const cleanupPreviewing = ref(false);
const deleting = ref(false);
const cleanupMessage = ref("");
const cleanupFailed = ref(false);
let cleanupRevision = 0;
const canCleanupFilter = computed(
  () =>
    queryHasRun.value &&
    !failed.value &&
    !loading.value &&
    Object.keys(executedFilters.value).length > 0,
);
const cleanupScopeSummary = computed(() =>
  exportScopeSummary(t, cleanupFilter.value),
);
const cleanupConfirmationMessage = computed(() =>
  cleanupMode.value === "selected"
    ? t("admin.requestTrace.list.cleanupActions.selectedMessage", {
        count: cleanupIDs.value.length,
      })
    : t("admin.requestTrace.list.cleanupActions.previewCount", {
        count: cleanupPreview.value?.matched_count ?? 0,
      }),
);
let queryFilters: Partial<RequestTraceListParams> = {};
/** A newly searched query drops the old cross-page selection, but only once it succeeds. */
let pendingSelectionReset = false;
let revision = 0;
let controller: AbortController | null = null;
let createController: AbortController | null = null;

const maxSelectedTraces = requestTraceExportMaxSelectedTraces;
/** Ordered by selection, so the exported set is what the operator checked, in order. */
const selectedIDs = ref<string[]>([]);
const selectedCount = computed(() => selectedIDs.value.length);
const selectionOverBound = computed(
  () => selectedCount.value > maxSelectedTraces,
);
const exportOpen = ref(false);
const exportID = ref<string | null>(null);
const creating = ref(false);
const refusal = ref<RequestTraceExportRefusal | null>(null);
/**
 * `null` means the risk state could not be read. An unreadable state is not
 * "unacknowledged": it blocks nothing and claims nothing.
 */
const riskAcknowledged = ref<boolean | null>(null);
const executedScopeSummary = computed(() =>
  exportScopeSummary(t, executedFilters.value as RequestTraceExportFilter),
);
const exportBlocked = computed(() => riskAcknowledged.value === false);

/**
 * The recall affordance: the export tasks the server says belong to this admin
 * login session. It holds no handle of its own — no task id in storage, no
 * cached list — so "find my task again" is answered by the session, not by the
 * browser. `recallOpen` only controls whether the list is shown.
 *
 * One page is bounded, so `recallCursor` is the server's token for the page
 * after the one on screen, or null when the server said there is none. The
 * token lives in memory only: it is passed straight back to the server, which
 * is what keeps the oldest task reachable without the browser remembering it.
 */
const recallOpen = ref(false);
const recallLoading = ref(false);
const recallFailed = ref(false);
const recallRefusal = ref<RequestTraceExportRefusal | null>(null);
const recallTasks = ref<RequestTraceExportTask[]>([]);
const recallCursor = ref<string | null>(null);
const recallMoreLoading = ref(false);
const recallMoreFailed = ref(false);
let recallController: AbortController | null = null;
/**
 * Every read of this session's tasks — first page or next page — takes a new
 * epoch, and only the newest one owns what is on screen: to set the list, to
 * raise a failure, and to clear a loading flag. Without it, a request that a
 * refresh aborted would clear the loading state of the read that replaced it
 * (leaving a button clickable mid-flight, or a spinner stuck), and a page that
 * arrived late would be appended into a list it no longer belongs to.
 */
let recallEpoch = 0;

const recallStateKeys = new Set<string>(requestTraceExportDisplayStates);

/**
 * The one reason code that says the manifest itself could not be read: the file
 * that carries the shard list and the completeness verdict is the file that is
 * gone, so nothing is known about what this export delivered. It is neither
 * "done" nor a smaller success, and it gets its own label (`file_lost`) rather
 * than borrowing the truncation one — the server never sends a truncation
 * without a reason from its closed set, so this code is unambiguous.
 */
const recallManifestLostReason = "manifest_lost";

/**
 * The same state resolution the task drawer uses: incompleteness is not success,
 * and neither is a delivery whose own record is unreadable. A row is only ever
 * 'completed' when the server reported it complete — never because a field was
 * missing.
 */
function recallState(
  task: RequestTraceExportTask,
): RequestTraceExportDisplayState {
  if (task.status !== "completed") return task.status;
  if (task.truncated && task.incomplete_reason === recallManifestLostReason)
    return "file_lost";
  if (task.truncated) return "incomplete";
  return task.downloadable ? "completed" : "expired";
}

/** The one key a recalled state renders through; an unknown state gets no label of its own. */
function recallStateKey(state: RequestTraceExportDisplayState): string {
  return `admin.requestTrace.export.state.${recallStateKeys.has(state) ? state : "unknown"}`;
}

/**
 * The bounded outcome a refused recall carries. It is read as a property, not
 * through a class check: anything else (including a malformed payload) has no
 * bounded outcome of its own and stays "unavailable".
 */
function recallRefusalOf(error: unknown): RequestTraceExportRefusal | null {
  const refusal = (error as { refusal?: unknown } | null)?.refusal;
  return typeof refusal === "string"
    ? (refusal as RequestTraceExportRefusal)
    : null;
}

function formatRecallDate(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}

/**
 * Reads the first page of the session's tasks. A refusal is a bounded outcome:
 * an admin API key or another session is told so, and it is never rendered as
 * "no tasks" — those are different facts. A malformed payload is a transport
 * failure, which the API layer raises as one rather than folding into an empty
 * list.
 */
async function loadRecall() {
  recallController?.abort();
  const controller = new AbortController();
  recallController = controller;
  const epoch = ++recallEpoch;
  recallLoading.value = true;
  recallFailed.value = false;
  recallMoreFailed.value = false;
  // A fresh read owns the whole panel, so any next page that was in flight is
  // no longer loading anything.
  recallMoreLoading.value = false;
  recallRefusal.value = null;
  try {
    const page = await listTraceExports({ signal: controller.signal });
    if (epoch !== recallEpoch) return;
    recallTasks.value = page.items;
    recallCursor.value = page.nextCursor;
    recallFailed.value = false;
  } catch (error) {
    if (epoch !== recallEpoch || controller.signal.aborted) return;
    recallTasks.value = [];
    recallCursor.value = null;
    recallFailed.value = true;
    recallRefusal.value = recallRefusalOf(error);
  } finally {
    if (epoch === recallEpoch) recallLoading.value = false;
  }
}

/**
 * Reads the page after the one on screen and appends it, so a session with more
 * tasks than one page can hold does not lose its oldest ones.
 *
 * A failed next page leaves the list exactly as it was and keeps the token: the
 * tasks already read are still true, the failure is shown as itself, and the
 * same page can be asked for again. A token that does not move forward is not a
 * page at all — the walk stops instead of re-reading the same rows forever.
 */
async function loadMoreRecall() {
  const cursor = recallCursor.value;
  if (cursor === null || recallMoreLoading.value) return;
  const controller = recallController;
  const epoch = ++recallEpoch;
  recallMoreLoading.value = true;
  recallMoreFailed.value = false;
  try {
    const page = await listTraceExports({ cursor, signal: controller?.signal });
    // A page that arrives after a newer read started belongs to a list that is
    // no longer on screen; appending it would mix two reads together.
    if (epoch !== recallEpoch) return;
    recallTasks.value = appendRecallPage(recallTasks.value, page.items);
    recallCursor.value = page.nextCursor === cursor ? null : page.nextCursor;
  } catch {
    // An aborted walk is not a failure: the caller that aborted it owns what
    // happens next.
    if (
      epoch !== recallEpoch ||
      controller === null ||
      controller.signal.aborted
    )
      return;
    recallMoreFailed.value = true;
  } finally {
    if (epoch === recallEpoch) recallMoreLoading.value = false;
  }
}

/**
 * Appends one page to the tasks already read.
 *
 * The server walks the session's tasks with a key-set boundary, so its pages are
 * disjoint by construction and a task is normally never returned twice. If one
 * ever is, the newer copy replaces the older one instead of being rendered
 * beside it: one task id is one row, and two rows sharing a `:key` are two rows
 * the renderer cannot tell apart.
 */
function appendRecallPage(
  loaded: RequestTraceExportTask[],
  page: RequestTraceExportTask[],
): RequestTraceExportTask[] {
  if (page.length === 0) return loaded;
  const merged = loaded.slice();
  const indexByID = new Map(merged.map((task, index) => [task.id, index]));
  for (const task of page) {
    const index = indexByID.get(task.id);
    if (index === undefined) {
      indexByID.set(task.id, merged.length);
      merged.push(task);
    } else {
      merged[index] = task;
    }
  }
  return merged;
}

function toggleRecall() {
  recallOpen.value = !recallOpen.value;
  // Opening re-asks the server: a refresh is the whole point of this control.
  if (recallOpen.value) void loadRecall();
}

/** A lookup id is only a filter when it is a positive integer; anything else is not a filter. */
function parseLookupID(raw: string): number | null {
  if (!/^[0-9]+$/.test(raw)) return null;
  const value = Number(raw);
  return Number.isSafeInteger(value) && value > 0 ? value : null;
}

function routeQueryString(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

/**
 * A usage row can jump here with `?usage_log_id=<id>`. Applying that lookup on
 * load keeps the navigation meaningful: without it the page would load every
 * Trace and the operator would think the row had been located. Only a positive
 * integer is accepted, so a hand-edited URL cannot turn the filter into "all".
 */
function routePrefillFilters(): Partial<RequestTraceListParams> {
  const next: Partial<RequestTraceListParams> = {};
  const traceID = routeQueryString(route.query.trace_id);
  if (/^[0-9a-f]{32}$/.test(traceID)) {
    filters.trace_id = traceID;
    next.trace_id = traceID;
  }
  const usageLogID = parseLookupID(routeQueryString(route.query.usage_log_id));
  if (usageLogID !== null) {
    filters.usage_log_id = String(usageLogID);
    next.usage_log_id = usageLogID;
  }
  const accountID = parseLookupID(routeQueryString(route.query.account_id));
  if (accountID !== null) {
    filters.account_id = String(accountID);
    next.account_id = accountID;
  }
  return next;
}

function identityLabel(id: number | null, name: string | null): string {
  if (id == null) return t("admin.requestTrace.list.unknownValue");
  return name ? `${name} (#${id})` : `#${id}`;
}

async function loadOptions() {
  const current = ++optionsRevision;
  optionsLoading.value = true;
  optionsError.value = "";
  try {
    const [loadedGroups, loadedModels] = await Promise.all([
      getAllIncludingInactive(),
      getTraceModelCandidates(),
    ]);
    if (current !== optionsRevision) return;
    groups.value = loadedGroups;
    modelCandidates.value = loadedModels;
  } catch {
    if (current === optionsRevision)
      optionsError.value = t("admin.requestTrace.list.candidates.error");
  } finally {
    if (current === optionsRevision) optionsLoading.value = false;
  }
}

async function loadCaptureStatus() {
  captureStatusLoading.value = true;
  try {
    captureStatus.value = await getOperatorSettings();
  } catch {
    captureStatus.value = null;
  } finally {
    captureStatusLoading.value = false;
  }
}

function onSettingsVisibility(show: boolean) {
  settingsOpen.value = show;
  if (!show) {
    void loadCaptureStatus();
    void loadExportRisk();
  }
}

function formatDate(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}

function onDetailVisibility(value: boolean) {
  detailOpen.value = value;
  if (!value) selectedID.value = null;
}

function openDetail(id: string) {
  selectedID.value = id;
  detailOpen.value = true;
}

function search() {
  filterError.value = false;
  const next: Partial<RequestTraceListParams> = {};
  if (filters.trace_id) {
    if (!/^[0-9a-f]{32}$/.test(filters.trace_id)) {
      filterError.value = true;
      return;
    }
    next.trace_id = filters.trace_id;
  }
  if (filters.q) {
    if (
      filters.q.length < 3 ||
      filters.q.length > 128 ||
      filters.q.replace(/[\\%_\s]/g, "").length < 2
    ) {
      filterError.value = true;
      return;
    }
    next.q = filters.q;
  }
  if (
    filters.route_family === "messages" ||
    filters.route_family === "chat_completions" ||
    filters.route_family === "responses"
  )
    next.route_family = filters.route_family;
  if (filters.client_status !== "") {
    const status = Number(filters.client_status);
    if (!Number.isSafeInteger(status) || status < 0 || status > 599) {
      filterError.value = true;
      return;
    }
    next.client_status = status;
  }
  if (filters.usage_linked !== "")
    next.usage_linked = filters.usage_linked === "true";
  for (const [raw, key] of [
    [filters.usage_log_id, "usage_log_id"],
    [filters.account_id, "account_id"],
  ] as const) {
    if (raw === "") continue;
    const id = parseLookupID(raw);
    if (id === null) {
      filterError.value = true;
      return;
    }
    next[key] = id;
  }
  if (filters.group_mode === "id") {
    const id = parseLookupID(filters.group_id);
    if (id === null) {
      filterError.value = true;
      return;
    }
    next.group_id = id;
  } else if (filters.group_mode === "unknown") {
    next.group_unknown = true;
  }
  // A chosen concrete value that is blank is an incomplete filter, not "no
  // filter": sending nothing here would quietly answer a different question.
  if (filters.model_mode === "value") {
    if (filters.requested_model.trim() === "") {
      filterError.value = true;
      return;
    }
    next.requested_model = filters.requested_model.trim();
  } else if (filters.model_mode === "unknown") {
    next.model_unknown = true;
  }
  if (filters.platform_mode === "value") {
    if (filters.platform_name.trim() === "") {
      filterError.value = true;
      return;
    }
    next.platform = filters.platform_name.trim();
  } else if (filters.platform_mode === "unknown") {
    next.platform_unknown = true;
  }
  for (const [mode, raw, idKey, unknownKey] of [
    [filters.user_mode, filters.user_id, "user_id", "user_unknown"],
    [filters.api_key_mode, filters.api_key_id, "api_key_id", "api_key_unknown"],
  ] as const) {
    if (mode === "id") {
      const id = parseLookupID(raw);
      if (id === null) {
        filterError.value = true;
        return;
      }
      if (idKey === "user_id") next.user_id = id;
      else next.api_key_id = id;
    } else if (mode === "unknown") {
      if (unknownKey === "user_unknown") next.user_unknown = true;
      else next.api_key_unknown = true;
    }
  }
  if (filters.created_from) {
    const from = new Date(filters.created_from);
    if (Number.isNaN(from.getTime())) {
      filterError.value = true;
      return;
    }
    next.created_from = from.toISOString();
  }
  if (filters.created_to) {
    const to = new Date(filters.created_to);
    if (Number.isNaN(to.getTime())) {
      filterError.value = true;
      return;
    }
    next.created_to = to.toISOString();
  }
  queryFilters = next;
  // The checked rows belong to the query that found them: a new one clears them,
  // but only after it has actually run, so a failed search does not lose them.
  pendingSelectionReset = true;
  void load(1);
}

function changePageSize(size: number) {
  pageSize.value = size;
  void load(1);
}

/**
 * Whether the form holds edits the executed query does not: those edits are not
 * a scope yet, and the export does not carry them.
 */
const draftDiffers = computed(
  () =>
    JSON.stringify(sortKeys(filtersToQuery())) !==
    JSON.stringify(sortKeys(executedFilters.value)),
);

function sortKeys(
  filter: Partial<RequestTraceListParams>,
): [string, unknown][] {
  return Object.entries(filter).sort(([left], [right]) =>
    left.localeCompare(right),
  );
}

/** The draft form as the filter set the query button would submit, without validating it. */
function filtersToQuery(): Partial<RequestTraceListParams> {
  const next: Partial<RequestTraceListParams> = {};
  if (filters.trace_id) next.trace_id = filters.trace_id;
  if (filters.q) next.q = filters.q;
  if (filters.route_family)
    next.route_family =
      filters.route_family as RequestTraceListParams["route_family"];
  if (filters.client_status !== "") {
    const status = Number(filters.client_status);
    if (Number.isSafeInteger(status)) next.client_status = status;
  }
  if (filters.usage_linked !== "")
    next.usage_linked = filters.usage_linked === "true";
  for (const [raw, key] of [
    [filters.usage_log_id, "usage_log_id"],
    [filters.account_id, "account_id"],
  ] as const) {
    if (raw === "") continue;
    const id = parseLookupID(raw);
    if (id !== null) next[key] = id;
  }
  if (filters.group_mode === "id") {
    const id = parseLookupID(filters.group_id);
    if (id !== null) next.group_id = id;
  } else if (filters.group_mode === "unknown") {
    next.group_unknown = true;
  }
  if (filters.model_mode === "value") {
    if (filters.requested_model.trim() !== "")
      next.requested_model = filters.requested_model.trim();
  } else if (filters.model_mode === "unknown") {
    next.model_unknown = true;
  }
  if (filters.platform_mode === "value") {
    if (filters.platform_name.trim() !== "")
      next.platform = filters.platform_name.trim();
  } else if (filters.platform_mode === "unknown") {
    next.platform_unknown = true;
  }
  if (filters.user_mode === "id") {
    const id = parseLookupID(filters.user_id);
    if (id !== null) next.user_id = id;
  } else if (filters.user_mode === "unknown") next.user_unknown = true;
  if (filters.api_key_mode === "id") {
    const id = parseLookupID(filters.api_key_id);
    if (id !== null) next.api_key_id = id;
  } else if (filters.api_key_mode === "unknown") next.api_key_unknown = true;
  if (filters.created_from) {
    const from = new Date(filters.created_from);
    if (!Number.isNaN(from.getTime())) next.created_from = from.toISOString();
  }
  if (filters.created_to) {
    const to = new Date(filters.created_to);
    if (!Number.isNaN(to.getTime())) next.created_to = to.toISOString();
  }
  return next;
}

function isSelected(traceID: string): boolean {
  return selectedIDs.value.includes(traceID);
}

/**
 * Checking is bounded by what the server will accept: past the bound the export
 * action refuses and says so rather than sending a trimmed set.
 */
function toggleSelected(traceID: string) {
  refusal.value = null;
  const current = selectedIDs.value;
  if (current.includes(traceID)) {
    selectedIDs.value = current.filter((id) => id !== traceID);
    return;
  }
  selectedIDs.value = [...current, traceID];
}

function prepareSelectedCleanup() {
  if (
    deleting.value ||
    cleanupPreviewing.value ||
    !selectedCount.value ||
    selectionOverBound.value
  )
    return;
  cleanupMode.value = "selected";
  cleanupIDs.value = [...selectedIDs.value];
  cleanupMessage.value = "";
  cleanupOpen.value = true;
}

async function prepareFilterCleanup() {
  if (deleting.value || cleanupPreviewing.value || !canCleanupFilter.value)
    return;
  const current = ++cleanupRevision;
  const filter = { ...executedFilters.value } as RequestTraceExportFilter;
  cleanupPreviewing.value = true;
  cleanupMessage.value = "";
  cleanupFailed.value = false;
  try {
    const preview = await previewTraceDelete(filter);
    if (current !== cleanupRevision) return;
    if (!preview.matched_count) {
      cleanupMessage.value = t(
        "admin.requestTrace.list.cleanupActions.noMatches",
      );
      return;
    }
    cleanupFilter.value = filter;
    cleanupPreview.value = preview;
    cleanupMode.value = "filter";
    cleanupOpen.value = true;
  } catch {
    if (current !== cleanupRevision) return;
    cleanupFailed.value = true;
    cleanupMessage.value = t(
      "admin.requestTrace.list.cleanupActions.previewFailed",
    );
  } finally {
    if (current === cleanupRevision) cleanupPreviewing.value = false;
  }
}

function closeCleanup() {
  if (deleting.value) return;
  cleanupOpen.value = false;
  cleanupPreview.value = null;
  cleanupRevision += 1;
  cleanupPreviewing.value = false;
}

async function confirmCleanup() {
  if (deleting.value || !cleanupOpen.value) return;
  if (cleanupMode.value === "filter") {
    const preview = cleanupPreview.value;
    if (!preview || Date.parse(preview.expires_at) <= Date.now()) {
      closeCleanup();
      cleanupFailed.value = true;
      cleanupMessage.value = t(
        "admin.requestTrace.list.cleanupActions.previewExpired",
      );
      return;
    }
    if (
      JSON.stringify(sortKeys(cleanupFilter.value)) !==
      JSON.stringify(sortKeys(executedFilters.value))
    ) {
      closeCleanup();
      cleanupFailed.value = true;
      cleanupMessage.value = t(
        "admin.requestTrace.list.cleanupActions.filterChanged",
      );
      return;
    }
  }
  deleting.value = true;
  cleanupFailed.value = false;
  try {
    const result =
      cleanupMode.value === "selected"
        ? await deleteSelectedTraces(cleanupIDs.value)
        : await deleteTracesByFilter(
            cleanupFilter.value,
            cleanupPreview.value!,
          );
    cleanupFailed.value = !result.completed;
    cleanupMessage.value = t(
      result.completed
        ? "admin.requestTrace.list.cleanupActions.deleted"
        : "admin.requestTrace.list.cleanupActions.partial",
      { count: result.deleted_count },
    );
    if (result.completed) {
      if (cleanupMode.value === "selected")
        selectedIDs.value = selectedIDs.value.filter(
          (id) => !cleanupIDs.value.includes(id),
        );
      else selectedIDs.value = [];
    }
    cleanupOpen.value = false;
    cleanupPreview.value = null;
    await load(page.value);
  } catch {
    cleanupFailed.value = true;
    cleanupMessage.value = t("admin.requestTrace.list.cleanupActions.failed");
    cleanupOpen.value = false;
    cleanupPreview.value = null;
    await load(page.value);
  } finally {
    deleting.value = false;
  }
}

function clearSelection() {
  selectedIDs.value = [];
  refusal.value = null;
}

/** Reads whether this deployment has accepted the export risk statement. */
async function loadExportRisk() {
  try {
    const risk = await getTraceExportRisk();
    riskAcknowledged.value = risk.acknowledged;
  } catch {
    riskAcknowledged.value = null;
  }
}

function exportSelected() {
  if (
    creating.value ||
    exportBlocked.value ||
    selectedCount.value === 0 ||
    selectionOverBound.value
  )
    return;
  void startExport({ trace_ids: [...selectedIDs.value] });
}

/** "Everything the current query matched" ignores the checked rows entirely. */
function exportAll() {
  if (creating.value || exportBlocked.value) return;
  void startExport({ ...executedFilters.value });
}

async function startExport(filter: RequestTraceExportFilter) {
  if (riskAcknowledged.value === false) {
    refusal.value = "risk_ack_required";
    return;
  }
  creating.value = true;
  refusal.value = null;
  createController?.abort();
  const next = new AbortController();
  createController = next;
  try {
    const created = await createTraceExport(filter, { signal: next.signal });
    // The new task is part of this session's set now; a later look at the list
    // must include it without a manual refresh.
    void loadRecall();
    await openExportTask(created.id);
  } catch (error) {
    // Only a bounded refusal is rendered; anything else stays "unavailable".
    refusal.value =
      error instanceof TraceExportRefusedError ? error.refusal : "unavailable";
    // The risk verdict may be stale (another tab acknowledged it), so re-read it
    // instead of blocking the next attempt on a guess.
    void loadExportRisk();
  } finally {
    createController = null;
    creating.value = false;
  }
}

/**
 * The task handle lives in the URL, so a refresh in the same admin session finds
 * the task again. It is only a handle: the task itself is read from the server,
 * which decides whether this session may see it at all.
 */
async function openExportTask(id: string) {
  exportID.value = id;
  exportOpen.value = true;
  await router
    .replace({ query: { ...route.query, export: id } })
    .catch(() => undefined);
}

function onExportVisibility(value: boolean) {
  exportOpen.value = value;
  if (value) return;
  exportID.value = null;
  refusal.value = null;
  void router
    .replace({ query: { ...route.query, export: undefined } })
    .catch(() => undefined);
}

/** A task id can only come back from the URL as a task handle. */
function routePrefillExport(): string | null {
  const id = routeQueryString(route.query.export);
  return requestTraceExportIDPattern.test(id) ? id : null;
}

async function load(nextPage: number) {
  const current = ++revision;
  controller?.abort();
  const requestController = new AbortController();
  controller = requestController;
  loading.value = true;
  failed.value = false;
  try {
    const result = await listTraces(
      { page: nextPage, page_size: pageSize.value, ...queryFilters },
      { signal: requestController.signal },
    );
    if (current !== revision) return;
    rows.value = result.items;
    total.value = result.total;
    queryStats.value = result.stats ?? null;
    page.value = result.page;
    pageSize.value = result.page_size;
    // Only a query that ran is a scope, and only then does it clear the old
    // selection: a failed search leaves both the rows and the checks alone.
    if (
      JSON.stringify(sortKeys(executedFilters.value)) !==
      JSON.stringify(sortKeys(queryFilters))
    ) {
      closeCleanup();
    }
    executedFilters.value = { ...queryFilters };
    queryHasRun.value = true;
    if (pendingSelectionReset) {
      pendingSelectionReset = false;
      selectedIDs.value = [];
    }
    onDetailVisibility(false);
  } catch {
    if (current !== revision) return;
    rows.value = [];
    total.value = 0;
    queryStats.value = null;
    onDetailVisibility(false);
    failed.value = true;
  } finally {
    if (current === revision) loading.value = false;
  }
}

onMounted(() => {
  queryFilters = routePrefillFilters();
  const taskID = routePrefillExport();
  if (taskID) {
    exportID.value = taskID;
    exportOpen.value = true;
  }
  void loadExportRisk();
  void loadCaptureStatus();
  void loadOptions();
  // Ask once on arrival: the handle may have been left behind on a previous
  // visit, and being able to see it again is the point of the recall entry.
  void loadRecall();
  void load(1);
});
onBeforeUnmount(() => {
  revision += 1;
  optionsRevision += 1;
  cleanupRevision += 1;
  controller?.abort();
  createController?.abort();
  recallController?.abort();
  onDetailVisibility(false);
});
</script>
