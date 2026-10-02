<template>
  <BaseDialog
    :show="show"
    :title="t('admin.requestTrace.export.settings.title')"
    width="full"
    @close="emit('update:show', false)"
  >
    <!--
      This dialog now carries only the independent export capability. The Trace
      capture configuration lives in its own page tab; keeping the two apart means
      an export acknowledgement can never be mistaken for a capture change.
    -->
    <div
      v-if="show"
      class="space-y-4"
      data-testid="request-trace-configuration"
    >
      <RequestTraceExportSettings />
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { useI18n } from "vue-i18n";
import BaseDialog from "@/components/common/BaseDialog.vue";
import RequestTraceExportSettings from "./RequestTraceExportSettings.vue";

defineProps<{ show: boolean }>();
const emit = defineEmits<{ (event: "update:show", value: boolean): void }>();
const { t } = useI18n();
</script>
