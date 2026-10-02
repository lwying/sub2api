export const requestTraceEn = {
  requestTrace: {
    title: "Request Traces",
    description:
      "One logical request per Trace; stages show only observed facts and explicit gaps.",
    tabs: {
      records: "Records",
      config: "Capture settings",
    },
    list: {
      refresh: "Refresh",
      search: "Search",
      view: "View",
      select: "Select",
      traceId: "Trace ID",
      createdAt: "Created",
      route: "Route",
      status: "Client status",
      state: "Capture state",
      usage: "Usage",
      usageAbsent: "No usage record",
      cleanup: "Retention",
      plannedCleanup:
        "Planned cleanup after {date}; still readable until physically deleted.",
      followsUsage: "Follows the linked usage record",
      empty: "No matching Traces",
      captureOff:
        "Capture may be disabled. An empty list does not prove that no requests were made.",
      loading: "Loading Traces…",
      failed: "Unable to read Traces. Capture state is unknown.",
      invalidFilter:
        "Enter a 32-character Trace ID, a positive group, usage or account ID, an HTTP status from 0 to 599, valid times, and a value for every filter set to one value, before searching.",
      routeFamily: "Route family",
      any: "All",
      messages: "Messages",
      chat_completions: "Chat Completions",
      responses: "Responses",
      from: "From",
      to: "To",
      usageLinked: "Usage association",
      linked: "Linked",
      unlinked: "Unlinked",
      usageLogId: "Usage record ID",
      accountId: "Account ID (upstream attempt)",
      specificValue: "One value",
      unknownValue: "Unknown",
      group: "API key group",
      groupId: "Group ID",
      requestedModel: "Requested model",
      modelName: "Model name (as the client sent it)",
      platform: "Platform",
      platformName: "Platform name (as selected upstream)",
      user: "User",
      apiKey: "API Key",
      userId: "User ID",
      apiKeyId: "API Key ID",
      keyword: "Metadata keyword",
      keywordHint:
        "Trace ID prefix, route, model, current user email or API Key name (3–128 characters; no body search).",
      settings: "Trace capture settings",
      captureDisabled: "Capture is off; historical Traces remain readable.",
      captureUnknown:
        "Current capture state could not be read; historical Traces remain searchable.",
      queryStats: "Current query",
      runtimeStats: "Operational status",
      statsTotal: "Matching Traces",
      statsStatus: "Client status",
      statsCapture: "Capture completeness",
      statsUsage: "Usage association",
      statsOther: "Other / not observed",
      filterPlaceholder: {
        group: "Select a group",
        model: "Select a model",
        platform: "Select a platform",
      },
      candidates: {
        loading: "Loading model candidates…",
        error:
          "Model candidates could not be loaded. Retry to choose from the full list.",
        retry: "Retry",
      },
      cleanupActions: {
        selectedAction: "Delete checked Traces",
        filteredAction: "Delete all Traces matching this query",
        selectedTitle: "Delete checked Traces",
        selectedMessage:
          "Delete {count} checked Trace(s)? This cannot be undone. Usage and billing data are kept.",
        previewTitle: "Delete matching Traces",
        previewHint:
          "Preview the count and conditions first; the deletion is bound to the query you actually ran.",
        previewCount: "{count} Trace(s) currently match this query.",
        previewExpired:
          "This preview token has expired. Preview again before deleting.",
        noMatches: "No Traces match this query; nothing to delete.",
        filterChanged:
          "The query changed since the preview. Preview again before deleting.",
        snapshotNote:
          "Newer Trace IDs are excluded. Within the preview boundary, the confirmed filters are checked again; linkage or labels may change, so the final count can differ.",
        confirm: "Delete matching Traces",
        cancel: "Cancel",
        deleting: "Deleting…",
        deleted: "Deleted {count} Trace(s).",
        cleaned: "Cleanup finished.",
        partial:
          "Deleted {count} Trace(s), then cleanup stopped. The remaining matches were not fully deleted.",
        failed:
          "The deletion did not complete. What was deleted is reported; check the list again.",
        needsSearch:
          "Run a search with real conditions before deleting a filtered set.",
        immutableNote:
          "Usage records, billing and user data are never deleted with a Trace.",
        previewFailed: "The preview could not be read; nothing was deleted.",
      },
      captureStateLabel: {
        not_observed: "Not observed",
        stored: "Stored",
        partial: "Partially captured",
        write_failed: "Write failed",
        unknown: "Unknown capture state",
      },
    },
    detail: {
      title: "Request Trace",
      loadFailed: "Could not read this Trace. It may have been deleted.",
      stage: "Stage",
      attempt: "Attempt",
      state: "State",
      reason: "Reason",
      observed: "Observed bytes",
      retained: "Retained bytes",
      droppedEvents: "Unretained events",
      source: "View",
      stateLabel: {
        not_observed: "Not observed",
        stored: "Stored",
        truncated: "Truncated",
        unsupported: "Unsupported",
        redaction_unverified: "Redaction unverified",
        write_failed: "Write failed",
        unknown: "Unknown state",
      },
      stageLabel: {
        client_metadata: "Client metadata",
        client_entry: "Client request body",
        wire_attempt: "Upstream attempt",
        wire_request: "Outbound request body",
        upstream_response: "Upstream response",
        client_response: "Client response",
        capture_gap: "Capture gap",
        gateway_decision: "Gateway decision",
        unknown: "Unknown stage",
      },
      viewLabel: {
        decoded: "Decoded",
        transmitted: "Transmitted",
        downstream: "Downstream",
        received: "Received",
        wire: "Wire",
        unknown: "Unknown view",
      },
      reasonLabel: {
        metadata_observed: "Metadata observed",
        retained: "Retained",
        body_not_observed: "Body not observed",
        capture_body_disabled: "Body not captured: body capture is disabled",
        auth_rejected_body_not_observed:
          "Body not observed: the request was rejected before it was read",
        attempt_not_observed: "No upstream attempt was observed",
        wire_observed: "Attempt observed without a retained body",
        transport_error: "Transport error",
        wire_protocol_outside_phase1: "Protocol outside phase 1",
        hijacked_unobservable:
          "Connection hijacked; the body cannot be observed",
        truncated: "Truncated",
        truncated_unverified: "Truncated before redaction was verified",
        credential_redaction_unverified: "Credential redaction unverified",
        incomplete_event: "Incomplete stream event",
        incomplete_read: "Incomplete read",
        decode_failed: "Body could not be decoded",
        read_failed: "Body could not be read",
        stage_budget_exceeded: "Stage budget exceeded",
        decision_budget_exceeded: "Decision budget exceeded",
        decision_recorded: "Decision recorded",
        auth_accepted: "Authentication accepted",
        auth_rejected: "Authentication rejected",
        route_selected: "Route selected",
        account_switch: "Account switched",
        model_rewritten: "Model rewritten",
        mock_served: "Local mock reply served; no upstream attempt was made",
        mock_served_without_content_audit:
          "Local mock served without a content audit; no upstream attempt was made",
        identity_sent: "Identity sent",
        identity_rewritten: "Identity rewritten",
        identity_not_sent: "Identity not sent",
        other: "Other reason",
      },
      notObserved: "Body was not observed; this is not an empty request.",
      risk: "Redaction unverified: raw bytes may contain known credentials and unknown secrets.",
      facts: "Observed facts",
      factsNote:
        "Only observed facts are listed. A missing field was not observed, not empty.",
      method: "Method",
      url: "URL",
      urlOmitted: "Not retained: unparseable or too long.",
      requestHeaders: "Request headers",
      responseHeaders: "Response headers",
      headersOmitted: "Omitted header values",
      redactedNote:
        "Values shown as [REDACTED] were replaced whole, not partially masked.",
      account: "Account",
      model: "Model",
      protocol: "Protocol",
      valueProtocol: "Value protocol",
      upstreamStatus: "Upstream status",
      startedAt: "Started",
      endedAt: "Ended",
      body: "Retained text",
      metadataOnly: "Metadata only — body capture was disabled for this Trace.",
      captureBodyDisabledNote:
        "Body capture is off, so only link metadata was stored. This is expected, not a capture gap.",
      plannedCleanup:
        "Scheduled for cleanup after {date}; readable until removed.",
      decision: {
        title: "Gateway decision",
        note: "Gateway-side decision facts only. This stage is not an upstream attempt and carries no request or response body; a missing field was not observed.",
        kind: "Decision",
        outcome: "Outcome",
        source: "Source",
        sequence: "Decision order",
        modelFrom: "Model before",
        modelTo: "Model after",
        protocolFrom: "Protocol before",
        protocolTo: "Protocol after",
        account: "Account",
        decidedAt: "Decided",
        kindLabel: {
          auth: "Authentication",
          route: "Routing",
          model_mapping: "Model mapping",
          account_switch: "Account switch",
          identity: "Identity",
          mock: "Local mock reply",
        },
        outcomeLabel: {
          accepted: "Accepted",
          rejected: "Rejected",
          selected: "Selected",
          unchanged: "Unchanged",
          rewritten: "Rewritten",
          not_sent: "Not sent",
          unsupported: "Unsupported",
        },
        sourceLabel: {
          inbound: "Inbound",
          api_key: "API key",
          group: "Group",
          account: "Account",
          identity: "Identity",
          protocol_convert: "Protocol conversion",
        },
      },
      followsUsage:
        "Deleted with its usage record; this is not a 30-day expiry.",
      usageMeteringNote:
        "Token and cost figures live on the admin usage page, not in the Trace payload.",
    },
    export: {
      /**
       * The export actions carry the query beside them, so the scope line is the
       * only place the operator can see which conditions a pending export will
       * actually use. "Everything" is a scope and is said out loud.
       */
      scope: {
        heading: "Export scope: the last successfully executed query",
        none: "No conditions: every Trace this query matched",
        selected: "{count} explicitly selected Trace(s)",
        draftPending:
          "The form holds edits that were not searched. The export still uses the last successfully executed conditions.",
      },
      action: {
        selected: "Export selected ({count})",
        all: "Export all results of this query",
        creating: "Creating…",
        selectionCount: "{count} selected",
        clearSelection: "Clear selection",
        overBound:
          "{count} selected, above the limit of {max}. Uncheck rows, or export the whole query instead.",
        riskRequired:
          "The written export risk statement has not been acknowledged, so no export task can be created here.",
        riskLink: "Open settings",
        riskRecheck: "Re-check",
      },
      /**
       * Bounded outcomes of a refused export. The server's own message is never
       * rendered; each outcome says what is known and nothing more.
       */
      refusal: {
        risk_ack_required:
          "The export risk statement is not acknowledged. Acknowledge it in settings first; nothing was started.",
        session_required:
          "Only the admin login session that creates a task may create, read or download it. Admin API keys and other sessions are refused; nothing was started.",
        disabled:
          "This instance does not export: export needs a single instance with a shared local temporary directory. Nothing was started.",
        capacity:
          "The instance is at its export capacity, or the checked set was not a usable export scope. Nothing was started.",
        invalid_filter:
          "The server refused this filter set as an export scope. Nothing was started.",
        selection_too_large:
          "More Traces are checked than one export can carry. Uncheck rows, or export the whole query instead; nothing was started.",
        unavailable:
          "The export task was not created and the server did not confirm why. Nothing was started.",
        unknown: "The export task was not created.",
      },
      /**
       * Task states. "Incomplete" is its own state: a task that hit a limit or
       * lost a source record is not a smaller success.
       */
      state: {
        pending: "Pending",
        running: "Running",
        completed: "Complete",
        incomplete: "Incomplete — not a full result",
        failed: "Failed",
        file_lost: "File lost",
        expired: "Download window closed",
        unknown: "State not recognised by this version",
      },
      reason: {
        limit_rows: "Stopped at the task row limit",
        limit_bytes: "Stopped at the task byte limit",
        limit_runtime: "Stopped at the task runtime limit",
        limit_shards: "Stopped at the task shard limit",
        source_gone: "Source records disappeared while the task ran",
        read_failed: "A source record was found but could not be read",
        unknown: "Reason not recognised by this version",
      },
      progress: {
        state: "Task state",
        created: "Created",
        completedAt: "Completed",
        notCompleted: "Not completed yet",
        downloadUntil: "Download until",
        unknownDeadline: "Not recorded by the server",
        exportedCount: "Exported Traces",
        skippedCount: "Skipped (deleted or unreadable)",
        bytes: "Bytes exported",
        skippedByReason: "Skipped by reason",
        shards: "Shards written",
        scope: "Scope",
        failedNote: "The task failed and left no downloadable file.",
        statusUnknown:
          "The task state could not be read; this does not mean the task failed. Retrying.",
        incompleteNote:
          "The task finished without covering the whole query: what is delivered is partial, not a full result. Reason:",
        notSnapshot:
          "Records deleted while the task ran are skipped and counted. The result is not a point-in-time snapshot of every Trace.",
        expiredNote:
          "The server reports the download window as closed, so download is refused. Physical cleanup may follow later.",
        fileLost:
          "The task finished, but a temporary file is already gone; the server refuses its download.",
      },
      download: {
        downloading: "Downloading…",
        manifest: "Download manifest",
        shard: "Shard {part}",
        shardsNote:
          "This task wrote {count} shard(s); the manifest lists them with their sizes.",
        failed:
          "The download was refused or failed. Only the admin session that created this task may download it.",
        fileLostFor:
          "{file} is no longer on this instance, so the server refuses its download.",
        notReady:
          "The task has not produced a downloadable file yet. This is not an expiry or a loss — try again shortly.",
      },
      task: {
        title: "Request Trace export task",
        sessionOnly:
          "Only the admin login session that created this task can read it or download its files through the API. Admin API keys, other sessions and ordinary users are refused; the files themselves are plaintext in a local temporary directory.",
        absent:
          "No export task is open. The export actions beside the query start one.",
        loading: "Reading the export task…",
        unreadable:
          "The export task could not be read. This does not mean it failed, and it does not mean it can be downloaded: the server decides that per session.",
        retry: "Read again",
        loadMore: "Load more",
        loadMoreFailed:
          "The next page could not be read. The tasks already listed are unchanged, and reading again continues from the same place.",
      },
      settings: {
        title: "Request Trace export",
        description:
          "The plaintext export copy is a separate capability with its own risk statement and its own resource caps. Neither is shared with the capture gate.",
        sessionOnly:
          "The risk acknowledgement and the caps can only be written by an admin login session. Admin API keys may read them and cannot change them.",
        risk: {
          title: "Export risk acknowledgement",
          state: "Statement",
          acknowledged: "Acknowledged",
          pending: "Not acknowledged yet",
          version: "Statement version",
          acceptedAt: "Acknowledged at",
          language: "Statement language",
          requiredPhrase:
            "Type the full statement below exactly to acknowledge it",
          typePhrase: "Export risk acknowledgement",
          submit: "Acknowledge export risk",
          submitting: "Saving…",
          saved:
            "Export risk acknowledged. Creating an export no longer needs a new acknowledgement.",
          failed:
            "The acknowledgement was not saved. A statement that does not match the current one word for word is refused.",
          unavailable:
            "The export risk status could not be read. This is not an acknowledgement and says nothing about whether one exists.",
        },
        limits: {
          title: "Export task caps",
          description:
            "Caps for NEW export tasks on this instance: every value is finite, and the server clamps values outside the allowed range instead of accepting them. A task keeps the caps it started with.",
          configured: "Set by an administrator.",
          defaults:
            "Never set explicitly: the deployment defaults are in effect.",
          max_rows: "Task: maximum rows",
          max_bytes: "Task: maximum bytes",
          max_runtime_seconds: "Task: maximum runtime (seconds)",
          max_shard_rows: "Shard: maximum rows",
          max_shard_bytes: "Shard: maximum bytes",
          range:
            "Allowed range: {min} to {max}. There is no unlimited setting.",
          shardRange:
            "Allowed range: {min} to {max} (bounded by the task cap).",
          save: "Save caps",
          saving: "Saving…",
          invalid:
            "Enter a whole number inside the allowed range for every cap. The shard caps cannot exceed the task caps.",
          saved:
            "Caps saved. Export tasks started from now on use them; running tasks keep their own snapshot.",
          unavailable:
            "The export caps could not be read, so they cannot be edited here.",
        },
      },
    },
    operator: {
      title: "Request Trace capture",
      description:
        "This new plaintext Trace gate is separate from legacy audit details and error diagnostics. No request body is captured until the new risk statement is accepted.",
      unavailable:
        "Trace settings could not be read; this does not mean capture is off.",
      stored: "Stored switch",
      effective: "Effective capture",
      on: "On",
      off: "Off",
      deployment: "Usage ownership check",
      deploymentBlocked:
        "The database cannot prove that usage-owned plaintext disappears when its usage row is deleted. Enabling is blocked; disabling remains available.",
      captureUnavailable:
        "The switch is stored as on, but the deployment cannot capture plaintext.",
      ackStale:
        "The switch is stored as on, but this version of the risk statement has not been confirmed; nothing is being captured.",
      riskVersion: "Risk statement version",
      language: "Statement language",
      languages: { en: "English", zh: "Chinese" },
      requiredPhrase: "Type the full statement below exactly to enable",
      typePhrase: "Risk acknowledgement",
      enable: "Enable Trace capture",
      disable: "Disable Trace capture",
      updateFailed:
        "Trace settings could not be saved; the server has not confirmed a state change.",
      scope: {
        title: "Capture scope",
        description:
          "The scope decides which future requests are captured. It is stored with the same switch and never rewrites Traces that are already stored.",
        groupsMode: {
          all: "All groups",
          selected: "Selected groups",
        },
        groupsPlaceholder: "Choose groups",
        modelsPlaceholder: "Choose models",
        platformsPlaceholder: "Choose platforms",
        staleNote:
          "Some saved or selected entries are not in the current catalog. They are kept and shown so a saved scope is never silently cleared.",
        optionsLoading: "Loading options…",
        optionsError:
          "Options could not be loaded. Retry to choose from the full list.",
        retryOptions: "Retry",
        selectedCount: "{count} selected",
        storedGroups: "Stored groups",
        storedModels: "Stored models",
        storedPlatforms: "Stored platforms",
        allValues: "All",
        onlyValues: "Only: {values}",
        exceptValues: "Except: {values}",
        emptyValues: "No entry listed, so nothing matches",
        groupsLabel: "Downstream API key groups",
        allGroups: "All groups",
        groupIdsLabel: "Group IDs",
        groupIdsHint: "Positive group IDs, separated by commas or spaces.",
        groupsNote:
          '"All groups" also captures a request whose group cannot be determined. A selected list captures only requests whose group is known and listed.',
        modelsLabel: "Client-requested models",
        modelAll: "All models",
        modelInclude: "Only the listed models",
        modelExclude: "All models except the listed ones",
        modelsListLabel: "Model names",
        modelsHint:
          "Separated by commas or spaces; compared without regard to case.",
        modelsNote:
          'Only "all models" captures a request whose model cannot be determined. "Only" and "except" need a known client-requested model; without one the request is not captured.',
        platformsLabel: "Upstream account platforms",
        platformAll: "All platforms",
        platformInclude: "Only the listed platforms",
        platformExclude: "All platforms except the listed ones",
        platformsListLabel: "Platform names",
        platformsHint:
          "Separated by commas or spaces; compared without regard to case.",
        platformsNote:
          'The decision uses the first platform that can be determined from an actually selected upstream account, and it never changes for that logical request. A request whose platform cannot be determined is never captured under "only" or "except", and there is no setting that changes that: only "all platforms" captures it.',
        excludeRisk:
          "Excluding a platform does not guarantee it is absent from a captured Trace: once the first determinable platform is captured, a later retry on an excluded platform stays in the same Trace.",
        save: "Save capture scope",
        saving: "Saving…",
        invalid:
          'Enter at least one positive group ID, and at least one model or platform name for any scope other than "all".',
        saveFailed:
          "The capture scope was not saved; the server has not confirmed a change.",
        phraseRequired:
          "Capture is on: saving the scope writes a new risk acknowledgement, so the statement has to be typed again.",
        phraseLabel: "Risk acknowledgement for this scope change",
        updated: "Capture scope saved.",
      },
      summaryTitle: "Applied and draft",
      summaryDescription:
        "Applied values are what the server stores now; draft values are what saving would write. They differ until you save.",
      summaryField: "Setting",
      summaryApplied: "Applied",
      summaryDraft: "Draft",
      status: {
        title: "Capture status",
        description:
          "The stored switch and the server's effective verdict are separate: a stored-on gate can still be blocked by deployment, acknowledgement or an expired window.",
        stored: "Stored switch",
        effective: "Effective capture",
        expiresAt: "Capture until",
        remaining: "Time remaining",
        forever: "No time limit",
        expired: "Expired — capture stopped",
        expiredNote:
          "The capture window ended, so nothing is being captured. Restart the window or enable capture again to resume; history is not deleted.",
        expiredPendingNote:
          "This device's clock shows the capture window has ended, but the server has not confirmed it yet. The status is being refreshed; nothing is being captured in the meantime.",
        active: "Capturing",
        inactive: "Not capturing",
      },
      presets: {
        title: "Quick presets",
        description:
          "Presets only fill the body and HTTP 200 switches below. They never change scope, sampling, size limit, stop time or the master switch, and nothing is saved until you save.",
        custom: "Custom",
        lite: "Lightweight triage",
        liteHint: "Body off, HTTP 200 off: link and non-200 failures only.",
        chain: "Link observation",
        chainHint: "Body off, HTTP 200 on: full request links, metadata only.",
        detailed: "Detailed diagnosis",
        detailedHint: "Body on, HTTP 200 on: full plaintext capture.",
      },
      content: {
        title: "What to capture",
        description:
          "These two switches decide whether body text and successful (HTTP 200) Traces are captured at all.",
        body: "Capture request and response bodies",
        bodyHint:
          "One switch for client request, upstream request, upstream response and client response bodies. Off means no body is captured or stored.",
        http200: "Capture HTTP 200 Traces",
        http200Hint:
          "Only the client's final status 200 is skipped when off. Other 2xx statuses (201, 204) are still captured.",
        metadataOnly: "Metadata only",
        metadataOnlyNote:
          "Body capture is off: link metadata, attempts, decisions and timing are kept, but no body bytes.",
      },
      advanced: {
        title: "Advanced capture",
        description:
          "Rarely changed bounds. Disabled fields keep their current value and are still saved.",
        sampleHttp200: "HTTP 200 sample rate (%)",
        sampleOther: "Other statuses sample rate (%)",
        sampleHint:
          "Stable per-Trace sampling, 0–100%. It lowers stored volume only; 0% stores none, 100% stores all.",
        bodyLimit: "Body limit per stage",
        bodyLimitHint:
          "Maximum retained bytes for each body view. Larger bodies are truncated with an explicit mark.",
        duration: "Stop capture after",
        durationHint:
          "The server times this window. A normal save never restarts it.",
        durationForever: "No time limit",
        duration15m: "15 minutes",
        duration1h: "1 hour",
        duration24h: "24 hours",
        renew: "Restart capture window",
        renewHint:
          "Starts a new window from the server's current time using the selected duration.",
        renewNeedsEnabled:
          "Capture is off. Enable capture first to set a stop time.",
        renewNeedsDuration:
          "Choose a stop time above; with “No time limit” there is no window to restart.",
        renewRequiredAfterExpiry:
          "The window has expired. Restart it explicitly to capture again.",
        disabledKeepsValue:
          "Off: the value is kept and saved, but has no effect while this switch is off.",
      },
      actions: {
        dirty: "Unsaved changes",
        synced: "All changes saved",
        reset: "Reset",
        save: "Save capture settings",
        saving: "Saving…",
        saved: "Capture settings saved.",
        saveFailed:
          "Capture settings were not saved; the server has not confirmed a change.",
      },
      confirm: {
        enableTitle: "Enable Trace capture",
        enableMessage:
          "Enabling capture stores plaintext request and response fragments. Type the risk statement below to confirm.",
        disableTitle: "Disable Trace capture",
        disableMessage:
          "Disabling stops new captures immediately. Already stored Traces remain readable. This does not need the risk statement.",
        phraseLabel: "Risk acknowledgement",
        confirm: "Confirm",
        cancel: "Cancel",
        renewTitle: "Restart capture window",
        renewMessage:
          "Capture is already on. Restarting the window begins a new timed period from now.",
      },
      support: {
        supported: "The database can enforce usage-owned Trace deletion.",
        unsupported_partitioned_usage_logs:
          "Partitioned usage rows do not provide the required deletion guarantee.",
        unsupported_missing_ownership_foreign_key:
          "The Trace ownership foreign key is missing.",
        unsupported_unknown_deployment: "The database layout is not verified.",
        probe_failed: "The deployment check failed.",
        probe_unavailable: "The deployment check is unavailable.",
      },
    },
    ops: {
      title: "Operational status",
      description:
        "Value-free counters and closed-set states for this deployment. No request body, header value, credential, raw query or database message is reported here.",
      unknownGateNote:
        "This endpoint never reports whether capture is enabled. Zero counters do not prove capture is off, and healthy counters do not prove it is on.",
      refresh: "Refresh status",
      refreshing: "Refreshing…",
      loading: "Reading the operational status…",
      failed:
        "The operational status could not be read. This says nothing about whether capture is running.",
      yes: "Yes",
      no: "No",
      probeLabel: "Store probe",
      storageProbe: {
        not_configured: "Not probed on this deployment",
        reachable: "The store answered a bounded read",
        unavailable: "The store did not answer",
        unknown: "Probe state not recognised",
      },
      capture: {
        title: "Capture queue",
        storageLabel: "Storage state",
        storage: {
          not_wired: "No trace store is attached, so nothing can be stored",
          no_traffic:
            "Nothing accepted since this process started; this is not a health claim",
          ok: "Work was accepted and none of it failed",
          write_failed: "At least one write or drop was recorded",
          unknown: "Storage state not recognised",
        },
        repository: "Trace store attached",
        stopped: "Queue stopped",
        queue: "Queue depth / capacity",
        countersNote:
          "Counters are cumulative since this process started; they are not a rate and not a time window.",
        accepted: "Accepted",
        stored: "Stored",
        writeFailed: "Write failures",
        dropped: "Dropped",
        rejected: "Rejected",
      },
      export: {
        title: "Export worker",
        started: "Worker started",
        ticks: "Worker ticks",
        tasksRun: "Tasks run",
        tasksCompleted: "Tasks completed",
        tasksFailed: "Tasks failed",
        failures: "Failures",
        disabledTicks: "Ticks with export disabled",
        cleanups: "Cleanup passes",
        cleanedFiles: "Temporary files removed",
      },
      cleanup: {
        title: "Unlinked cleanup",
        runs: "Cleanup runs",
        deleted: "Deleted traces",
        failures: "Failures",
        lastDeleted: "Deleted in the last run",
        backlogLabel: "Unlinked backlog past its planned cleanup",
        backlogState: {
          unavailable: "Not measured",
          measured: "Exact count",
          at_least: "At least this many",
          unknown: "Backlog state not recognised",
        },
        backlogLimit: "Probe cap",
        atLeastNote:
          "The count stopped at the probe cap, so the real backlog is at least this large.",
        unavailableNote:
          "The count could not be taken; that is not a claim that the backlog is zero.",
      },
    },
  },
};

