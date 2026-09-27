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
      plain_header_retained: '429 header values retained as plaintext',
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
      plain_body_retained: 'Request body retained as plaintext',
      unknown: 'Unknown retention outcome',
    },
    formats: {
      encrypted: 'encrypted',
      plaintext: 'plaintext',
    },
    rules: {
      encrypted:
        'Held as encrypted ciphertext with its own seven-day window; it needs the configured key to be readable.',
      plaintextLinked:
        'Held as plaintext in the database and owned by its usage record: it has no window of its own and is deleted with that usage record.',
      plaintextUnlinked:
        'Held as plaintext in the database and not linked to a usage record: it stops being readable 30 days after creation, when its metadata expires.',
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
      plaintextNotice: 'The body is stored as plaintext in the database and shown only on request; it is never cached by this view.',
      headers: 'Allowlisted upstream 429 header values',
      headerExpiresAt: 'Header values expire',
      headerNotice:
        'Only allowlisted header values are shown, not a complete wire capture. Values are decrypted only on request, never cached, and exclude credentials, cookies and the request body. They are revealed separately from the body.',
      plaintextHeaderNotice:
        'Only allowlisted header values are shown, not a complete wire capture. They are stored as plaintext in the database and shown only on request, never cached by this view. Credentials, cookies and the request body are excluded.',
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
        // The encrypted layers are named as such: a bare "request body retention"
        // would read as covering the plaintext layer too, which is the reading this
        // panel has to prevent.
        retentionLabel: 'Effective encrypted request body retention',
        headerValuesLabel: 'Effective encrypted 429 header value retention',
        plainBodyLabel: 'Effective plaintext request body retention',
        plainHeaderValuesLabel: 'Effective plaintext 429 header value retention',
        // The deployment premise is neither a stored switch nor an acknowledgement,
        // so it is named after what it actually is: a check of the database.
        deploymentLabel: 'Plaintext deployment check',
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
        plainBodyMismatchCapture:
          'Plaintext request body retention is stored as on, but it is not in effect because capture is not running.',
        plainBodyMismatchStaleAck:
          'Plaintext request body retention is stored as on, but it is not running: its own recorded acknowledgement does not cover the current plaintext statement. Acknowledging that statement again turns this layer back on.',
        plainBodyMismatchNoAck:
          'Plaintext request body retention is stored as on, but it is not running: no written acknowledgement of its own statement is in effect. Acknowledging that statement turns this layer back on.',
        plainHeaderValuesMismatchCapture:
          'Plaintext 429 header value retention is stored as on, but it is not in effect because capture is not running.',
        plainHeaderValuesMismatchStaleAck:
          'Plaintext 429 header value retention is stored as on, but it is not running: its own recorded acknowledgement does not cover the current plaintext statement. Acknowledging that statement again turns this layer back on.',
        plainHeaderValuesMismatchNoAck:
          'Plaintext 429 header value retention is stored as on, but it is not running: no written acknowledgement of its own statement is in effect. Acknowledging that statement turns this layer back on.',
        // The deployment premise is not an acknowledgement problem, so it gets its
        // own copy: telling the operator to acknowledge the statement again here
        // would send them to a button that can never turn the layer on.
        plaintextDeploymentBlocked:
          'Both plaintext layers are refused on this deployment because the database cannot guarantee that plaintext rows disappear together with their usage record. Acknowledging any statement will not change that.',
        deploymentSupported:
          'The database can guarantee that plaintext rows disappear together with their usage record.',
        deploymentPartitioned:
          'usage_logs is a partitioned table. The ownership foreign key the plaintext layers rely on cannot exist on it, and dropping a partition does not run foreign key actions, so a plaintext row could outlive the usage record it belongs to.',
        deploymentMissingOwnership:
          'The usage-owned plaintext tables no longer carry their ON DELETE CASCADE ownership foreign key, so deleting a usage record would leave the plaintext behind.',
        deploymentProbeFailed:
          'The deployment check could not be answered right now (database unavailable or not readable). Plaintext capture stays off until the check succeeds; no database error is shown here.',
        deploymentProbeUnavailable:
          'This instance has no deployment check wired in, so plaintext capture cannot be verified and stays off.',
        deploymentUnknown:
          'The deployment check returned a shape this build does not recognise, so plaintext capture stays off.',
      },
      ack: {
        none: 'No written risk acknowledgement has been recorded.',
        stale:
          'The recorded acknowledgement does not cover the current statement version; enabling requires acknowledging the current statement again.',
        version: 'Statement version',
        operator: 'Acknowledged by admin user',
        acceptedAt: 'Acknowledged at',
        phrase: 'Statement accepted',
        captureTitle: 'Capture gate — statement',
        plainBodyTitle: 'Plaintext request body — statement',
        plainHeaderTitle: 'Plaintext 429 header values — statement',
      },
      languages: {
        en: 'English',
        zh: 'Chinese',
      },
      enable: {
        title: 'Enable capture',
        titleRetention: 'Enable retention',
        titleLayers: 'Retention layers',
        notice:
          'Enabling always requires typing the statement exactly as shown. The statement is never saved in this browser, and it is asked for again on every enable.',
        // Layer switches turn layers off as well as on, and the server records a
        // fresh acknowledgement with every update, so the statement is required for
        // a narrowing change too. Saying so avoids the reading that "off" should be free.
        layersNotice:
          'Every change here is applied with the capture statement above, which the server records again. Turning a layer off keeps capture, the metadata and the other layers exactly as they are.',
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
        plainBodyToggle: 'Also retain request bodies as plaintext',
        plainBodyNotice:
          'Plaintext retention stores eligible request bodies unencrypted: anything that can read the database, a replica, a backup or an export can read them. It needs no encryption key, and it has its own risk statement, which does not replace the one above.',
        plainBodyRequiredPhrase: 'Required plaintext statement',
        plainHeaderValuesToggle: 'Also retain upstream 429 header values as plaintext',
        plainHeaderValuesNotice:
          'Plaintext retention stores allowlisted 429 header values unencrypted: anything that can read the database, a replica, a backup or an export can read them. It needs no encryption key, and it does not depend on the request body switch in either direction.',
        plainHeaderValuesRequiredPhrase: 'Required plaintext statement',
        retentionBlocked:
          'Request body and 429 header value retention both need a configured, restart-stable encryption key. Capture can be enabled without them.',
        confirm: 'Enable',
        applyLayers: 'Apply layer changes',
        confirming: 'Enabling…',
      },
      disable: {
        action: 'Disable',
        disabling: 'Disabling…',
        notice:
          'Disabling is always possible, needs no statement and turns every retention layer off as well, including the plaintext ones.',
      },
      errors: {
        phraseRequired: 'The written statement is required to enable capture.',
        phraseInvalid: 'The statement does not match the required statement.',
        plainBodyPhraseInvalid:
          'Plaintext request body retention needs its own statement to be typed exactly; the capture statement does not cover it.',
        plainHeaderPhraseInvalid:
          'Plaintext 429 header value retention needs its own statement to be typed exactly; the capture statement does not cover it.',
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
        deploymentUnsupported:
          'The two plaintext layers cannot be enabled on this deployment: the database cannot guarantee that plaintext rows disappear together with their usage record. Nothing was changed, and the layers can still be turned off.',
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
      plain_header_retained: '429 头值以明文留存',
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
      plain_body_retained: '请求正文以明文留存',
      unknown: '未知留存结果',
    },
    formats: {
      encrypted: '密文',
      plaintext: '明文',
    },
    rules: {
      encrypted: '以密文保存，有自有七天窗口；可读需要已配置的密钥。',
      plaintextLinked: '以明文保存在数据库中，属于其用量记录：没有自有窗口，随该用量记录一起删除。',
      plaintextUnlinked: '以明文保存在数据库中，未关联用量记录：创建后第三十天元数据到期即不可读。',
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
      plaintextNotice: '正文以明文存放在数据库中，仅在明确请求时展示，本视图不会缓存。',
      headers: '上游 429 白名单头值',
      headerExpiresAt: '头值到期',
      headerNotice:
        '只展示白名单内允许记录的头值，不是完整请求抓包。头值仅在明确请求时解密、不会被缓存，且不包含凭据、Cookie 与请求正文；头值与正文分开揭示。',
      plaintextHeaderNotice:
        '只展示白名单内允许记录的头值，不是完整请求抓包。头值以明文存放在数据库中，仅在明确请求时展示，本视图不会缓存；凭据、Cookie 与请求正文不在此列。',
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
        // 密文层显式标出「加密」：否则「实际正文留存」会被读成也涵盖明文层，
        // 而这正是本面板必须避免的误读。
        retentionLabel: '实际加密正文留存',
        headerValuesLabel: '实际加密 429 头值留存',
        plainBodyLabel: '实际明文正文留存',
        plainHeaderValuesLabel: '实际明文 429 头值留存',
        // 部署前提既不是存量开关也不是书面确认，按它的实质命名：一次数据库检查。
        deploymentLabel: '明文部署检查',
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
        plainBodyMismatchCapture:
          '明文正文留存设置上已存为开启，但采集并未运行，因此该层没有生效。',
        plainBodyMismatchStaleAck:
          '明文正文留存设置上已存为开启，但并未运行：它自己的书面确认不覆盖当前明文语句。按该语句重新确认即可恢复这一层。',
        plainBodyMismatchNoAck:
          '明文正文留存设置上已存为开启，但并未运行：当前没有覆盖它自己语句的有效书面确认。完成该语句的确认即可恢复这一层。',
        plainHeaderValuesMismatchCapture:
          '明文 429 头值留存设置上已存为开启，但采集并未运行，因此该层没有生效。',
        plainHeaderValuesMismatchStaleAck:
          '明文 429 头值留存设置上已存为开启，但并未运行：它自己的书面确认不覆盖当前明文语句。按该语句重新确认即可恢复这一层。',
        plainHeaderValuesMismatchNoAck:
          '明文 429 头值留存设置上已存为开启，但并未运行：当前没有覆盖它自己语句的有效书面确认。完成该语句的确认即可恢复这一层。',
        // 部署前提不是确认问题，因此单独给文案：让操作员去重新确认，只会把他引到一个
        // 永远开不了这一层的按钮上。
        plaintextDeploymentBlocked:
          '本部署下两个明文层都被拒绝：数据库无法保证明文行随所属使用记录一起消失。重新确认任何语句都不会改变这一点。',
        deploymentSupported: '数据库可以保证明文行随所属使用记录一起消失。',
        deploymentPartitioned:
          'usage_logs 是分区表。明文层依赖的所有权外键在这张表上无法建立，而删除分区不会触发外键动作，因此明文行可能比所属使用记录活得更久。',
        deploymentMissingOwnership:
          '随 usage 的明文旁路表已不再持有 ON DELETE CASCADE 所有权外键，删除使用记录会把明文留在库里。',
        deploymentProbeFailed:
          '部署检查此刻无法给出结论（数据库不可用或不可读）。明文采集在检查成功前保持关闭；此处不展示数据库错误原文。',
        deploymentProbeUnavailable: '本实例没有接入部署检查，无法证明明文可随 usage 消失，因此明文采集保持关闭。',
        deploymentUnknown: '部署检查返回了本版本无法识别的形态，因此明文采集保持关闭。',
      },
      ack: {
        none: '尚未记录书面风险确认。',
        stale:
          '已记录的确认不覆盖当前版本的语句；开启前必须按当前语句重新确认。',
        version: '语句版本',
        operator: '确认的管理员用户',
        acceptedAt: '确认时间',
        phrase: '已确认的语句',
        captureTitle: '采集门控语句',
        plainBodyTitle: '明文正文语句',
        plainHeaderTitle: '明文 429 头值语句',
      },
      languages: {
        en: '英文',
        zh: '中文',
      },
      enable: {
        title: '开启采集',
        titleRetention: '开启留存',
        titleLayers: '留存层设置',
        notice:
          '开启时必须逐字输入所显示的语句。输入内容不会保存在本浏览器中，且每次开启都会重新要求输入。',
        // 留存开关既能开也能关，而服务端每次更新都会重新记录一次书面确认，
        // 因此收窄（关闭某一层）同样需要上方语句；写清楚才不会让人以为「关」是免费的。
        layersNotice:
          '此处的每次更改都会连同上方采集语句一起提交并由服务端重新记录。关闭某一层不会改变采集、元数据，也不会影响其它层。',
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
        plainBodyToggle: '同时以明文留存请求正文',
        plainBodyNotice:
          '明文留存会把合格的请求正文以未加密形式保存：能读取数据库、只读副本、备份或导出的一方都能读到。它不需要加密密钥，并有自己的风险语句，不代替上方语句。',
        plainBodyRequiredPhrase: '明文留存必须确认的语句',
        plainHeaderValuesToggle: '同时以明文留存上游 429 头值',
        plainHeaderValuesNotice:
          '明文留存会把白名单内的 429 头值以未加密形式保存：能读取数据库、只读副本、备份或导出的一方都能读到。它不需要加密密钥，也与正文开关互不影响。',
        plainHeaderValuesRequiredPhrase: '明文留存必须确认的语句',
        retentionBlocked:
          '正文留存与 429 头值留存都需要已配置且重启后稳定的加密密钥；可以先开启采集而两者都不留存。',
        confirm: '开启',
        applyLayers: '应用留存层更改',
        confirming: '正在开启…',
      },
      disable: {
        action: '关闭',
        disabling: '正在关闭…',
        notice: '关闭始终可用，无需输入语句，并会同时关闭全部留存层（含两个明文层）。',
      },
      errors: {
        phraseRequired: '开启采集必须提交书面确认语句。',
        phraseInvalid: '输入的语句与必须确认的语句不一致。',
        plainBodyPhraseInvalid: '明文正文留存需要逐字输入它自己的语句；采集语句不覆盖它。',
        plainHeaderPhraseInvalid: '明文 429 头值留存需要逐字输入它自己的语句；采集语句不覆盖它。',
        keyUnavailable: '正文留存需要已配置且重启后稳定的加密密钥。',
        headerKeyUnavailable: '429 头值留存需要已配置且重启后稳定的加密密钥。',
        sessionRequired: '开启需要已认证的管理员会话，以便记录这次确认。',
        adminApiKeyForbidden:
          '开启需要管理员会话，不能使用管理员 API Key；仍然可以关闭采集。',
        unavailable: '运维开关暂时不可用，未做任何更改。',
        deploymentUnsupported:
          '本部署下无法开启两个明文层：数据库不能保证明文行随所属使用记录一起消失。本次未做任何更改，仍然可以关闭这两层。',
        generic: '服务端拒绝了本次更改，未生效。',
      },
    },
  },
} as const

export type ErrorDiagnosticsLocale = typeof errorDiagnosticsEn
