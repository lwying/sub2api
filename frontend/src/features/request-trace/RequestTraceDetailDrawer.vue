<template>
  <BaseDialog
    :show="show"
    :title="t('admin.requestTrace.detail.title')"
    width="extra-wide"
    @close="emit('update:show', false)"
  >
    <RequestTraceDetailContent :show="show" :trace-id="traceId" />
  </BaseDialog>
</template>

<script setup lang="ts">
import { useI18n } from "vue-i18n";
import BaseDialog from "@/components/common/BaseDialog.vue";
import RequestTraceDetailContent from "./RequestTraceDetailContent.vue";

defineProps<{ show: boolean; traceId: string | null }>();
const emit = defineEmits<{ (event: "update:show", value: boolean): void }>();
const { t } = useI18n();
</script>
