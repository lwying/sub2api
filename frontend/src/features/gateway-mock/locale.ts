export const gatewayMockEn = {
  gatewayMock: {
    title: "Downstream test mock",
    description:
      "Answers a configured test message locally: no upstream attempt is made and no usage record is written.",
    loading: "Loading mock rules…",
    unavailable: "Mock rules are not available on this deployment.",
    stateLabel: "Saved state",
    stateNote: "Changes take effect only after you save the whole rule set.",
    on: "On",
    off: "Off",
    switchLabel: "Global switch",
    switchNote:
      "When the switch is off, requests continue through the normal gateway.",
    unsaved: "Unsaved changes",
    rules: {
      title: "Rules",
      description:
        "Single-turn plain text must fully match the keyword. Fixed account-test instructions are supported; tasks, tools and history are not.",
      empty:
        "No rules yet. Add one, or load the built-in presets and enable the ones you want.",
      add: "Add rule",
      remove: "Delete",
      keyword: "Keyword",
      reply: "Reply",
      enabled: "Enabled",
      enabledOn: "Enabled",
      enabledOff: "Disabled",
    },
    actions: {
      save: "Save rules",
      saving: "Saving…",
      loadPresets: "Load presets",
      loadingPresets: "Loading presets…",
    },
    saved: "Rules saved.",
    presetsLoaded: "Wrote {written} preset rules; all of them are disabled.",
    presetsUnchanged: "The rule set was not empty, so nothing was seeded.",
    events: {
      title: "Recent mock hits",
      description:
        "Local replies, newest first. Expand a row to inspect its request metadata.",
      recordingNote: "About recorded data and retention",
      retry: "Retry",
      details: {
        show: "Details",
        hide: "Collapse",
        title: "Request metadata",
        metadataNote: "Keywords and reply text are not stored with hits.",
      },
      refresh: "Refresh",
      refreshing: "Refreshing…",
      loading: "Loading mock hits…",
      failed:
        "Unable to read mock hits. Whether any rule has fired is unknown.",
      empty: "No mock hit has been recorded.",
      absentNote:
        "— means the gateway did not record that value; it is not an empty value.",
      retentionNote:
        "Hits follow the usage-log retention policy. Records older than that window are removed while auto-cleanup is enabled. No cleanup deadline is disclosed in the event details; an internal estimate must not be read as the actual cleanup time.",
      noDeadline: "No deadline recorded",
      absent: "—",
      columns: {
        occurredAt: "Time",
        rule: "Rule",
        protocol: "Protocol",
        model: "Model",
        apiKey: "API key",
        user: "User",
        group: "Group",
        account: "Account",
        clientIp: "Client IP",
        traceId: "Trace",
        cleanup: "Cleanup after",
        contentAudit: "Content audit",
        requestSource: "Request source",
        details: "Details",
      },
      contentAudit: {
        unknown: "Not recorded",
        skipped_local_mock:
          "Local mock; the content audit did not run and no upstream attempt was made",
      },
      page: "{page} / {pages}",
      total: "{total} hits",
      previous: "Previous",
      next: "Next",
      protocol: {
        messages: "Messages",
        chat_completions: "Chat Completions",
        responses: "Responses",
        unknown: "Unknown protocol",
      },
    },
    failures: {
      settingsUnavailable:
        "Mock settings are temporarily unavailable; the rule set on the server is unknown.",
      ruleInvalid:
        "A rule was rejected. Check that every rule has a keyword and a reply.",
      keywordEmpty:
        "Every rule needs a keyword with at least one non-whitespace character.",
      keywordTooLong: "A keyword is too long. Shorten it and save again.",
      replyEmpty:
        "Every rule needs a reply with at least one non-whitespace character.",
      replyTooLong: "A reply is too long. Shorten it and save again.",
      keywordTaken:
        "An enabled rule already uses that keyword. Keywords are compared after trimming and lower-casing.",
      ruleNotFound: "A rule was not found. Reload the rules and save again.",
      tooManyRules: "Too many rules. Remove some and save again.",
      sessionRequired:
        "Changing mock rules requires an admin login session, not an admin API key.",
      generic:
        "The change was not saved, so the rule set on the server is unknown. Reload before trying again.",
    },
  },
};