export const requestTraceZh = {
  requestTrace: {
    title: "请求 Trace",
    description: "每条逻辑请求一条 Trace；各阶段只显示已观察事实和明确缺口。",
    tabs: {
      records: "记录",
      config: "采集配置",
    },
    list: {
      refresh: "刷新",
      search: "查询",
      view: "查看",
      select: "选择",
      traceId: "Trace ID",
      createdAt: "创建时间",
      route: "入口",
      status: "客户端状态",
      state: "采集状态",
      usage: "使用记录",
      usageAbsent: "未关联使用记录",
      cleanup: "留存规则",
      plannedCleanup: "计划于 {date} 起清理；实际删除前仍可读取。",
      followsUsage: "随关联的使用记录删除",
      empty: "没有匹配的 Trace",
      captureOff: "采集可能尚未开启；列表为空不代表没有请求。",
      loading: "正在加载 Trace…",
      failed: "无法读取 Trace；采集状态未知。",
      invalidFilter:
        "查询前请输入 32 位 Trace ID、正整数的分组／使用记录／账号 ID、0 至 599 的 HTTP 状态码及有效时间；设为“指定值”的筛选项必须填写内容。",
      routeFamily: "路由族",
      any: "全部",
      messages: "Messages",
      chat_completions: "Chat Completions",
      responses: "Responses",
      from: "起始时间",
      to: "截止时间",
      usageLinked: "使用记录关联",
      linked: "已关联",
      unlinked: "未关联",
      usageLogId: "使用记录 ID",
      accountId: "账号 ID（上游尝试）",
      specificValue: "指定值",
      unknownValue: "未知",
      group: "API Key 分组",
      groupId: "分组 ID",
      requestedModel: "客户端请求模型",
      modelName: "模型名（客户端原始输入）",
      platform: "平台",
      platformName: "平台名（上游实际选中）",
      user: "用户",
      apiKey: "API Key",
      userId: "用户 ID",
      apiKeyId: "API Key ID",
      keyword: "元数据关键词",
      keywordHint:
        "检索 Trace ID 前缀、路由、模型、当前用户邮箱或 Key 名称（3–128 字；不搜索正文）。",
      settings: "Trace 采集设置",
      captureDisabled: "当前采集已关闭；历史 Trace 仍可查询。",
      captureUnknown: "无法读取当前采集状态；历史 Trace 仍可查询。",
      queryStats: "当前查询",
      runtimeStats: "运行状态",
      statsTotal: "匹配 Trace",
      statsStatus: "客户端状态",
      statsCapture: "采集完整性",
      statsUsage: "使用记录关联",
      statsOther: "其他／未观察",
      filterPlaceholder: {
        group: "选择分组",
        model: "选择模型",
        platform: "选择平台",
      },
      candidates: {
        loading: "正在加载模型候选…",
        error: "无法加载模型候选；请重试以从完整列表中选择。",
        retry: "重试",
      },
      cleanupActions: {
        selectedAction: "清理勾选记录",
        filteredAction: "清理全部匹配当前查询的记录",
        selectedTitle: "清理勾选记录",
        selectedMessage:
          "确认删除已勾选的 {count} 条 Trace？此操作不可恢复；使用量与计费数据会保留。",
        previewTitle: "清理匹配记录",
        previewHint: "请先预览数量与条件；删除绑定到你实际执行的查询。",
        previewCount: "当前查询匹配 {count} 条 Trace。",
        previewExpired: "该预览令牌已过期；请重新预览后再删除。",
        noMatches: "没有 Trace 匹配该查询，无需清理。",
        filterChanged: "查询条件已变化；请重新预览后再删除。",
        snapshotNote:
          "仅清理预览时的 ID 边界内、执行时仍匹配这些条件的 Trace。关联状态或显示名称可能变化，因此最终数量可能与预览不同。新 ID 不会被清理。",
        confirm: "删除匹配的 Trace",
        cancel: "取消",
        deleting: "正在删除…",
        deleted: "已删除 {count} 条 Trace。",
        cleaned: "清理已完成。",
        partial:
          "已删除 {count} 条 Trace，随后清理中断；剩余匹配记录尚未全部删除。",
        failed: "删除未全部完成；已如实报告删除数量，请重新查看列表。",
        needsSearch: "删除筛选集合前，请先用真实条件执行一次查询。",
        immutableNote: "使用记录、计费与用户数据不会随 Trace 删除。",
        previewFailed: "无法读取预览；没有删除任何内容。",
      },
      captureStateLabel: {
        not_observed: "未观察到",
        stored: "已存储",
        partial: "部分采集",
        write_failed: "写入失败",
        unknown: "未知采集状态",
      },
    },
    detail: {
      title: "请求 Trace",
      loadFailed: "无法读取该 Trace；记录可能已被删除。",
      stage: "阶段",
      attempt: "上游尝试",
      state: "状态",
      reason: "原因",
      observed: "已观察字节",
      retained: "已保留字节",
      droppedEvents: "未保留事件",
      source: "视图",
      stateLabel: {
        not_observed: "未观察到",
        stored: "已存储",
        truncated: "已截断",
        unsupported: "不支持",
        redaction_unverified: "脱敏未验证",
        write_failed: "写入失败",
        unknown: "未知状态",
      },
      stageLabel: {
        client_metadata: "客户端元数据",
        client_entry: "客户端请求正文",
        wire_attempt: "上游尝试",
        wire_request: "出站请求正文",
        upstream_response: "上游响应",
        client_response: "客户端响应",
        capture_gap: "采集中断",
        gateway_decision: "网关决定",
        unknown: "未知阶段",
      },
      viewLabel: {
        decoded: "已解码",
        transmitted: "已传输",
        downstream: "下游",
        received: "已接收",
        wire: "线上",
        unknown: "未知视图",
      },
      reasonLabel: {
        metadata_observed: "已观察元数据",
        retained: "已保留",
        body_not_observed: "正文未被观察",
        capture_body_disabled: "未采集正文：正文采集已关闭",
        auth_rejected_body_not_observed: "正文未被观察：请求在读取前即被拒绝",
        attempt_not_observed: "未观察到上游尝试",
        wire_observed: "已观察尝试，但未保留正文",
        transport_error: "传输错误",
        wire_protocol_outside_phase1: "协议不在第一阶段范围内",
        hijacked_unobservable: "连接被劫持，正文无法观察",
        truncated: "已截断",
        truncated_unverified: "脱敏验证前即被截断",
        credential_redaction_unverified: "凭据脱敏未验证",
        incomplete_event: "流式事件不完整",
        incomplete_read: "读取不完整",
        decode_failed: "正文无法解码",
        read_failed: "正文无法读取",
        stage_budget_exceeded: "超出阶段预算",
        decision_budget_exceeded: "超出决定预算",
        decision_recorded: "已记录决定",
        auth_accepted: "鉴权通过",
        auth_rejected: "鉴权被拒",
        route_selected: "已选择路由",
        account_switch: "已切换账号",
        model_rewritten: "模型已改写",
        mock_served: "已由本地 mock 应答，未发出上游尝试",
        mock_served_without_content_audit:
          "本地 Mock，未执行内容审计、未发上游",
        identity_sent: "身份已发送",
        identity_rewritten: "身份已改写",
        identity_not_sent: "身份未发送",
        other: "其他原因",
      },
      notObserved: "正文未被观察，不代表请求正文为空。",
      risk: "脱敏未验证：原始字节可能包含已知凭据和未知秘密。",
      facts: "已观察事实",
      factsNote: "仅列出已观察事实；字段缺失表示未观察到，而非为空。",
      method: "方法",
      url: "URL",
      urlOmitted: "未保留：无法解析或过长。",
      requestHeaders: "请求头",
      responseHeaders: "响应头",
      headersOmitted: "未保留的头值数量",
      redactedNote: "显示为 [REDACTED] 的值已被整体替换，而非部分遮蔽。",
      account: "账号",
      model: "模型",
      protocol: "协议",
      valueProtocol: "取值协议",
      upstreamStatus: "上游状态",
      startedAt: "开始时间",
      endedAt: "结束时间",
      body: "已保留文本",
      metadataOnly: "仅元信息——该 Trace 未采集正文。",
      captureBodyDisabledNote:
        "正文采集已关闭，因此只存储链路元信息；这是预期结果，不是采集缺口。",
      plannedCleanup: "计划于 {date} 起清理；实际删除前仍可读取。",
      decision: {
        title: "网关决定",
        note: "仅列出网关侧决定事实；该阶段不是一次上游尝试，也不携带请求或响应正文，字段缺失表示未观察到。",
        kind: "决定类型",
        outcome: "结果",
        source: "来源",
        sequence: "决定顺序",
        modelFrom: "改写前模型",
        modelTo: "改写后模型",
        protocolFrom: "改写前协议",
        protocolTo: "改写后协议",
        account: "账号",
        decidedAt: "决定时间",
        kindLabel: {
          auth: "鉴权",
          route: "路由",
          model_mapping: "模型映射",
          account_switch: "账号切换",
          identity: "身份",
          mock: "本地 mock 应答",
        },
        outcomeLabel: {
          accepted: "已接受",
          rejected: "已拒绝",
          selected: "已选择",
          unchanged: "未改变",
          rewritten: "已改写",
          not_sent: "未发送",
          unsupported: "不支持",
        },
        sourceLabel: {
          inbound: "入站",
          api_key: "API Key",
          group: "分组",
          account: "账号",
          identity: "身份",
          protocol_convert: "协议转换",
        },
      },
      followsUsage: "随使用记录删除，而非固定 30 天到期。",
      usageMeteringNote:
        "Token 与费用数据在使用记录页查看，不来自 Trace 正文。",
    },
    export: {
      scope: {
        heading: "导出范围：最近一次成功执行的查询条件",
        none: "无条件：该查询匹配的全部 Trace",
        selected: "明确勾选的 {count} 条 Trace",
        draftPending:
          "表单中有尚未查询的修改；导出仍使用最近一次成功执行的条件。",
      },
      action: {
        selected: "导出所选（{count}）",
        all: "导出当前查询全部",
        creating: "正在创建…",
        selectionCount: "已选 {count} 条",
        clearSelection: "清除勾选",
        overBound:
          "已勾选 {count} 条，超过上限 {max} 条。请取消部分勾选，或改用“导出当前查询全部”。",
        riskRequired: "尚未确认导出风险声明，因此无法在此创建导出任务。",
        riskLink: "前往设置",
        riskRecheck: "重新检查",
      },
      refusal: {
        risk_ack_required:
          "尚未确认导出风险声明；请先在设置中完成逐字确认。没有任务被启动。",
        session_required:
          "只有创建任务的管理员登录会话可创建、查看或下载；管理员 API Key 与其他会话会被拒绝。没有任务被启动。",
        disabled:
          "本实例不提供导出：导出要求单实例且使用本机共享临时目录。没有任务被启动。",
        capacity:
          "实例导出容量已达上限，或所选集合不是可用的导出范围。没有任务被启动。",
        invalid_filter: "服务端拒绝把该筛选条件作为导出范围。没有任务被启动。",
        selection_too_large:
          "勾选数量超过单次导出可携带的上限。请取消部分勾选或改用“导出当前查询全部”；没有任务被启动。",
        unavailable: "导出任务未创建，服务端未确认原因。没有任务被启动。",
        unknown: "导出任务未创建。",
      },
      state: {
        pending: "排队中",
        running: "进行中",
        completed: "完整",
        incomplete: "不完整——不是全量结果",
        failed: "已失败",
        file_lost: "文件已丢失",
        expired: "下载窗口已关闭",
        unknown: "本版本无法识别的状态",
      },
      reason: {
        limit_rows: "达到任务条数上限而停止",
        limit_bytes: "达到任务字节上限而停止",
        limit_runtime: "达到任务运行时长上限而停止",
        limit_shards: "达到任务分片数上限而停止",
        source_gone: "任务执行期间源记录消失",
        read_failed: "找到了源记录，但读取失败",
        unknown: "本版本无法识别的原因",
      },
      progress: {
        state: "任务状态",
        created: "创建时间",
        completedAt: "完成时间",
        notCompleted: "尚未完成",
        downloadUntil: "可下载至",
        unknownDeadline: "服务端未记录",
        exportedCount: "已导出 Trace",
        skippedCount: "已跳过（已删除或无法读取）",
        bytes: "已导出字节",
        skippedByReason: "按原因分类的跳过数",
        shards: "已写分片",
        scope: "范围",
        failedNote: "任务失败，没有留下可下载的文件。",
        statusUnknown: "无法读取任务状态；这不代表任务已失败，正在重试。",
        incompleteNote:
          "任务结束但未覆盖整个查询：交付内容是部分结果，不是全量结果。原因：",
        notSnapshot:
          "任务执行期间被删除的记录会跳过并计数；结果不是全量固定时点快照。",
        expiredNote:
          "服务端报告下载窗口已关闭，因此拒绝下载；物理清理可能稍后执行。",
        fileLost:
          "任务已完成，但某个临时文件已不存在，服务端将拒绝下载该文件。",
      },
      download: {
        downloading: "正在下载…",
        manifest: "下载清单",
        shard: "分片 {part}",
        shardsNote:
          "本次任务写入了 {count} 个分片；清单中列出了它们及各自字节数。",
        failed: "下载被拒绝或失败；只有创建该任务的管理员会话可以下载。",
        fileLostFor: "{file} 在本机已不存在，服务端拒绝下载该文件。",
        notReady:
          "任务还没有产出可下载的文件。这不是过期也不是丢失，稍后重试即可。",
      },
      task: {
        title: "请求 Trace 导出任务",
        sessionOnly:
          "只有创建此任务的管理员登录会话可通过 API 读取它或下载其文件。管理员 API Key、其他会话及普通用户会被拒绝；文件本身是本机临时目录中的明文副本。",
        absent: "当前没有打开的导出任务。查询旁的导出操作会启动一个。",
        loading: "正在读取导出任务…",
        unreadable:
          "无法读取该导出任务。这不代表任务已失败，也不代表可以下载——能否下载由服务端按会话判定。",
        retry: "重新读取",
        loadMore: "加载更多",
        loadMoreFailed:
          "无法读取下一页；已列出的导出任务保持不变，可以继续从同一位置重试。",
      },
      settings: {
        title: "请求 Trace 导出",
        description:
          "明文导出副本是独立能力：它有独立的逐字风险声明与独立的资源上限，二者都不与采集总开关共用。",
        sessionOnly:
          "风险确认与上限只有管理员登录会话可以写入；管理员 API Key 可以读取，但不能修改。",
        risk: {
          title: "导出风险确认",
          state: "声明状态",
          acknowledged: "已确认",
          pending: "尚未确认",
          version: "声明版本",
          acceptedAt: "确认时间",
          language: "确认语句语言",
          requiredPhrase: "确认前请逐字输入以下完整语句",
          typePhrase: "导出风险确认",
          submit: "确认导出风险",
          submitting: "正在保存…",
          saved: "已确认导出风险；后续创建导出任务不再需要新的确认。",
          failed: "确认未保存。与当前声明不是逐字一致的语句会被拒绝。",
          unavailable:
            "无法读取导出风险状态；这不构成一次确认，也不代表已存在确认。",
        },
        limits: {
          title: "导出任务上限",
          description:
            "这些上限作用于本实例新建的导出任务：所有值都必须有限，超出允许区间的取值会被服务端收敛到边界，而不是被接受。任务保留创建时的上限快照。",
          configured: "已由管理员显式设置。",
          defaults: "尚未显式设置：当前为部署默认值。",
          max_rows: "整任务：最大条数",
          max_bytes: "整任务：最大字节数",
          max_runtime_seconds: "整任务：最长运行秒数",
          max_shard_rows: "单分片：最大条数",
          max_shard_bytes: "单分片：最大字节数",
          range: "允许区间：{min} 至 {max}；没有“无限制”选项。",
          shardRange: "允许区间：{min} 至 {max}（受整任务上限约束）。",
          save: "保存上限",
          saving: "正在保存…",
          invalid:
            "每一项都请填写允许区间内的整数；分片上限不得超过整任务上限。",
          saved:
            "上限已保存。此后的新导出任务会使用它；已开始的任务保留自己的快照。",
          unavailable: "无法读取导出上限，因此无法在此编辑。",
        },
      },
    },
    operator: {
      title: "请求 Trace 明文采集",
      description:
        "新的 Trace 总开关独立于旧值明细和错误诊断。完成新版逐字风险确认前，不会据此采集请求正文。",
      unavailable: "无法读取 Trace 设置；这不表示采集已关闭。",
      stored: "已保存的开关",
      effective: "实际采集结论",
      on: "开启",
      off: "关闭",
      deployment: "使用记录所有权检查",
      deploymentBlocked:
        "数据库无法证明明文随使用记录删除；禁止开启，但仍可关闭。",
      captureUnavailable: "已保存的开关为开启，但当前部署无法采集明文。",
      ackStale: "已保存的开关为开启，但尚未确认本版风险语句，当前不采集。",
      riskVersion: "风险语句版本",
      language: "确认语句语言",
      languages: { en: "英文", zh: "中文" },
      requiredPhrase: "开启前请逐字输入以下完整语句",
      typePhrase: "风险确认",
      enable: "开启 Trace 采集",
      disable: "关闭 Trace 采集",
      updateFailed: "Trace 设置保存失败；服务器未确认状态变更。",
      scope: {
        title: "采集范围",
        description:
          "范围决定后续哪些请求进入采集；它与同一个开关一起保存，不追溯改变已存 Trace。",
        groupsMode: {
          all: "全部分组",
          selected: "指定分组",
        },
        groupsPlaceholder: "选择分组",
        modelsPlaceholder: "选择模型",
        platformsPlaceholder: "选择平台",
        staleNote:
          "部分已保存或已选条目不在当前目录中；它们会被保留并显示，已保存范围不会被静默清空。",
        optionsLoading: "正在加载选项…",
        optionsError: "无法加载选项；请重试以从完整列表中选择。",
        retryOptions: "重试",
        selectedCount: "已选 {count} 项",
        storedGroups: "已保存的分组",
        storedModels: "已保存的模型",
        storedPlatforms: "已保存的平台",
        allValues: "全部",
        onlyValues: "仅：{values}",
        exceptValues: "排除：{values}",
        emptyValues: "列表为空，因此没有任何请求匹配",
        groupsLabel: "下游 API Key 分组",
        allGroups: "全部分组",
        groupIdsLabel: "分组 ID",
        groupIdsHint: "正整数分组 ID，用逗号或空格分隔。",
        groupsNote:
          "“全部分组”也会采集无法确定分组的请求；选择指定分组时，只有分组已知且在列表中的请求才会被采集。",
        modelsLabel: "客户端请求模型",
        modelAll: "所有模型",
        modelInclude: "仅指定的模型",
        modelExclude: "排除指定的模型",
        modelsListLabel: "模型名",
        modelsHint: "用逗号或空格分隔；比较时不区分大小写。",
        modelsNote:
          "只有“所有模型”才会采集无法确定模型的请求；“仅指定”和“排除指定”都要求模型已知，无法确定时不采集。",
        platformsLabel: "上游账号平台",
        platformAll: "所有平台",
        platformInclude: "仅指定的平台",
        platformExclude: "排除指定的平台",
        platformsListLabel: "平台名",
        platformsHint: "用逗号或空格分隔；比较时不区分大小写。",
        platformsNote:
          "平台结论由首个可确定的实际选中上游账号决定，且对该次逻辑请求不再改变。无法确定平台时，“仅指定”与“排除指定”都不采集，且没有可以改变这一点的开关；只有“所有平台”才会采集。",
        excludeRisk:
          "排除某个平台并不保证它不会出现在已采集的 Trace 中：首个可确定平台一旦命中，之后切换到被排除平台的重试仍属于同一条 Trace。",
        save: "保存采集范围",
        saving: "正在保存…",
        invalid:
          "请至少输入一个正整数分组 ID；范围不是“全部”时，请至少输入一个模型或平台名。",
        saveFailed: "采集范围未保存；服务器未确认变更。",
        phraseRequired:
          "采集已开启：保存范围会写入一次新的风险确认，因此需要重新逐字输入该语句。",
        phraseLabel: "本次范围变更的风险确认",
        updated: "采集范围已保存。",
      },
      summaryTitle: "生效与草稿",
      summaryDescription:
        "“生效”是服务端当前的存储值；“草稿”是保存后将写入的值，保存前二者可能不同。",
      summaryField: "项目",
      summaryApplied: "生效",
      summaryDraft: "草稿",
      status: {
        title: "采集状态",
        description:
          "已保存的开关与服务端的实际结论相互独立：已保存为开启，仍可能因部署、风险确认或窗口到期而未在采集。",
        stored: "已保存的开关",
        effective: "实际采集结论",
        expiresAt: "采集截止时间",
        remaining: "剩余时间",
        forever: "无时限",
        expired: "已到期——采集已停止",
        expiredNote:
          "采集窗口已结束，当前不再采集。可重新开始窗口或再次开启采集；历史记录不会被删除。",
        expiredPendingNote:
          "按本机时间采集窗口已结束，但服务端尚未确认；状态正在刷新，在此期间不进行采集。",
        active: "采集中",
        inactive: "未采集",
      },
      presets: {
        title: "快捷预设",
        description:
          "预设只会填充下方的正文与 HTTP 200 开关，不改变范围、采样率、体积上限、停止时间或总开关；保存前不会生效。",
        custom: "自定义",
        lite: "轻量排查",
        liteHint: "正文关闭、200 关闭：只看链路与非 200 失败。",
        chain: "链路观察",
        chainHint: "正文关闭、200 开启：保留完整链路，仅元信息。",
        detailed: "详细诊断",
        detailedHint: "正文开启、200 开启：完整明文采集。",
      },
      content: {
        title: "采集内容",
        description:
          "这两个开关决定是否采集正文文本以及最终状态为 HTTP 200 的整条 Trace。",
        body: "采集请求与响应正文",
        bodyHint:
          "一个开关控制客户端请求、上游请求、上游响应与客户端响应正文。关闭后不采集、不存储任何正文。",
        http200: "采集 HTTP 200 的 Trace",
        http200Hint:
          "关闭时只跳过客户端最终状态码为 200 的整条 Trace；其他 2xx（201、204）仍会采集。",
        metadataOnly: "仅元信息",
        metadataOnlyNote:
          "正文采集已关闭：保留链路元信息、上游尝试、决定与耗时，但不保留任何正文字节。",
      },
      advanced: {
        title: "高级采集",
        description: "这些边界很少改动。禁用字段会保留当前值并照常保存。",
        sampleHttp200: "HTTP 200 采样率（%）",
        sampleOther: "其他状态采样率（%）",
        sampleHint:
          "按 Trace 稳定取样的 0–100%。仅降低入库量；0% 全部跳过，100% 全部保留。",
        bodyLimit: "每阶段正文上限",
        bodyLimitHint:
          "每个正文视图的最大保留字节数；超出部分会被截断并明确标识。",
        duration: "停止采集于",
        durationHint: "窗口由服务端计时；普通保存不会重新计时。",
        durationForever: "无时限",
        duration15m: "15 分钟",
        duration1h: "1 小时",
        duration24h: "24 小时",
        renew: "重新开始采集窗口",
        renewHint: "按所选时长，以服务端当前时间开始一个新窗口。",
        renewNeedsEnabled: "采集已关闭；请先开启采集，再设置停止时间。",
        renewNeedsDuration:
          "请先在上方选择停止时间；“无时限”下没有可重新开始的窗口。",
        renewRequiredAfterExpiry: "窗口已到期；需显式重新开始才能继续采集。",
        disabledKeepsValue:
          "已关闭：该值会被保留并保存，但在此开关关闭期间不生效。",
      },
      actions: {
        dirty: "有未保存的修改",
        synced: "已全部保存",
        reset: "重置",
        save: "保存采集设置",
        saving: "正在保存…",
        saved: "采集设置已保存。",
        saveFailed: "采集设置未保存；服务端未确认变更。",
      },
      confirm: {
        enableTitle: "开启 Trace 采集",
        enableMessage:
          "开启后会存储明文请求与响应片段；请在下方逐字输入风险语句以确认。",
        disableTitle: "关闭 Trace 采集",
        disableMessage:
          "关闭后立即停止新的采集；已存储的 Trace 仍可读取。此操作不需要风险语句。",
        phraseLabel: "风险确认",
        confirm: "确认",
        cancel: "取消",
        renewTitle: "重新开始采集窗口",
        renewMessage:
          "采集已开启；重新开始窗口将从当前时间起计算新的限时周期。",
      },
      support: {
        supported: "数据库可保证随使用记录删除关联 Trace。",
        unsupported_partitioned_usage_logs:
          "分区使用记录不满足所需的同步删除保证。",
        unsupported_missing_ownership_foreign_key: "Trace 所有权外键缺失。",
        unsupported_unknown_deployment: "无法核实数据库部署形态。",
        probe_failed: "部署检查失败。",
        probe_unavailable: "部署检查不可用。",
      },
    },
    ops: {
      title: "运维状态",
      description:
        "本部署的无值计数与闭集状态；此处不报告请求正文、头值、凭据、原始查询或数据库消息。",
      unknownGateNote:
        "该接口不报告采集是否开启；计数为零不能证明采集已关闭，计数正常也不能证明采集已开启。",
      refresh: "刷新状态",
      refreshing: "正在刷新…",
      loading: "正在读取运维状态…",
      failed: "无法读取运维状态；这不代表采集是否在运行。",
      yes: "是",
      no: "否",
      probeLabel: "存储探测",
      storageProbe: {
        not_configured: "本部署未进行探测",
        reachable: "存储完成了一次有界读取",
        unavailable: "存储未响应",
        unknown: "无法识别的探测状态",
      },
      capture: {
        title: "采集队列",
        storageLabel: "存储状态",
        storage: {
          not_wired: "未接入 Trace 存储，无法写入",
          no_traffic: "本进程启动后未接受过任务；这不是健康结论",
          ok: "已接受任务且没有失败",
          write_failed: "至少发生一次写入失败或丢弃",
          unknown: "无法识别的存储状态",
        },
        repository: "已接入 Trace 存储",
        stopped: "队列已停止",
        queue: "队列深度 / 容量",
        countersNote:
          "计数为本进程启动以来的累计值，不是速率，也不对应某个时间窗口。",
        accepted: "已接受",
        stored: "已存储",
        writeFailed: "写入失败",
        dropped: "已丢弃",
        rejected: "已拒绝",
      },
      export: {
        title: "导出工作器",
        started: "工作器已启动",
        ticks: "工作器轮次",
        tasksRun: "已执行任务",
        tasksCompleted: "已完成任务",
        tasksFailed: "失败任务",
        failures: "失败次数",
        disabledTicks: "导出关闭的轮次",
        cleanups: "清理轮次",
        cleanedFiles: "已删除临时文件",
      },
      cleanup: {
        title: "未关联清理",
        runs: "清理轮次",
        deleted: "已删除 Trace",
        failures: "失败次数",
        lastDeleted: "上轮删除数量",
        backlogLabel: "已过计划清理点的未关联积压",
        backlogState: {
          unavailable: "未测量",
          measured: "精确计数",
          at_least: "至少这么多",
          unknown: "无法识别的积压状态",
        },
        backlogLimit: "探测上限",
        atLeastNote: "计数在探测上限处停止，因此真实积压至少这么多。",
        unavailableNote: "计数无法取得；这不代表积压为零。",
      },
    },
  },
};
