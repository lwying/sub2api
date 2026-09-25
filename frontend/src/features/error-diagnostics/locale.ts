/**
 * Feature-local locale payload for the admin error-request diagnostics.
 *
 * Kept next to the feature so the ticket-05 work on the shared
 * `i18n/locales/{en,zh}/common|misc` blocks stays untouched. Registration into
 * the shared locale bundles is a one-line import plus spread per language in
 * `i18n/locales/{en,zh}/admin/index.ts`.
 *
 * Wording rules:
 *   - never claim "deleted" or "destroyed" — the app can say a body is expired or
 *     cleared and unreadable, not that every replica and backup is gone;
 *   - never distinguish "no encryption key" from "encryption failed"
 *     (`skipped_encryption_unavailable` is one label by design);
 *   - describe what is shown, never the model text or a credential.
 */
export const errorDiagnosticsEn = {
  errorDiagnostics: {
    title: 'Error diagnostics',
    description:
      'Per-attempt metadata for upstream 4xx/5xx responses. The request body is not listed here and is only decrypted on request.',
    list: {
      createdAt: 'Created',
      protocol: 'Protocol',
      attempt: 'Attempt',
      upstreamStatus: 'Upstream status',
      body: 'Stored body',
      expiresAt: 'Expires',
      usage: 'Usage',
      actions: 'Actions',
      view: 'View',
      refresh: 'Refresh',
      empty: 'No diagnostics recorded',
      loadFailed: 'Could not load diagnostics',
      notEnabled: 'Error diagnostics are not enabled on this instance',
      capturePaused:
        'Capture is paused, so new failed upstream attempts are not being recorded. Any records shown below were captured before it was paused.',
      captureOffNotice:
        'Capture is off, so failed upstream attempts are not being recorded. An empty list here would not mean nothing failed.',
      usageAbsent: 'No usage record',
    },
    protocols: {
      messages: 'Messages',
      chat_completions: 'Chat Completions',
      responses: 'Responses',
      unknown: 'Unknown protocol',
    },
    bodyStates: {
      notObserved: 'No request body was observed',
      stored: 'Body stored',
      skipped: 'Body not retained',
      expired: 'Body expired',
      purged: 'Body cleared',
      unknown: 'Unknown state',
    },
    headerStates: {
      notObserved: 'No 429 header values were observed',
      stored: 'Header values stored',
      skipped: 'Header values not retained',
      expired: 'Header values expired',
      purged: 'Header values cleared',
      unknown: 'Unknown state',
    },
    headerReasons: {
      not_observed: 'No 429 header values were observed for this attempt',
      retained: '429 header values retained',
      skipped_out_of_scope: 'This attempt was not an upstream 429 on the Messages path',
      skipped_header_retention_disabled: '429 header value retention is disabled',
      skipped_encryption_unavailable: 'Encryption was unavailable',
      skipped_invalid_values: 'The observed header values did not pass validation',
      unknown: 'Unknown retention outcome',
    },
    reasons: {
      not_observed: 'No request body was observed for this attempt',
      retained: 'Request body retained',
      skipped_not_text_json: 'The request was not a text JSON body',
      skipped_too_large: 'The request exceeded the retention size limit',
      skipped_attachment: 'The request contained an attachment',
      skipped_known_credential: 'The request contained a known credential',
      skipped_incomplete_read: 'The outbound body was not fully read',
      skipped_encryption_unavailable: 'Encryption was unavailable',
      skipped_body_retention_disabled: 'Request body retention is disabled',
      unknown: 'Unknown retention outcome',
    },
    detail: {
      title: 'Error diagnostic',
      loadFailed: 'Could not load the diagnostic metadata',
      createdAt: 'Created',
      protocol: 'Protocol',
      attempt: 'Attempt #{index}',
      upstreamStatus: 'Upstream status',
      body: 'Stored request body',
      bodyExpiresAt: 'Body expires',
      metadataExpiresAt: 'Metadata expires',
      reveal: 'Reveal request body',
      revealing: 'Revealing…',
      revealFailed: 'The request body could not be revealed',
      bodyBytes: '{bytes} bytes',
      usageLink: 'Related usage record',
      usageAbsent: 'No usage record',
      notice: 'The body is decrypted only on request and is never cached.',
      headers: 'Allowlisted upstream 429 header values',
      headerExpiresAt: 'Header values expire',
      headerNotice:
        'Only allowlisted header values are shown, not a complete wire capture. Values are decrypted only on request, never cached, and exclude credentials, cookies and the request body. They are revealed separately from the body.',
      revealHeaders: 'Reveal 429 header values',
      revealingHeaders: 'Revealing…',
      headerRevealFailed: 'The 429 header values could not be revealed',
      headerEntryCount: '{count} header values stored',
      requestHeaders: 'Request header values',
      responseHeaders: 'Response header values',
    },
    operator: {
      title: 'Operator capture settings',
      description:
        'Production capture is off by default. Turning it on requires an operator to type the current written risk statement, which the server records with the operator identity and time.',
      loading: 'Loading operator settings…',
      unavailable:
        'The operator settings could not be read; this is not a report that capture is off.',
      state: {
        captureLabel: 'Effective capture',
        retentionLabel: 'Effective request body retention',
        headerValuesLabel: 'Effective 429 header value retention',
        on: 'On',
        off: 'Off',
        storedLabel: 'Stored setting',
        keyLabel: 'Encryption key',
        keyAvailable: 'Configured and restart-stable',
        keyUnavailable: 'Not configured, or not restart-stable',
        captureMismatchStaleAck:
          'Capture is stored as on, but it is not running: the recorded written acknowledgement does not cover the current statement version. No failures are being recorded until the statement is acknowledged again.',
        captureMismatchNoAck:
          'Capture is stored as on, but it is not running: no written acknowledgement is in effect. No failures are being recorded until the statement is acknowledged.',
        retentionMismatchCapture:
          'Request body retention is stored as on, but it is not in effect because capture is not running.',
        retentionMismatchKey:
          'Request body retention is stored as on, but it is not in effect because no usable encryption key is configured.',
        headerValuesMismatchCapture:
          '429 header value retention is stored as on, but it is not in effect because capture is not running.',
        headerValuesMismatchKey:
          '429 header value retention is stored as on, but it is not in effect because no usable encryption key is configured. Both retention layers encrypt with the same stable key.',
      },
      ack: {
        none: 'No written risk acknowledgement has been recorded.',
        stale:
          'The recorded acknowledgement does not cover the current statement version; enabling requires acknowledging the current statement again.',
        version: 'Statement version',
        operator: 'Acknowledged by admin user',
        acceptedAt: 'Acknowledged at',
        phrase: 'Statement accepted',
      },
      languages: {
        en: 'English',
        zh: 'Chinese',
      },
      enable: {
        title: 'Enable capture',
        titleRetention: 'Enable retention',
        notice:
          'Enabling always requires typing the statement exactly as shown. The statement is never saved in this browser, and it is asked for again on every enable.',
        language: 'Statement language',
        requiredPhrase: 'Required statement',
        copyPhrase: 'Copy statement',
        phraseLabel: 'Type the statement to confirm',
        phrasePlaceholder: 'Type the statement above exactly',
        retentionToggle: 'Also retain request bodies (encrypted)',
        retentionUnavailable:
          'Request body retention needs a configured, restart-stable encryption key. Capture can still be enabled without it.',
        headerValuesToggle: 'Also retain upstream 429 header values (encrypted)',
        headerValuesUnavailable:
          '429 header value retention needs a configured, restart-stable encryption key. It is a separate switch from request body retention, and capture can be enabled without either.',
        retentionBlocked:
          'Request body and 429 header value retention both need a configured, restart-stable encryption key. Capture can be enabled without them.',
        confirm: 'Enable',
        confirming: 'Enabling…',
      },
      disable: {
        action: 'Disable',
        disabling: 'Disabling…',
        notice:
          'Disabling is always possible, needs no statement and turns request body and 429 header value retention off as well.',
      },
      errors: {
        phraseRequired: 'The written statement is required to enable capture.',
        phraseInvalid: 'The statement does not match the required statement.',
        keyUnavailable:
          'Request body retention needs a configured, restart-stable encryption key.',
        headerKeyUnavailable:
          '429 header value retention needs a configured, restart-stable encryption key.',
        sessionRequired:
          'Enabling needs an authenticated admin session so the acknowledgement can be recorded.',
        adminApiKeyForbidden:
          'Enabling needs an admin session, not an admin API key. Capture can still be disabled.',
        unavailable:
          'The operator settings are temporarily unavailable; nothing was changed.',
        generic: 'The change was rejected by the server and nothing was applied.',
      },
    },
  },
} as const

