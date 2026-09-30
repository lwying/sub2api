<template>
  <BaseDialog :show="show" :title="t('admin.requestTrace.list.settings')" width="full" @close="emit('update:show', false)">
    <div v-if="show" class="space-y-4" data-testid="request-trace-configuration">
      <RequestTraceOperatorSettings :status="status" :loading="loading" @updated="status = $event" />
      <RequestTraceExportSettings />
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import RequestTraceOperatorSettings from './RequestTraceOperatorSettings.vue'
import RequestTraceExportSettings from './RequestTraceExportSettings.vue'
import { getOperatorSettings } from './api'
import type { RequestTraceOperatorStatus } from './types'

const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{ (event: 'update:show', value: boolean): void }>()
const { t } = useI18n()
const status = ref<RequestTraceOperatorStatus | null>(null)
const loading = ref(false)
let revision = 0
watch(() => props.show, show => {
  const current = ++revision
  status.value = null
  loading.value = false
  if (!show) return
  loading.value = true
  void getOperatorSettings().then(value => {
    if (current === revision && props.show) status.value = value
  }).catch(() => {
    if (current === revision) status.value = null
  }).finally(() => {
    if (current === revision) loading.value = false
  })
}, { immediate: true })
</script>
