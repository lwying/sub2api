export const gatewayMockEn = {
  gatewayMock: {
    title: "Downstream test mock",
    description:
      "Answers a configured test message locally: no upstream attempt is made and no usage record is written.",
    loading: "Loading mock rules…",
    unavailable: "Mock rules are not available on this deployment.",
    stateLabel: "Effective state",
    stateNote:
      "Saving replaces the whole rule set at once; a rule that is not enabled never answers.",
    on: "On",
    off: "Off",
    switchLabel: "Global switch",
    switchNote:
      "Off by default. Only when it is on do the enabled rules below answer downstream test requests locally.",
    unsaved: "Unsaved changes",
    rules: {
      title: "Rules",
      description:
        "A rule answers only a request whose whole text equals the keyword. The reply text is sent to the downstream client as it is.",
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
        "Which rule answered which request, newest first. Keywords and reply text are not stored, so they cannot appear here.",
      refresh: "Refresh",
      refreshing: "Refreshing…",
      loading: "Loading mock hits…",
      failed:
        "Unable to read mock hits. Whether any rule has fired is unknown.",
      empty: "No mock hit has been recorded.",
      absentNote:
        "— means the gateway did not record that value; it is not an empty value.",
      retentionNote:
        "These hits follow the current usage-log retention policy: a hit is removed once it is older than that window, and while the usage auto-cleanup is off nothing is removed automatically. The cleanup column shows a deadline only when one was recorded; it is not the effective cleanup time.",
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
    stateLabel: "当前生效状态",
    stateNote: "保存会一次性替换整份规则集；未启用的规则不会命中。",
    on: "已开启",
    off: "已关闭",
    switchLabel: "总开关",
    switchNote:
      "默认关闭。只有开启后，下方已启用的规则才会对下游测试请求本地作答。",
    unsaved: "有未保存的改动",
    rules: {
      title: "规则",
      description:
        "只有整条请求文本与关键词完全相同的请求才会命中；回复正文原样发给下游客户端。",
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
      title: "最近的 mock 命中",
      description:
        "按时间倒序列出哪条规则在何时因哪个请求命中过。关键词与回复正文未落库，因此也不会出现在这里。",
      refresh: "刷新",
      refreshing: "正在刷新…",
      loading: "正在读取 mock 命中…",
      failed: "无法读取 mock 命中；是否有规则命中过未知。",
      empty: "尚未记录到任何 mock 命中。",
      absentNote: "— 表示网关当时没有记录该值，不代表它是空值。",
      retentionNote:
        "这些命中记录跟随当前的使用记录保留策略清理：超过保留窗口即被删除；使用记录自动清理关闭时不会被自动删除。「清理截止」列只在确实记录到期限时显示日期，不代表实际清理时间。",
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