export const errorDiagnosticsZh = {
  errorDiagnostics: {
    title: '错误诊断',
    description:
      '针对上游 4xx/5xx 的逐次尝试元数据。列表不展示请求正文，正文仅在明确请求时才解密。',
    list: {
      createdAt: '创建时间',
      protocol: '协议',
      attempt: '尝试序号',
      upstreamStatus: '上游状态',
      body: '正文留存',
      expiresAt: '到期时间',
      usage: '用量',
      actions: '操作',
      view: '查看',
      refresh: '刷新',
      empty: '暂无诊断记录',
      loadFailed: '无法加载诊断记录',
      notEnabled: '本实例未开启错误诊断',
      capturePaused:
        '采集已暂停，新的失败上游尝试不会被记录；下方显示的记录均为暂停前已采集的内容。',
      captureOffNotice:
        '采集处于关闭状态，失败的上游尝试不会被记录；此处的空列表并不代表没有失败。',
      usageAbsent: '无用量记录',
    },
    protocols: {
      messages: 'Messages',
      chat_completions: 'Chat Completions',
      responses: 'Responses',
      unknown: '未知协议',
    },
    bodyStates: {
      notObserved: '未观察到请求正文',
      stored: '正文已留存',
      skipped: '正文未留存',
      expired: '正文已过期',
      purged: '正文已清除',
      unknown: '未知状态',
    },
    headerStates: {
      notObserved: '未观察到 429 头值',
      stored: '头值已留存',
      skipped: '头值未留存',
      expired: '头值已过期',
      purged: '头值已清除',
      unknown: '未知状态',
    },
    headerReasons: {
      not_observed: '该次尝试未观察到 429 头值',
      retained: '429 头值已留存',
      skipped_out_of_scope: '该次尝试不是 Messages 路径上的上游 429',
      skipped_header_retention_disabled: '429 头值留存已关闭',
      skipped_encryption_unavailable: '加密不可用',
      skipped_invalid_values: '观察到的头值未通过校验',
      unknown: '未知留存结果',
    },
    reasons: {
      not_observed: '该次尝试未观察到请求正文',
      retained: '请求正文已留存',
      skipped_not_text_json: '请求不是文本 JSON 正文',
      skipped_too_large: '请求超过留存大小上限',
      skipped_attachment: '请求包含附件',
      skipped_known_credential: '请求包含已知凭据',
      skipped_incomplete_read: '出站正文未被完整读取',
      skipped_encryption_unavailable: '加密不可用',
      skipped_body_retention_disabled: '请求正文留存已关闭',
      unknown: '未知留存结果',
    },
    detail: {
      title: '错误诊断详情',
      loadFailed: '无法加载诊断元数据',
      createdAt: '创建时间',
      protocol: '协议',
      attempt: '第 {index} 次尝试',
      upstreamStatus: '上游状态',
      body: '已留存请求正文',
      bodyExpiresAt: '正文到期',
      metadataExpiresAt: '元数据到期',
      reveal: '查看请求正文',
      revealing: '正在解密…',
      revealFailed: '无法获取请求正文',
      bodyBytes: '{bytes} 字节',
      usageLink: '关联用量记录',
      usageAbsent: '无用量记录',
      notice: '正文仅在明确请求时解密，且不会被缓存。',
      headers: '上游 429 白名单头值',
      headerExpiresAt: '头值到期',
      headerNotice:
        '只展示白名单内允许记录的头值，不是完整请求抓包。头值仅在明确请求时解密、不会被缓存，且不包含凭据、Cookie 与请求正文；头值与正文分开揭示。',
      revealHeaders: '查看 429 头值',
      revealingHeaders: '正在解密…',
      headerRevealFailed: '无法获取 429 头值',
      headerEntryCount: '已留存 {count} 条头值',
      requestHeaders: '请求头值',
      responseHeaders: '响应头值',
    },
    operator: {
      title: '错误诊断采集开关',
      description:
        '生产采集默认关闭。开启必须由运维逐字输入当前版本的书面风险确认语句，服务端会连同操作员身份与时间一并记录。',
      loading: '正在加载运维开关…',
      unavailable: '无法读取运维开关状态；这不代表采集已关闭。',
      state: {
        captureLabel: '实际采集',
        retentionLabel: '实际正文留存',
        headerValuesLabel: '实际 429 头值留存',
        on: '开启',
        off: '关闭',
        storedLabel: '存储的设置',
        keyLabel: '加密密钥',
        keyAvailable: '已配置且重启后稳定',
        keyUnavailable: '未配置，或重启后不稳定',
        captureMismatchStaleAck:
          '设置上已存为开启，但并未实际采集：已记录的书面确认不覆盖当前版本的语句。在按当前语句重新确认前，不会有任何失败被记录。',
        captureMismatchNoAck:
          '设置上已存为开启，但并未实际采集：当前没有生效的书面确认。在完成书面确认前，不会有任何失败被记录。',
        retentionMismatchCapture:
          '设置上已存为开启，但采集并未运行，因此正文留存没有生效。',
        retentionMismatchKey:
          '设置上已存为开启，但没有可用的加密密钥，因此正文留存没有生效。',
        headerValuesMismatchCapture:
          '设置上已存为开启，但采集并未运行，因此 429 头值留存没有生效。',
        headerValuesMismatchKey:
          '设置上已存为开启，但没有可用的加密密钥，因此 429 头值留存没有生效。两层留存使用同一把稳定密钥。',
      },
      ack: {
        none: '尚未记录书面风险确认。',
        stale:
          '已记录的确认不覆盖当前版本的语句；开启前必须按当前语句重新确认。',
        version: '语句版本',
        operator: '确认的管理员用户',
        acceptedAt: '确认时间',
        phrase: '已确认的语句',
      },
      languages: {
        en: '英文',
        zh: '中文',
      },
      enable: {
        title: '开启采集',
        titleRetention: '开启留存',
        notice:
          '开启时必须逐字输入所显示的语句。输入内容不会保存在本浏览器中，且每次开启都会重新要求输入。',
        language: '语句语言',
        requiredPhrase: '必须输入的语句',
        copyPhrase: '复制语句',
        phraseLabel: '输入语句以确认',
        phrasePlaceholder: '请逐字输入上方语句',
        retentionToggle: '同时留存请求正文（加密）',
        retentionUnavailable:
          '正文留存需要已配置且重启后稳定的加密密钥；不配置密钥仍可开启采集。',
        headerValuesToggle: '同时留存上游 429 头值（加密）',
        headerValuesUnavailable:
          '429 头值留存需要已配置且重启后稳定的加密密钥；它与正文留存是两个独立开关，不配置密钥仍可开启采集。',
        retentionBlocked:
          '正文留存与 429 头值留存都需要已配置且重启后稳定的加密密钥；可以先开启采集而两者都不留存。',
        confirm: '开启',
        confirming: '正在开启…',
      },
      disable: {
        action: '关闭',
        disabling: '正在关闭…',
        notice: '关闭始终可用，无需输入语句，并会同时关闭正文留存与 429 头值留存。',
      },
      errors: {
        phraseRequired: '开启采集必须提交书面确认语句。',
        phraseInvalid: '输入的语句与必须确认的语句不一致。',
        keyUnavailable: '正文留存需要已配置且重启后稳定的加密密钥。',
        headerKeyUnavailable: '429 头值留存需要已配置且重启后稳定的加密密钥。',
        sessionRequired: '开启需要已认证的管理员会话，以便记录这次确认。',
        adminApiKeyForbidden:
          '开启需要管理员会话，不能使用管理员 API Key；仍然可以关闭采集。',
        unavailable: '运维开关暂时不可用，未做任何更改。',
        generic: '服务端拒绝了本次更改，未生效。',
      },
    },
  },
} as const

export type ErrorDiagnosticsLocale = typeof errorDiagnosticsEn
