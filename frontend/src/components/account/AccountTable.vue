<template>
  <DataTable
    ref="table"
    v-bind="$attrs"
    :data="data"
    :columns="columns"
    :loading="loading"
    row-key="id"
  >
    <template #cell-name="scope">
      <slot name="cell-name" v-bind="scope">
        <div class="flex min-w-0 flex-col gap-1">
          <span class="font-medium text-gray-900 dark:text-white">{{
            scope.row.name
          }}</span>
        </div>
      </slot>
    </template>
    <template #cell-email_masked="scope"
      ><slot name="cell-email_masked" v-bind="scope"
        ><span data-test="cell-email">{{ scope.value || "—" }}</span></slot
      ></template
    >
    <template #cell-username_masked="scope"
      ><slot name="cell-username_masked" v-bind="scope"
        ><span data-test="cell-username">{{ scope.value || "—" }}</span></slot
      ></template
    >
    <template #cell-upstream_account_id_masked="scope"
      ><slot name="cell-upstream_account_id_masked" v-bind="scope"
        ><span data-test="cell-upstream-id">{{
          scope.value || "—"
        }}</span></slot
      ></template
    >
    <template #cell-platform_type="scope">
      <slot name="cell-platform_type" v-bind="scope"
        ><PlatformTypeBadge
          :platform="scope.row.platform"
          :type="scope.row.type"
      /></slot>
    </template>
    <template #cell-status="scope">
      <slot name="cell-status" v-bind="scope">
        <AccountStatusIndicator
          v-if="scope.row.schedulable !== undefined"
          :account="scope.row"
          :read-only="readOnly"
        />
        <span v-else class="text-sm text-gray-600 dark:text-gray-300">{{
          statusLabel(scope.row.status)
        }}</span>
      </slot>
    </template>
    <template #cell-groups="scope">
      <slot name="cell-groups" v-bind="scope"
        ><AccountGroupsCell :groups="scope.row.groups" :max-display="4"
      /></slot>
    </template>
    <template #cell-capacity="scope">
      <slot name="cell-capacity" v-bind="scope"
        ><AccountCapacityCell
          v-if="scope.row.concurrency !== undefined"
          :account="scope.row"
          :read-only="readOnly"
      /></slot>
    </template>
    <template #cell-today_stats="scope">
      <slot name="cell-today_stats" v-bind="scope"
        ><AccountTodayStatsCell
          :stats="runtimeById[String(scope.row.id)]?.today_stats ?? null"
          :loading="runtimeLoading"
          :error="runtimeError ? t('common.error') : undefined"
      /></slot>
    </template>
    <template #cell-usage="scope">
      <slot name="cell-usage" v-bind="scope"
        ><AccountUsageCell
          :account="scope.row"
          :read-only="readOnly"
          :batched-usage="runtimeById[String(scope.row.id)]?.usage ?? null"
          :batched-usage-loading="runtimeLoading"
          :batched-usage-error="runtimeError ? t('common.error') : null"
      /></slot>
    </template>
    <template v-for="name in forwardedSlots" #[name]="scope"
      ><slot :name="name" v-bind="scope"
    /></template>
  </DataTable>
</template>

<script setup lang="ts" generic="T extends AccountTableRow">
import { computed, ref, useSlots } from "vue";
import { useI18n } from "vue-i18n";
import DataTable from "@/components/common/DataTable.vue";
import PlatformTypeBadge from "@/components/common/PlatformTypeBadge.vue";
import AccountStatusIndicator from "./AccountStatusIndicator.vue";
import AccountCapacityCell from "./AccountCapacityCell.vue";
import AccountGroupsCell from "./AccountGroupsCell.vue";
import AccountTodayStatsCell from "./AccountTodayStatsCell.vue";
import AccountUsageCell from "./AccountUsageCell.vue";
import type { Column } from "@/components/common/types";
import {
  accountStatusLabel,
  type AccountTableRow,
  type AccountRuntimeSnapshot,
} from "./accountDisplay";

defineOptions({ inheritAttrs: false });
withDefaults(
  defineProps<{
    data: T[];
    columns: Column[];
    loading?: boolean;
    runtimeById?: Record<string, AccountRuntimeSnapshot>;
    runtimeLoading?: boolean;
    runtimeError?: boolean;
    /**
     * 默认单元格的只读开关。用户/授权选择保持默认 true；
     * 管理页传 false 以复用默认展示但保留管理语义（真实并发等）。
     */
    readOnly?: boolean;
  }>(),
  {
    loading: false,
    runtimeById: () => ({}),
    runtimeLoading: false,
    runtimeError: false,
    readOnly: true,
  },
);

const slots = useSlots();
const sharedSlots = new Set([
  "cell-name",
  "cell-platform_type",
  "cell-status",
  "cell-groups",
  "cell-capacity",
  "cell-today_stats",
  "cell-usage",
  "cell-email_masked",
  "cell-username_masked",
  "cell-upstream_account_id_masked",
]);
const forwardedSlots = computed(() =>
  Object.keys(slots).filter((name) => !sharedSlots.has(name)),
);
const table = ref<InstanceType<typeof DataTable> | null>(null);
const { t } = useI18n();
const statusLabel = (status: string) => accountStatusLabel(status, t);

// 管理页的滑动选择继续访问同一个 DataTable 虚拟滚动接缝。
defineExpose({
  virtualizer: computed(() => table.value?.virtualizer ?? null),
  sortedData: computed(() => table.value?.sortedData ?? []),
  tableWrapperEl: computed(() => table.value?.tableWrapperEl ?? null),
});
</script>
