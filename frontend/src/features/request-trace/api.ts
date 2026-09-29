import { apiClient } from '@/api/client'
import {
  normalizeRequestTraceOperatorStatus,
  normalizeRequestTracePage,
  normalizeRequestTraceDetail,
  normalizeRequestTraceExportTask,
  normalizeRequestTraceOpsStatus,
  requestTraceExportIDPattern,
  type RequestTraceOperatorStatus,
  type RequestTraceOperatorUpdateInput,
  type RequestTraceListParams,
  type RequestTracePage,
  type RequestTraceDetail,
  type RequestTraceExportFilter,
  type RequestTraceExportTask,
  type RequestTraceOpsStatus,
} from './types'

const path = '/admin/settings/request-trace'
const tracePath = '/admin/request-traces'
const exportPath = '/admin/request-traces/exports'
const headers = { 'Cache-Control': 'no-store', Pragma: 'no-cache' }

export async function listTraces(params: RequestTraceListParams, options?: { signal?: AbortSignal }): Promise<RequestTracePage> {
  const { data } = await apiClient.get<unknown>(tracePath, { params, headers, signal: options?.signal })
  return normalizeRequestTracePage(data)
}

export async function getTrace(traceID: string, options?: { signal?: AbortSignal }): Promise<RequestTraceDetail> {
  if (!/^[0-9a-f]{32}$/.test(traceID)) throw new Error('Invalid request Trace ID')
  const { data } = await apiClient.get<unknown>(`${tracePath}/${encodeURIComponent(traceID)}`, { headers, signal: options?.signal })
  return normalizeRequestTraceDetail(data)
}

export async function getOperatorSettings(): Promise<RequestTraceOperatorStatus> {
  const { data } = await apiClient.get<unknown>(path, { headers })
  return normalizeRequestTraceOperatorStatus(data)
}

/**
 * Reads the value-free operational status. The route is a static sibling of
 * `/request-traces/:trace_id`, so it takes no trace id, no filter and no query
 * string at all; the answer is counts and closed sets, and it never says
 * whether the capture gate is enabled.
 */
export async function getTraceOpsStatus(options?: { signal?: AbortSignal }): Promise<RequestTraceOpsStatus> {
  const { data } = await apiClient.get<unknown>(`${tracePath}/status`, { headers, signal: options?.signal })
  return normalizeRequestTraceOpsStatus(data)
}

export async function updateOperatorSettings(input: RequestTraceOperatorUpdateInput): Promise<RequestTraceOperatorStatus> {
  const { data } = await apiClient.put<unknown>(path, {
    enabled: input.enabled,
    language: input.language,
    phrase: input.phrase,
  }, { headers })
  return normalizeRequestTraceOperatorStatus(data)
}

/**
 * Creates a bounded background export. The server reads the bounded metadata
 * filter from the query string and accepts no request body at all, so a body or
 * full-text search cannot be expressed here. Only the originating admin session
 * may later read or download the task; the server enforces that.
 */
export async function createTraceExport(
  filter: RequestTraceExportFilter,
  options?: { signal?: AbortSignal },
): Promise<RequestTraceExportTask> {
  const { data } = await apiClient.post<unknown>(exportPath, null, { params: filter, headers, signal: options?.signal })
  return normalizeRequestTraceExportTask(data)
}

export async function getTraceExport(exportID: string, options?: { signal?: AbortSignal }): Promise<RequestTraceExportTask> {
  if (!requestTraceExportIDPattern.test(exportID)) throw new Error('Invalid request Trace export ID')
  const { data } = await apiClient.get<unknown>(`${exportPath}/${exportID}`, { headers, signal: options?.signal })
  return normalizeRequestTraceExportTask(data)
}

/** The server refuses a download with 410 and one of these two bounded reasons. */
const exportFileLostReason = 'REQUEST_TRACE_EXPORT_FILE_LOST'
const exportWindowClosedReason = 'REQUEST_TRACE_EXPORT_DOWNLOAD_EXPIRED'
const unsafeFileNamePattern = /[^A-Za-z0-9._-]/g

export type TraceExportDownloadResult =
  | { outcome: 'ready'; blob: Blob; fileName: string }
  | { outcome: 'file_lost' | 'expired' | 'unavailable' }

/** Uses only the server-provided basename; anything hostile degrades to the server's own naming. */
function exportFileName(exportID: string, disposition: unknown): string {
  const match = typeof disposition === 'string' ? /filename\*?=(?:UTF-8'')?"?([^";]+)"?/i.exec(disposition) : null
  const candidate = match?.[1]?.trim().replace(unsafeFileNamePattern, '_') ?? ''
  return candidate && candidate !== '.' && candidate !== '..' ? candidate : `sub2api-request-trace-export-${exportID}.jsonl`
}

/** Reads a refused download body as text, whatever the transport handed back. */
async function exportRefusalBody(data: unknown): Promise<string> {
  if (typeof data === 'string') return data
  if (data === null || typeof data !== 'object') return ''
  const blob = data as { text?: () => Promise<string> }
  if (typeof blob.text === 'function') {
    try {
      return await blob.text()
    } catch {
      return ''
    }
  }
  // Older engines (and the test DOM) expose Blob without text().
  return await new Promise<string>(resolve => {
    try {
      const reader = new FileReader()
      reader.onload = () => resolve(typeof reader.result === 'string' ? reader.result : '')
      reader.onerror = () => resolve('')
      reader.readAsText(data as Blob)
    } catch {
      resolve('')
    }
  })
}

/** Reads the bounded reason of a refused download; a blob or JSON body is parsed defensively. */
async function exportRefusalReason(data: unknown): Promise<string> {
  try {
    const parsed: unknown = JSON.parse(await exportRefusalBody(data))
    const reason = (parsed as { reason?: unknown } | null)?.reason
    return typeof reason === 'string' ? reason : ''
  } catch {
    return ''
  }
}

/**
 * Downloads the plaintext temporary file. Refusals are results, not exceptions:
 * "file_lost" is only ever reported from the server's file-lost reason, never
 * inferred from a status code, a clock or a missing field.
 */
export async function downloadTraceExport(
  exportID: string,
  options?: { signal?: AbortSignal },
): Promise<TraceExportDownloadResult> {
  if (!requestTraceExportIDPattern.test(exportID)) throw new Error('Invalid request Trace export ID')
  const response = await apiClient.get<Blob>(`${exportPath}/${exportID}/download`, {
    headers,
    responseType: 'blob',
    signal: options?.signal,
    // 410 is a bounded verdict about this file, not a transport failure: read its reason.
    validateStatus: status => status === 200 || status === 410,
  }).catch(() => null)
  if (response === null) return { outcome: 'unavailable' }
  if (response.status !== 200) {
    const reason = await exportRefusalReason(response.data)
    if (reason === exportFileLostReason) return { outcome: 'file_lost' }
    if (reason === exportWindowClosedReason) return { outcome: 'expired' }
    return { outcome: 'unavailable' }
  }
  return { outcome: 'ready', blob: response.data, fileName: exportFileName(exportID, response.headers?.['content-disposition']) }
}