export const gatewayMockZh = {
  gatewayMock: {
    title: "下游测试请求 mock",
    description:
      "命中配置的测试词时由本地直接作答：不发出上游请求，也不产生使用记录。",
    loading: "正在读取 mock 规则…",
    unavailable: "当前部署不可读取 mock 规则。",
    stateLabel: "已保存状态",
    stateNote: "编辑不会立即生效，保存后统一更新整份规则。",
    on: "已开启",
    off: "已关闭",
    switchLabel: "总开关",
    switchNote: "总开关关闭时，请求继续走正常网关流程。",
    unsaved: "有未保存的改动",
    rules: {
      title: "规则",
      description:
        "仅匹配单轮纯文本；兼容固定账号测试指令，附加任务、工具或历史不命中。",
      empty: "还没有规则。可以手动添加，或载入预置词后按需启用。",
      add: "添加规则",
      remove: "删除",
      keyword: "关键词",
      reply: "回复",
      enabled: "启用",
      enabledOn: "已启用",
      enabledOff: "未启用",
    },
    actions: {
      save: "保存规则",
      saving: "正在保存…",
      loadPresets: "载入预置词",
      loadingPresets: "正在载入预置词…",
    },
    saved: "规则已保存。",
    presetsLoaded: "已写入 {written} 条预置规则，全部默认未启用。",
    presetsUnchanged: "规则集非空，未写入预置词。",
    events: {
      title: "最近命中",
      description: "按时间查看本地回复，展开记录可查看请求元数据。",
      recordingNote: "记录范围与保留策略",
      retry: "重新读取",
      details: {
        show: "详情",
        hide: "收起",
        title: "请求元数据",
        metadataNote: "命中记录不保存当时的关键词与回复正文。",
      },
      refresh: "刷新",
      refreshing: "正在刷新…",
      loading: "正在读取 mock 命中…",
      failed: "无法读取 mock 命中；是否有规则命中过未知。",
      empty: "尚未记录到任何 mock 命中。",
      absentNote: "— 表示网关当时没有记录该值，不代表它是空值。",
      retentionNote:
        "命中记录跟随使用记录的保留策略清理；自动清理关闭时不会自动删除。记录详情不披露清理期限，内部估算不能当作实际清理时间。",
      noDeadline: "未记录清理期限",
      absent: "—",
      columns: {
        occurredAt: "时间",
        rule: "规则",
        protocol: "协议",
        model: "模型",
        apiKey: "API Key",
        user: "用户",
        group: "分组",
        account: "账号",
        clientIp: "客户端 IP",
        traceId: "Trace",
        cleanup: "清理截止",
        contentAudit: "内容审计",
        requestSource: "请求来源",
        details: "详情",
      },
      contentAudit: {
        unknown: "未记录",
        skipped_local_mock: "本地 Mock，内容审计未执行",
      },
      page: "第 {page} / {pages} 页",
      total: "共 {total} 条",
      previous: "上一页",
      next: "下一页",
      protocol: {
        messages: "Messages",
        chat_completions: "Chat Completions",
        responses: "Responses",
        unknown: "无法识别的协议",
      },
    },
    failures: {
      settingsUnavailable: "mock 设置暂时不可读；服务端当前的规则集未知。",
      ruleInvalid: "有规则被拒绝。请检查每条规则都填了关键词与回复。",
      keywordEmpty: "每条规则的关键词都要至少包含一个非空白字符。",
      keywordTooLong: "有关键词过长，请缩短后重新保存。",
      replyEmpty: "每条规则的回复都要至少包含一个非空白字符。",
      replyTooLong: "有回复过长，请缩短后重新保存。",
      keywordTaken:
        "已启用的规则里已经用了这个关键词。关键词比较时会去掉首尾空白并转为小写。",
      ruleNotFound: "有规则找不到。请重新读取规则后再保存。",
      tooManyRules: "规则数量过多，请删除一些后重新保存。",
      sessionRequired:
        "改动 mock 规则需要管理员登录会话，管理员 API Key 不行。",
      generic: "改动未保存，服务端当前的规则集未知。请重新读取后再试。",
    },
  },
};
