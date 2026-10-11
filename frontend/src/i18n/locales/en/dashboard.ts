// w58 conflict-resolution fusion notes (second wave, B10 — funds_or_state / semantic_conflict)
// en 侧 dashboard 模块的逐类裁决。中英成对硬约束：zh/dashboard.ts 由 w61 认领（notes 在其文件顶部），
// 本模块的任何增删键都必须 en + zh 双侧同时发生，否则 localeKeyCompleteness 的
// "keeps English and Chinese locale schemas identical" 断言必挂。
// 三方基准：b0-base / b0-ours / b0-upstream。叶子键数 base 848 / ours 857 / upstream 885 / 工作树 859。
// 分类口径：E = 我方纯新增（base 无）、C = 上游纯新增（base 无）、D = 上游删除（base 有）、
// A = 双方同改 base 值、A2 = 仅上游改 base 值、B = 仅我方改 base 值。
// 实测 A = 0、B = 0 —— 本模块**没有**同 key 异值真冲突，批规则"i18n 同 key 值冲突默认我方"
// 在本文件没有作用对象；所有裁决都落在存在性冲突（E / C / D）与单方改值（A2）上。
// 灌入状态判别（w59 的两种开工形态）：**部分灌入** —— 上游 39 个新键里只有 (2) 的 2 个 redeem 键被
// 自动合并带进工作树（树值逐字等于 b0-upstream），其余 37 个新键以及 (3)(4) 的上游改动**一个都没进来**。
// 因此 (3)(4)(5) 的每一条 keep-ours 断言都是**预防性现状锁**（防 B11 原样灌回），不是回退已发生的合并。
//
// (1) E 类 9 键 —— 我方纯新增，全部保留：
//     dashboard.welfareBalance / dashboard.welfareExpiresAt / modelPlaza.table.group /
//     modelPlaza.table.discountHint / redeem.codeUsed / redeem.codeExpired / redeem.codeNotFound /
//     redeem.codeLocked / redeem.codeRateLimited。
//     措辞校正（供 B11 复核，勿按 D 类处理）：zh 侧 w61 notes 的 (1) 与 (3) 把 welfare 两键和
//     discountHint 描述成"b0-upstream 删除了 base 的这两个键"，实测不成立 —— 这三键在 b0-base 里
//     根本不存在，上游相对 base 的删除集只有 (3) 的两个 maxReasoningMultiplier 键。正确归类是
//     E 类"我方新增、上游从未有过"。裁决不变（两侧都保留），但按 D 类读会凭空造出一个
//     "上游已删"的历史包袱，诱使 B11 去复核并不存在的删除理由。
//
// (2) redeem 段无损并集 —— 本文件唯一已吸收的上游增量：
//     base 原有 codeRedeemSuccess / failedToRedeem / subscriptionRefreshFailed / pleaseEnterCode
//     共 4 键；我方在其上新增 5 个错误分支键，上游新增 2 个加载/刷新失败键
//     redeem.historyLoadFailed / redeem.userRefreshFailed（上游兑换记录新功能的文案）。
//     两侧在 base 之上互不重叠 → 取无损并集，7 键全保留：我方 5 键 default 我方，
//     上游 2 键按批规则"对应上游新功能的键取上游"。已实测树值逐字等于各方来源值。
//     zh 侧同一并集由 w61 的 dashboardLocaleFusion.spec.ts 锁定，本卡不重复其断言。
//
// (3) D 类 2 键 —— 上游改名，必须拒绝（反删除 + 反改名双锁）：
//     modelPlaza.table.maxReasoningMultiplierBadge 与 modelPlaza.table.maxReasoningMultiplierHint
//     在 b0-upstream 里被删，替代品是 modelPlaza.table.reasoningMultiplierBadge / reasoningMultiplierHint
//     （插值集从 {multiplier} 扩成 {effort} + {multiplier}，语义从"max 档倍率"改成"任意档倍率"）。
//     取证：本树唯一消费者是 src/components/modelPlaza/PlazaModelPricingTable.vue 的第 98 与 100 行，
//     两处只传 multiplier 一个插值（数据源 m.pricing.max_reasoning_effort_multiplier），全文件零
//     effort 实参；上游两个替代键在本树零生产者。接受改名 = 生产代码引用两个不存在的键 + 缺 {effort}。
//     按权威表第 7 条"funds 数值保留我方值 + notes 登记双方值"，此处登记双方文案与占位符差异，
//     结论 keep OURS。若终裁要采纳上游语义，收敛单元是"该 .vue 的两处实参 + 双侧 locale 四键同改"，
//     owner 是 PlazaModelPricingTable.vue 的功能卡，不是本卡。
//
// (4) A2 类 5 键 —— 上游单方改值，文案绑定上游未实现的实现，keep OURS：
//     keys.useKeyModal.deepseek.codexConfigTomlHint / keys.useKeyModal.minimax.codexConfigTomlHint /
//     keys.useKeyModal.composite.codexConfigTomlHint / keys.useKeyModal.routedCodex.configTomlHint /
//     keys.useKeyModal.codexModelCatalog.description。
//     上游这 5 条新文案描述的是"remote vs local 模型目录双模式"（配套 (5) 里那 4 个
//     codexModelCatalog 新键 mode / remote / local / oversized）。取证：本树
//     src/components/keys/UseKeyModal.vue 的目录区段只有单一"下载 catalog"路径，无模式切换变量，
//     上游双模式实现整组缺席（w48 已在本文件登记为 NOT ADOPTED 第 5 项，并点名"w58 owns
//     en/dashboard.ts, w61 owns zh/dashboard.ts"）。取上游文案等于让 UI 声明本树不存在的能力。
//     这 5 条与 (5) 的 codexModelCatalog 四键是一个收敛单元，必须一起换，不许半吸收。
//
// (5) C 类 37 键 —— 上游纯新增，本卡不补（暂缓，跨卡依赖）。分组键数：
//     keys.bulkEdit 13 / keys.providerLabel 1 / keys.providers 4 / keys.providerHints 4 /
//     keys.useKeyModal.cliTabs.systemOne 1 / keys.useKeyModal.typesafe 2 /
//     keys.useKeyModal.codexModelCatalog 4 / usage 2 / monitorCommon 2 / availableChannels.pricing 2 /
//     modelPlaza.table 2 = 37。
//     按"有无生产消费者"拆两组，两组都不单方补入：
//     (5a) 5 键曾在本树有生产消费者 —— keys.bulkEdit.title / selectedCount / selectKey /
//          clearSelection 与 keys.providerLabel，消费者 src/views/user/KeysView.vue（属 w69 派发单）。
//          开工时该文件的上游 bulkEdit hunk 已被自动合并灌入（b0-ours 引用数 0，灌入态 5 处），
//          这 5 键正是 localeKeyCompleteness 既有失败 6 键里的 5 键（第 6 键
//          admin.accounts.modelMappingConflict 属 admin 模块，不在本文件）。
//          **2026-10-11 16:26 w69 已把该 hunk 回退**（当时的 blob 1ed621ec，代码层引用数归 0），
//          实测 localeKeyCompleteness 的缺失键集随之从 6 键自愈到 1 键。该 hunk 本身也不自洽：
//          它引用 @/utils/keyGroupProviders，而该模块在本树不存在（全仓唯一引用者就是那个 .vue），
//          vue-tsc 报 TS2307，补键救不了它 —— 所以本卡不补、也不能靠补键让它绿。
//          blob 时效声明：w69 在 16:33 之后仍在继续编辑该文件（本卡 16:46 复测已是 c5001799），
//          所以这里不钉具体 blob 值；(5a) 的守卫是**代码层引用扫描**（剥注释后 not-to-contain），
//          w69 注记里出现的裸键名不会误触发，M12 变异（往该 .vue 的 script 注入一个 hunk 键调用）
//          实测恰好转红 1 条。
//          终态：37 键当前**全部零消费者**。(5a) 只保留一个**回退现状锁**：若 w69 或 B11 重新采纳
//          该 hunk，spec 的 notes (5a) 断言转红，正确动作是"双侧同时补这 5 键"（走 (i)）或
//          "维持回退"（走 (ii)），二选一由 KeysView.vue 的 owner 定，本卡不代决。
//     (5b) 32 键在本树零消费者 → 标准暂缓。其中 codexModelCatalog 4 + typesafe 2 + cliTabs.systemOne 1
//          共 7 键的 owner 是 w48（UseKeyModal.vue，已显式 BLOCKED），providers / providerHints /
//          bulkEdit 其余 8 键的 owner 是 w69，usage.outputTps 2 / monitorCommon 2 /
//          availableChannels.pricing 2 的 owner 是 monitor / model-plaza 功能卡。
//     报警锚点：spec 的上游独键缺席哨兵（37 项，en + zh 双侧）+ en 叶子键总数锁 859 + 分组求和簿记。
//
// (6) 注释块安全形态：localeKeyCompleteness 的 collectStaticSourceKeys 用 glob 扫源码提取翻译键，
//     它只排除 the __tests__ 目录和 spec/test 结尾的文件 —— locale 源文件本体在扫描范围内。
//     所以本块只写裸点分键名，绝不写带引号串的翻译调用形态（t 括号、$t 括号、i18n 点 t 括号三种），
//     也不写 keypath / titleKey / descriptionKey 加引号串的引用形态，否则 (5) 的暂缓键会被当成
//     生产引用，"单侧暂缓"立刻升级成双侧红灯。spec 有一条幻影引用自检断言守这条（期望集为空）。
//     另：本块刻意用行注释而非块注释，规避 glob 里的星号斜杠序列提前闭合注释（w33 自踩过）。
//
// (7) 变异电池（15 轮，沙箱 /tmp/w58mut 跑批，真树字节未动）—— 证明上述每条锁都非空洞：
//     M1 灌 1 个上游独键 → 3 红（缺席哨兵 + 总数锁 + 跨语言键集）；M2 采纳替代品双键 → 4 红；
//     M3/M4 采纳上游 A2 文案 → 各 1 红；M5 删我方 E 类键 → 7 红（跨 w61 的 3 条也报警）；
//     M6 删已吸收上游键 → 6 红；M8 削弱 presentPaths helper → 3 红；M9 往注释注入调用形态
//     → 1 红（幻影引用自检）；M10 往注释注入**裸键名** → 全绿（反向对照，证明 (6) 的安全形态
//     不被自己的 notes 喂饱）；M11/M12 改两个消费者 .vue → 各 1 红；M13 篡改 37 键清单 → 1 红
//     （簿记非空洞）；M14 灌 CJK → 1 红；M15 灌一个**未登记**的新键 → 2 红（只有总数锁 +
//     跨语言键集报警，缺席哨兵清单覆盖不到 —— 这就是总数锁存在的理由）。
//     **M7 是这轮唯一先 GREEN 后修掉的盲区**：只改 zh 侧已吸收上游键的文案（占位符不变）时，
//     w61 的 spec 只对该键做"存在且非空"、本卡 spec 只做键集/占位符 parity，两侧都不比字面值 →
//     26 例全绿。修法是在**本卡 spec** 里加一条跨卡只读锁（REDEEM_ZH_ABSORBED_VALUES 钉 zh 两个
//     已吸收键的逐字上游值），不改 w61 的字节。加完后 M7 转红 1 条。
//
// 非本卡 scope 但不改动、仅登记观察：本文件与 zh 侧的四态叶子键集差异数全为 0
// （base / ours / upstream / tree 四个版本下 en 与 zh 的键集完全同构），所以 en 卡可直接复用
// zh 卡的裁决口径，反之亦然；两侧 notes 若措辞不一致会让 B11 读到矛盾记录，(1) 的校正即为此而写。

export default {
  dashboard: {
    title: 'Dashboard',
    welcomeMessage: "Welcome back! Here's an overview of your account.",
    balance: 'Balance',
    welfareBalance: 'Gift balance',
    welfareExpiresAt: 'clears at {time}',
    apiKeys: 'API Keys',
    todayRequests: 'Today Requests',
    todayCost: 'Today Cost',
    todayTokens: 'Today Tokens',
    totalTokens: 'Total Tokens',
    cacheToday: 'Cache (Today)',
    performance: 'Performance',
    avgResponse: 'Avg Response',
    averageTime: 'Average time',
    timeRange: 'Time Range',
    granularity: 'Granularity',
    day: 'Day',
    hour: 'Hour',
    modelDistribution: 'Model Distribution',
    groupDistribution: 'Group Usage Distribution',
    platformBreakdown: 'Per-platform Breakdown',
    platformBreakdownEmpty: 'No platform usage yet',
    platformCount: '{count} platforms',
    platformOther: 'Other',
    platformQuota: {
      title: 'Quota Usage',
      daily: 'Daily',
      weekly: 'Weekly',
      monthly: 'Monthly (30-day rolling)',
      resetsAt: 'Resets {time}',
      noLimit: 'unlimited',
      disabled: 'Disabled',
    },
    tokenUsageTrend: 'Token Usage Trend',
    noDataAvailable: 'No data available',
    model: 'Model',
    group: 'Group',
    noGroup: 'No Group',
    requests: 'Requests',
    tokens: 'Tokens',
    actual: 'Actual',
    standard: 'Standard',
    input: 'Input',
    output: 'Output',
    cache: 'Cache',
    recentUsage: 'Recent Usage',
    last7Days: 'Last 7 days',
    noUsageRecords: 'No usage records',
    startUsingApi: 'Start using the API to see your usage history here.',
    viewAllUsage: 'View all usage',
    quickActions: 'Quick Actions',
    createApiKey: 'Create API Key',
    generateNewKey: 'Generate a new API key',
    batchImageAgent: 'Batch Image Assistant',
    batchImageAgentDesc: 'Copy instructions for an agent',
    viewUsage: 'View Usage',
    checkDetailedLogs: 'Check detailed usage logs',
    redeemCode: 'Redeem Code',
    addBalanceWithCode: 'Add balance with a code'
  },

  // Groups (shared)
  groups: {
    subscription: 'Sub'
  },

  // API Keys
  keys: {
    title: 'API Keys',
    description: 'Manage your API keys and access tokens',
    searchPlaceholder: 'Search name or key...',
    endpoints: {
      title: 'API Endpoints',
      default: 'Default',
      copied: 'Copied',
      copiedHint: 'Copied to clipboard',
      clickToCopy: 'Click to copy this endpoint',
      speedTest: 'Speed Test',
    },
    allGroups: 'All Groups',
    allStatus: 'All Status',
    columnSettings: 'Column Settings',
    columnAlwaysVisible: 'This column is always visible',
    createKey: 'Create API Key',
    editKey: 'Edit API Key',
    deleteKey: 'Delete API Key',
    deleteConfirmMessage: "Are you sure you want to delete '{name}'? This action cannot be undone.",
    id: 'ID',
    apiKey: 'API Key',
    group: 'Group',
    currentConcurrency: 'Current Concurrency',
    noGroup: 'No group',
    searchGroup: 'Search groups...',
    noGroupFound: 'No groups found',
    created: 'Created',
    copyToClipboard: 'Copy to clipboard',
    copied: 'Copied!',
    importToCcSwitch: 'Import to CCS',
    enable: 'Enable',
    disable: 'Disable',
    nameLabel: 'Name',
    namePlaceholder: 'My API Key',
    groupLabel: 'Group',
    selectGroup: 'Select a group',
    statusLabel: 'Status',
    selectStatus: 'Select status',
    saving: 'Saving...',
    noKeysYet: 'No API keys yet',
    createFirstKey: 'Create your first API key to get started with the API.',
    keyCreatedSuccess: 'API key created successfully',
    keyUpdatedSuccess: 'API key updated successfully',
    keyDeletedSuccess: 'API key deleted successfully',
    keyEnabledSuccess: 'API key enabled successfully',
    keyDisabledSuccess: 'API key disabled successfully',
    failedToLoad: 'Failed to load API keys',
    failedToSave: 'Failed to save API key',
    failedToDelete: 'Failed to delete API key',
    failedToUpdateStatus: 'Failed to update API key status',
    clickToChangeGroup: 'Click to change group',
    groupChangedSuccess: 'Group changed successfully',
    failedToChangeGroup: 'Failed to change group',
    groupRequired: 'Please select a group',
    usage: 'Usage',
    today: 'Today',
    total: 'Last 30d',
    quota: 'Quota',
    lastUsedAt: 'Last Used',
    lastUsedIP: 'Last Used IP',
    useKey: 'Use Key',
    useKeyModal: {
      title: 'Use API Key',
      description:
        'Add the following environment variables to your terminal profile or run directly in terminal to configure API access.',
      copy: 'Copy',
      copied: 'Copied',
      note: 'These environment variables will be active in the current terminal session. For permanent configuration, add them to ~/.bashrc, ~/.zshrc, or the appropriate configuration file.',
      claudeSettingsHint: 'User-level persistent configuration. Do not commit this file containing your API key to a project repository.',
      noGroupTitle: 'Please assign a group first',
      noGroupDescription: 'This API key has not been assigned to a group. Please click the group column in the key list to assign one before viewing the configuration.',
      openai: {
        description: 'Add the following configuration files to your Codex CLI config directory.',
        authModeTitle: 'Codex authentication mode',
        authModeDescription: 'Compatibility mode keeps the existing setup for older Codex clients. API Key Mode authorizes the client-side image executor.',
        authModeLegacy: 'Compatibility mode',
        authModeApiKey: 'API Key Mode',
        authModeApiKeyRestartNotice: 'After saving this configuration, completely quit and restart Codex Desktop or CLI, then create a new task so the client can rebuild its tool registry.',
        configTomlHint: 'Make sure the following content is at the beginning of the config.toml file',
        note: 'Make sure the config directory exists. macOS/Linux users can run mkdir -p ~/.codex to create it.',
        noteWindows: 'Press Win+R and enter %userprofile%\\.codex to open the config directory. Create it manually if it does not exist.',
      },
      cliTabs: {
        claudeCode: 'Claude Code',
        geminiCli: 'Gemini CLI',
        codexCli: 'Codex CLI',
        codexCliWs: 'Codex CLI (WebSocket)',
        grokCli: 'Grok CLI',
        opencode: 'OpenCode',
      },
      antigravity: {
        description: 'Configure API access for Antigravity group. Select the configuration method based on your client.',
        claudeCode: 'Claude Code',
        geminiCli: 'Gemini CLI',
        claudeNote: 'These environment variables will be active in the current terminal session. For permanent configuration, add them to ~/.bashrc, ~/.zshrc, or the appropriate configuration file.',
        geminiNote: 'These environment variables will be active in the current terminal session. For permanent configuration, add them to ~/.bashrc, ~/.zshrc, or the appropriate configuration file.',
      },
      gemini: {
        description: 'Add the following environment variables to your terminal profile or run directly in terminal to configure Gemini CLI access.',
        modelComment: 'If you have Gemini 3 access, you can use: gemini-3-pro-preview',
        note: 'These environment variables will be active in the current terminal session. For permanent configuration, add them to ~/.bashrc, ~/.zshrc, or the appropriate configuration file.',
      },
      grok: {
        description:
          'Configure Grok CLI, Claude Code, Codex, or OpenCode to send requests through your Sub2API Grok group. Text models use Responses; image/video use Imagine model IDs on media endpoints.',
        claudeDescription: 'Configure Claude Code to send Messages API traffic through your Sub2API Grok group.',
        codexDescription: 'Configure Codex to send Responses API traffic through your Sub2API Grok group.',
        configTomlHint:
          'Official path: ~/.grok/config.toml (or $GROK_HOME). Fill [endpoints] (models_base_url / models_list_url / xai_api_base_url / cli_chat_proxy_base_url), [auth] preferred_method=api_key, [models], [session], and [features] image/video overrides. Prefer env_key over api_key; every text model needs api_backend=responses. Back up before merge, then run grok inspect.',
        codexConfigTomlHint:
          'Official Codex: wire_api = "responses" only; prefer env_key over experimental_bearer_token; supports_websockets = false for non-OpenAI gateways (Sub2API can still accept client WS and bridge to HTTP/SSE). Back up ~/.codex/config.toml before merge.',
        note:
          'Export GROK_MODELS_BASE_URL and XAI_API_KEY, save the full config.toml (endpoints/auth/models/session/features) as ~/.grok/config.toml, run grok inspect, then /model grok-4.5 (or grok-build-0.1 for coding).',
        noteWindows:
          'Set GROK_MODELS_BASE_URL and XAI_API_KEY, save the full config.toml as %USERPROFILE%\\.grok\\config.toml, run grok inspect, then /model grok-4.5 (or grok-build-0.1 for coding).',
        claudeNote:
          'Choose one method: terminal env for this session, or ~/.claude/settings.json for persistence. Do not commit files that contain your API key.',
        codexNote:
          'Export SUB2API_API_KEY, save config.toml under ~/.codex (mkdir -p ~/.codex). Prefer env_key auth; do not commit secrets.',
        codexNoteWindows:
          'Set $env:SUB2API_API_KEY, save config.toml under %USERPROFILE%\\.codex. Prefer env_key auth; do not commit secrets.',
      },
      deepseek: {
        description: 'Configure Claude Code, Codex, or OpenCode through the current DeepSeek group.',
        codexDescription: 'Configure Codex with API key authentication through the current DeepSeek group.',
        codexConfigTomlHint: 'Download the model catalog below, save both files under the Codex config directory, and restart Codex.',
        codexNote: 'Export SUB2API_API_KEY before starting Codex. The downloaded catalog contains model metadata only, not your API key.',
      },
      minimax: {
        description: 'Configure Claude Code, Codex, or OpenCode through the current MiniMax group.',
        codexDescription: 'Configure Codex with API key authentication through the current MiniMax group.',
        codexConfigTomlHint: 'Download the model catalog below, save both files under the Codex config directory, and restart Codex.',
        codexNote: 'Export SUB2API_API_KEY before starting Codex. The downloaded catalog contains model metadata only, not your API key.',
      },
      composite: {
        description: 'Configure supported clients through the current Composite routing group.',
        codexDescription: 'Configure Codex with API key authentication and the complete model catalog for this Composite group.',
        codexConfigTomlHint: 'Download the model catalog below, save both files under the Codex config directory, and restart Codex.',
        codexNote: 'Export SUB2API_API_KEY before starting Codex. Model requests are routed by the selected catalog slug.',
      },
      routedCodex: {
        description: 'Configure Codex with the complete model catalog for the current routed group.',
        configTomlHint: 'Download the model catalog below, save both files under the Codex config directory, and restart Codex.',
        note: 'Export SUB2API_API_KEY before starting Codex. The downloaded catalog contains model metadata only, not your API key.',
      },
      codexModelCatalog: {
        title: 'Codex model catalog',
        description: 'Fetch with this API key, then save the catalog at the path referenced by config.toml.',
        fetch: 'Fetch catalog',
        retry: 'Retry',
        download: 'Download catalog',
        modelsCount: '{count} models ready to download',
        errorDescription: 'The catalog could not be fetched with this API key.',
      },
      opencode: {
        title: 'OpenCode Example',
        subtitle: 'opencode.json',
        hint: 'Config path: ~/.config/opencode/opencode.json (or opencode.jsonc), create if not exists. Use default providers (openai/anthropic/google) or custom provider_id. API Key can be configured directly or via /connect command. This is an example, adjust models and options as needed.',
      },
    },
    customKeyLabel: 'Custom Key',
    customKeyPlaceholder: 'Enter your custom key (min 16 chars)',
    customKeyHint: 'Only letters, numbers, underscores and hyphens allowed. Minimum 16 characters.',
    customKeyTooShort: 'Custom key must be at least 16 characters',
    customKeyInvalidChars: 'Custom key can only contain letters, numbers, underscores, and hyphens',
    customKeyRequired: 'Please enter a custom key',
    ipRestriction: 'IP Restriction',
    ipWhitelist: 'IP Whitelist',
    ipWhitelistPlaceholder: '192.168.1.100\n10.0.0.0/8',
    ipWhitelistHint: 'One IP or CIDR per line. Only these IPs can use this key when set.',
    ipBlacklist: 'IP Blacklist',
    ipBlacklistPlaceholder: '1.2.3.4\n5.6.0.0/16',
    ipBlacklistHint: 'One IP or CIDR per line. These IPs will be blocked from using this key.',
    ipRestrictionEnabled: 'IP restriction enabled',
    ccSwitchNotInstalled: 'CC-Switch is not installed or the protocol handler is not registered. Please install CC-Switch first or manually copy the API key.',
    ccsClientSelect: {
      title: 'Select Client',
      description: 'Please select the client type to import to CC-Switch:',
      claudeCode: 'Claude Code',
      claudeCodeDesc: 'Import as Claude Code configuration',
      geminiCli: 'Gemini CLI',
      geminiCliDesc: 'Import as Gemini CLI configuration',
    },
    // Quota and expiration
    quotaLimit: 'Quota Limit',
    quotaAmount: 'Quota Amount (USD)',
    quotaAmountPlaceholder: 'Enter quota limit in USD',
    quotaAmountHint: 'Set the maximum amount this key can spend. 0 = unlimited.',
    quotaUsed: 'Quota Used',
    reset: 'Reset',
    resetQuotaUsed: 'Reset used quota to 0',
    resetQuotaTitle: 'Confirm Reset Quota',
    resetQuotaConfirmMessage: 'Are you sure you want to reset the used quota (${used}) for key "{name}" to 0? This action cannot be undone.',
    quotaResetSuccess: 'Quota reset successfully',
    failedToResetQuota: 'Failed to reset quota',
    rateLimitColumn: 'Rate Limit',
    rateLimitSection: 'Rate Limit',
    resetUsage: 'Reset',
    rateLimit5h: '5-Hour Limit (USD)',
    rateLimit1d: 'Daily Limit (USD)',
    rateLimit7d: '7-Day Limit (USD)',
    rateLimitHint: 'Set the maximum spending for this key within each time window. 0 = unlimited.',
    rateLimitUsage: 'Rate Limit Usage',
    resetRateLimitUsage: 'Reset Rate Limit Usage',
    resetRateLimitTitle: 'Confirm Reset Rate Limit',
    resetRateLimitConfirmMessage: 'Are you sure you want to reset the rate limit usage for key "{name}"? All time window usage will be reset to zero. This action cannot be undone.',
    rateLimitResetSuccess: 'Rate limit usage reset successfully',
    failedToResetRateLimit: 'Failed to reset rate limit usage',
    resetNow: 'Resetting soon',
    expiration: 'Expiration',
    expiresInDays: '{days} days',
    extendDays: '+{days} days',
    customDate: 'Custom',
    expirationDate: 'Expiration Date',
    expirationDateHint: 'Select when this API key should expire.',
    currentExpiration: 'Current expiration',
    expiresAt: 'Expires',
    noExpiration: 'Never',
    status: {
      active: 'Active',
      inactive: 'Inactive',
      quota_exhausted: 'Quota Exhausted',
      expired: 'Expired',
    },
  },

  // Usage
  usage: {
    title: 'Usage Records',
    description: 'View and analyze your API usage history',
    costDetails: 'Cost Breakdown',
    tokenDetails: 'Token Breakdown',
    cacheTtlOverriddenHint: 'Cache TTL Override enabled',
    cacheTtlOverriddenLabel: 'TTL Override',
    cacheTtlOverridden5m: 'Billed as 5m',
    cacheTtlOverridden1h: 'Billed as 1h',
    totalRequests: 'Total Requests',
    totalTokens: 'Total Tokens',
    cacheTotal: 'Cache',
    cacheBreakdown: 'Cache Token Breakdown',
    cacheCreationTokensLabel: 'Cache Creation',
    cacheReadTokensLabel: 'Cache Read',
    totalCost: 'Total Cost',
    standardCost: 'Standard',
    actualCost: 'Actual',
    accountCost: 'Cost',
    userBilled: 'User billed',
    accountBilled: 'Account billed',
    resetNow: 'Now',
    resetPending: 'Pending refresh',
    accountMultiplier: 'Account rate',
    avgDuration: 'Avg Duration',
    inSelectedRange: 'in selected range',
    perRequest: 'per request',
    apiKeyFilter: 'API Key',
    allApiKeys: 'All API Keys',
    timeRange: 'Time Range',
    exportCsv: 'Export CSV',
    exportExcel: 'Export Excel',
    exportingProgress: 'Exporting data...',
    exportedCount: 'Exported {current}/{total} records',
    estimatedTime: 'Estimated time remaining: {time}',
    cancelExport: 'Cancel Export',
    exportCancelled: 'Export cancelled',
    exporting: 'Exporting...',
    preparingExport: 'Preparing export...',
    model: 'Model',
    requestedModel: 'Requested',
    upstreamModel: 'Upstream',
	  sentUpstreamModel: 'Sent upstream',
	  upstreamResponseModel: 'Upstream response',
	  upstreamModelMismatch: 'Response model mismatch',
	  modelVariant: 'Possible version variant',
	  modelMismatch: 'Different model',
    reasoningEffort: 'Reasoning Effort',
    requestedReasoningEffort: 'Requested reasoning effort',
    endpoint: 'Endpoint',
    endpointDistribution: 'Endpoint Distribution',
    inbound: 'Inbound',
    upstream: 'Upstream',
    mapping: 'Mapping',
    path: 'Path',
    inboundEndpoint: 'Inbound Endpoint',
    upstreamEndpoint: 'Upstream Endpoint',
    type: 'Type',
    tokens: 'Tokens',
    cost: 'Cost',
    firstToken: 'First Token',
    duration: 'Duration',
    latency: 'Latency',
    latencyFirstToken: 'First',
    latencyDuration: 'Total',
    time: 'Time',
    ws: 'WS',
    stream: 'Stream',
    sync: 'Sync',
    nativeCompactionV2: 'Compaction',
    compactionFilter: 'Request Kind',
    allCompactionTypes: 'All Requests',
    compactionOnly: 'Compaction Only',
    cyber: 'Cyber',
    live: 'Live',
    unknown: 'Unknown',
    in: 'In',
    out: 'Out',
    cacheHit: 'Cache hit',
    cacheCreate: 'Cache create',
    cacheHitRate: 'Cache hit rate',
    inputTokenPrice: 'Input price',
    outputTokenPrice: 'Output price',
    perMillionTokens: '/ 1M tokens',
    unitPrice: 'Per-request price',
    imageUnitPrice: 'Per-image price',
    imageTotalPrice: 'Image total price',
    imageCount: 'Image count',
    imageBillingSize: 'Billing size',
    imageInputSize: 'Input size',
    imageOutputSize: 'Output size',
    imageInputTokens: 'Image Input Tokens',
    imageInputTokenPrice: 'Image Input Price',
    imageInputCost: 'Image Input Cost',
    imageOutputTokens: 'Image Output Tokens',
    imageOutputTokenPrice: 'Image Output Price',
    imageOutputCost: 'Image Output Cost',
    imageSizeSource: 'Size source',
    imageSizeBreakdown: 'Size breakdown',
    imageSizeSourceOutput: 'Upstream output',
    imageSizeSourceInput: 'Request input',
    imageSizeSourceDefault: 'Default billing tier',
    imageSizeSourceLegacy: 'Legacy record',
    imageSizeSourceMissing: 'Not recorded',
    imageSizeNotRecorded: 'not recorded',
    imageSizeLegacyUnstandardized: 'legacy unstandardized',
    imageSizeUnknown: 'unknown',
    cacheRead: 'Read',
    cacheWrite: 'Write',
    serviceTier: 'Service tier',
    serviceTierPriority: 'Fast',
    serviceTierUltrafast: 'Ultrafast',
    serviceTierFlex: 'Flex',
    serviceTierStandard: 'Standard',
    rate: 'Rate',
    original: 'Original',
    billed: 'Billed',
    noRecords: 'No usage records found. Try adjusting your filters.',
    failedToLoad: 'Failed to load usage logs',
    noDataToExport: 'No data to export',
    exportSuccess: 'Usage data exported successfully',
    exportFailed: 'Failed to export usage data',
    exportExcelSuccess: 'Usage data exported successfully (Excel format)',
    exportExcelFailed: 'Failed to export usage data',
    imageUnit: ' images',
    userAgent: 'User-Agent',
    ipGeo: {
      fetch: 'Fetch region',
      fetching: 'Fetching...',
      failed: 'Failed',
      private: 'Private address',
      refreshTitle: 'Refresh region info',
      batchFetch: 'Batch fetch regions',
      batchFetching: 'Fetching...',
      pending: '{count} IPs pending',
      batchFailed: 'Failed to batch fetch IP regions',
      detailOrg: 'ISP',
      detailTimezone: 'Timezone',
      detailAccuracy: 'Accuracy',
      detailCoordinates: 'Coordinates',
    },
    tabs: { usage: 'Usage', errors: 'Error Requests', ranking: 'User Ranking' },
    errors: {
      time: 'Time', model: 'Model', endpoint: 'Endpoint', status: 'Status',
      category: 'Category', platform: 'Platform', message: 'Message',
      keyName: 'Key Name', keyDeleted: 'Deleted', allKeys: 'All keys',
      modelPlaceholder: 'Search model', allCategories: 'All categories', allStatuses: 'All status codes',
      empty: 'No error requests', failedToLoad: 'Failed to load error requests',
      categories: {
        auth: 'Auth failed', rate_limit: 'Rate limited', quota: 'Balance/Subscription',
        invalid_request: 'Invalid request', service_unavailable: 'Service unavailable',
        upstream: 'Upstream error', internal: 'Platform error', other: 'Other', cyber: 'Cyber policy',
      },
      detail: {
        title: 'Error Request Detail',
        responseBody: 'Response Body',
        upstreamStatus: 'Upstream Status',
        loadFailed: 'Failed to load detail, please try again',
      },
    },
  },

  // Shared keys for channel monitor (admin + user views)
  monitorCommon: {
    status: {
      operational: 'Operational',
      degraded: 'Degraded',
      failed: 'Failed',
      error: 'Error',
      unknown: '-'
    },
    providers: {
      openai: 'OpenAI',
      anthropic: 'Anthropic',
      gemini: 'Gemini',
      grok: 'Grok',
      antigravity: 'Antigravity',
      kimi: 'Kimi',
      zhipu: 'Zhipu GLM',
      deepseek: 'DeepSeek',
      minimax: 'MiniMax'
    },
    // Check modes (how a monitor performs its checks)
    checkMode: {
      probe: 'Probe',
      quota: 'Quota',
      quota_probe: 'Probe + Quota'
    },
    // Quota snapshot rendering (MonitorQuotaView, shared by admin + user views)
    quota: {
      unavailable: 'Quota unavailable',
      windows: {
        '5h': '5h',
        '7d': '7d',
        '7dSonnet': '7d Sonnet',
        '7dFable': '7d Fable',
        weekly: 'Weekly',
        daily: 'Daily',
        '30d': '30d',
        total: 'Total'
      },
      labels: {
        requests: 'Requests',
        tokens: 'Tokens',
        shared: 'Shared',
        pro: 'Pro',
        flash: 'Flash'
      }
    },
    extraModelsHeader: 'Extra Models',
    extraModelsEmpty: 'No extra models',
    latencyEmpty: '-',
    availabilityPrefix: 'Availability',
    dialogLatency: 'Dialog Latency',
    endpointPing: 'Endpoint PING',
    history60pts: 'HISTORY ({n} PTS)',
    nextUpdateIn: 'NEXT UPDATE IN {n}s',
    past: 'PAST',
    now: 'NOW',
    maintenancePaused: 'Maintenance · timeline paused',
    extraModelsCount: '+ {n} models',
    pollEvery: '{n}s polling',
    updatedAt: 'Updated {time}',
    relativeSecondsAgo: '{n}s ago',
    relativeMinutesAgo: '{n}m ago',
    relativeHoursAgo: '{n}h ago',
    relativeDaysAgo: '{n}d ago'
  },

  // Channel Status (user-facing read-only view)
  channelStatus: {
    title: 'Channel Status',
    description: 'Inspect channel availability, latency and recent status',
    searchPlaceholder: 'Search channels...',
    allProviders: 'All Providers',
    loadError: 'Failed to load channel status',
    detailLoadError: 'Failed to load channel detail',
    detailTitle: 'Channel Detail',
    closeDetail: 'Close',
    windowTab: {
      '7d': '7 days',
      '15d': '15 days',
      '30d': '30 days'
    },
    overall: {
      operational: 'OPERATIONAL',
      degraded: 'DEGRADED',
      unavailable: 'UNAVAILABLE'
    },
    columns: {
      name: 'Name',
      provider: 'Provider',
      groupName: 'Group',
      primaryModel: 'Primary Model',
      availability7d: '7d Availability',
      latency: 'Latency (ms)'
    },
    detailColumns: {
      model: 'Model',
      latestStatus: 'Latest Status',
      latestLatency: 'Latest Latency (ms)',
      availability7d: '7d Availability',
      availability15d: '15d Availability',
      availability30d: '30d Availability',
      avgLatency7d: '7d Avg Latency (ms)'
    },
    empty: {
      title: 'No channels available',
      description: 'No monitored channels have been configured yet.'
    }
  },

  // Available Channels (user-facing)
  availableChannels: {
    title: 'Available Channels',
    description: 'Channels you can access, along with their supported models and pricing',
    searchPlaceholder: 'Search channels or models...',
    empty: 'No available channels',
    noModels: 'No models configured',
    noPricing: 'Pricing not configured',
    exclusive: 'Exclusive',
    public: 'Public',
    exclusiveTooltip: 'Exclusive groups granted to you by an admin',
    publicTooltip: 'Groups open to all users',
    columns: {
      name: 'Channel',
      description: 'Description',
      platform: 'Platform',
      groups: 'Your Accessible Groups',
      supportedModels: 'Supported Models'
    },
    pricing: {
      billingMode: 'Billing Mode',
      billingModeToken: 'Per Token',
      billingModePerRequest: 'Per Request',
      billingModeImage: 'Per Image',
      billingModeVideo: 'Per Video',
      inputPrice: 'Input',
      outputPrice: 'Output',
      cacheWritePrice: 'Cache Write',
      cacheWrite5mPrice: 'Cache Write (5m)',
      cacheWrite1hPrice: 'Cache Write (1h)',
      cacheReadPrice: 'Cache Read',
      imageInputPrice: 'Image Input',
      imageOutputPrice: 'Image Output',
      perRequestPrice: 'Per Request',
      intervals: 'Tiered Pricing',
      unitPerMillion: '/ 1M tokens',
      unitPerRequest: '/ request'
    }
  },

  // Model Plaza (public group/model pricing showcase)
  modelPlaza: {
    title: 'Model Plaza',
    description: 'Browse available models and pricing by group',
    loading: 'Loading...',
    empty: 'No groups to display',
    loadFailed: 'Failed to load model plaza',
    noSearchResult: 'No matching models',
    anonymousHint: 'Sign in to see your exclusive groups and personal rates',
    filters: {
      platformLabel: 'Platform',
      groupLabel: 'Group',
      rateLabel: 'Rate',
      modelLabel: 'Model',
      searchPlaceholder: 'Search models',
      all: 'All'
    },
    badges: {
      exclusive: 'Exclusive',
      subscription: 'Subscription'
    },
    detail: {
      noModels: 'No models configured for this group',
      noPricing: 'Pricing not configured',
      peakNote: 'Peak hours {window}: billing rate ×{multiplier}',
      longContextDisabledNote: 'Long-context tier pricing is disabled for this group: requests above the threshold are billed at the base tier; official tiers are for reference only'
    },
    table: {
      model: 'Model',
      group: 'Group',
      input: 'Input',
      output: 'Output',
      cache: 'Cache',
      cacheWrite: 'Write',
      cacheRead: 'Read',
      cacheWriteShort: 'W',
      cacheReadShort: 'R',
      tierHint: 'The whole request is billed at the tier matching its total context (input + cache write + cache read)',
      tierHintMarginal: 'Only the portion above the threshold is billed at this tier; output is unaffected',
      maxReasoningMultiplierBadge: 'Max ×{multiplier}',
      maxReasoningMultiplierHint: 'When the forwarded reasoning effort is max, billing and quota usage for the request are multiplied by {multiplier}',
      marginalBadge: 'excess-only tiers',
      timePricingRowHint: 'Requests made within this period ({timezone} time) are billed at the prices in this row',
      timePricingRowHintWeekdays:
        'On weekdays (Mon–Fri) only, requests made within this period ({timezone} time) are billed at the prices in this row; weekends use the standard prices',
      timePricingRowHintPeak:
        '; prices in this row exclude the peak-hour rate — where this period overlaps the peak hours {window}, the overlapping portion is additionally multiplied by ×{multiplier}',
      timePricingWeekdays: 'Weekdays',
      timePricingRateHint: 'Effective rate {rate} × period multiplier {multiplier}',
      paidPrice: 'Your Price (Discounted)',
      officialPrice: 'Official Price',
      discountHint: 'Paid price ÷ official reference price (same traffic cache-mix basis)',
      rate: 'Rate',
      unitPerMillion: '$ / 1M tokens',
      perUnitRequest: '/ request',
      perUnitImage: '/ image',
      perRequest: 'Per request',
      perImage: 'Per image'
    },
    nav: {
      login: 'Sign In',
      backToDashboard: 'Back to Console'
    }
  },

  affiliate: {
    title: 'Affiliate Rebates',
    description: 'Invite new users and convert your rebate quota into account balance',
    yourCode: 'Your Affiliate Code',
    inviteLink: 'Invite Link',
    copyCode: 'Copy Code',
    copyLink: 'Copy Link',
    codeCopied: 'Affiliate code copied',
    linkCopied: 'Invite link copied',
    loadFailed: 'Failed to load affiliate data',
    transferFailed: 'Failed to transfer affiliate quota',
    stats: {
      rebateRate: 'My Rebate Rate',
      rebateRateHint: 'What you earn each time an invitee recharges',
      invitedUsers: 'Invited Users',
      availableQuota: 'Available Rebate Quota',
      frozenQuota: 'Frozen',
      frozenQuotaHint: 'Recently earned rebates pending release',
      totalQuota: 'Historical Rebate Quota'
    },
    transfer: {
      title: 'Transfer Rebate Quota',
      description: 'Move available rebate quota into your account balance',
      button: 'Transfer to Balance',
      transferring: 'Transferring...',
      empty: 'No available rebate quota',
      success: '{amount} has been transferred to your balance'
    },
    invitees: {
      title: 'Invited Users',
      empty: 'No invited users yet',
      columns: {
        email: 'Email',
        username: 'Username',
        rebate: 'Rebate',
        joinedAt: 'Joined At'
      }
    },
    tips: {
      title: 'How It Works',
      line1: 'Share your affiliate code or invite link with new users.',
      line2: 'When invitees recharge, you receive {rate} of the recharge as rebate quota.',
      line3: 'Transfer rebate quota to balance at any time.',
      line4: 'Newly earned rebates may have a waiting period before they can be transferred.'
    }
  },

  // Redeem
  redeem: {
    title: 'Redeem Code',
    description: 'Enter your redeem code to add balance or increase concurrency',
    currentBalance: 'Current Balance',
    concurrency: 'Concurrency',
    requests: 'requests',
    redeemCodeLabel: 'Redeem Code',
    redeemCodePlaceholder: 'Enter your redeem code',
    redeemCodeHint: 'Redeem codes are case-sensitive',
    redeeming: 'Redeeming...',
    redeemButton: 'Redeem Code',
    redeemSuccess: 'Code Redeemed Successfully!',
    redeemFailed: 'Redemption Failed',
    added: 'Added',
    concurrentRequests: 'concurrent requests',
    newBalance: 'New Balance',
    newConcurrency: 'New Concurrency',
    aboutCodes: 'About Redeem Codes',
    codeRule1: 'Each code can only be used once',
    codeRule2: 'Codes may add balance, increase concurrency, or grant trial access',
    codeRule3: 'Contact support if you have issues redeeming a code',
    codeRule4: 'Balance and concurrency updates are immediate',
    recentActivity: 'Recent Activity',
    historyWillAppear: 'Your redemption history will appear here',
    balanceAddedRedeem: 'Balance Added (Redeem)',
    balanceAddedAffiliate: 'Balance Added (Affiliate Transfer)',
    balanceAddedAdmin: 'Balance Added (Admin)',
    balanceDeductedAdmin: 'Balance Deducted (Admin)',
    concurrencyAddedRedeem: 'Concurrency Added (Redeem)',
    concurrencyAddedAdmin: 'Concurrency Added (Admin)',
    concurrencyReducedAdmin: 'Concurrency Reduced (Admin)',
    adminAdjustment: 'Admin Adjustment',
    subscriptionAssigned: 'Subscription Assigned',
    subscriptionAssignedDesc: 'You have been granted access to {groupName}',
    subscriptionDays: '{days} days',
    days: ' days',
    codeRedeemSuccess: 'Code redeemed successfully!',
    failedToRedeem: 'Failed to redeem code. Please check the code and try again.',
    codeUsed: 'This code has already been used. Each code can only be used once.',
    codeExpired: 'This code has expired and cannot be used.',
    codeNotFound: 'Code not found. Please check and try again.',
    codeLocked: 'This code is being processed. Please try again later.',
    codeRateLimited: 'Too many failed attempts. Please try again later.',
    historyLoadFailed: 'Failed to load activity. Please try again.',
    userRefreshFailed: 'Redeemed successfully, but failed to refresh account information.',
    subscriptionRefreshFailed: 'Redeemed successfully, but failed to refresh subscription status.',
    pleaseEnterCode: 'Please enter a redeem code'
  },

  // Profile
  profile: {
    title: 'Profile Settings',
    description: 'Manage your account information and settings',
    accountBalance: 'Account Balance',
    concurrencyLimit: 'Concurrency Limit',
    rpmLimit: 'RPM Limit',
    rpmUnlimited: 'Unlimited',
    memberSince: 'Member Since',
    overviewTitle: 'Account Overview',
    overviewDescription: 'Check account status, profile sources, and common actions at a glance.',
    basicsTitle: 'Profile & Avatar',
    basicsDescription: 'Keep your public profile details and avatar aligned.',
    linkedProfileSources: 'Profile Sources',
    linkedProfileSourcesDescription: 'Some profile details may stay synced from third-party sign-in methods.',
    securityTitle: 'Security Settings',
    securityDescription: 'Password, two-factor authentication, and alerts live in the right rail.',
    administrator: 'Administrator',
    user: 'User',
    username: 'Username',
    email: 'Email',
    status: 'Status',
    role: 'Role',
    enterUsername: 'Enter username',
    editProfile: 'Edit Profile',
    updateProfile: 'Update Profile',
    updating: 'Updating...',
    updateSuccess: 'Profile updated successfully',
    updateFailed: 'Failed to update profile',
    usernameRequired: 'Username is required',
    changePassword: 'Change Password',
    currentPassword: 'Current Password',
    newPassword: 'New Password',
    confirmNewPassword: 'Confirm New Password',
    passwordHint: 'Password must be at least 8 characters long',
    changingPassword: 'Changing...',
    changePasswordButton: 'Change Password',
    passwordsNotMatch: 'New passwords do not match',
    passwordTooShort: 'Password must be at least 8 characters long',
    passwordChangeSuccess: 'Password changed successfully',
    passwordChangeFailed: 'Failed to change password',
    // TOTP 2FA
    totp: {
      title: 'Two-Factor Authentication (2FA)',
      description: 'Enhance account security with Google Authenticator or similar apps',
      enabled: 'Enabled',
      enabledAt: 'Enabled at',
      notEnabled: 'Not Enabled',
      notEnabledHint: 'Enable two-factor authentication to enhance account security',
      enable: 'Enable',
      disable: 'Disable',
      featureDisabled: 'Feature Unavailable',
      featureDisabledHint: 'Two-factor authentication has not been enabled by the administrator',
      setupTitle: 'Set Up Two-Factor Authentication',
      setupStep1: 'Scan the QR code below with your authenticator app',
      setupStep2: 'Enter the 6-digit code from your app',
      manualEntry: "Can't scan? Enter the key manually:",
      enterCode: 'Enter 6-digit code',
      verify: 'Verify',
      setupFailed: 'Failed to get setup information',
      verifyFailed: 'Invalid code, please try again',
      enableSuccess: 'Two-factor authentication enabled',
      disableTitle: 'Disable Two-Factor Authentication',
      disableWarning: 'After disabling, you will no longer need a verification code to log in. This may reduce your account security.',
      enterPassword: 'Enter your current password to confirm',
      confirmDisable: 'Confirm Disable',
      disableSuccess: 'Two-factor authentication disabled',
      disableFailed: 'Failed to disable, please check your password',
      loginTitle: 'Two-Factor Authentication',
      loginHint: 'Enter the 6-digit code from your authenticator app',
      loginFailed: 'Verification failed, please try again',
      // New translations for email verification
      verifyEmailFirst: 'Please verify your email first',
      verifyPasswordFirst: 'Please verify your identity first',
      emailCode: 'Email Verification Code',
      enterEmailCode: 'Enter 6-digit code',
      sendCode: 'Send Code',
      codeSent: 'Verification code sent to your email',
      sendCodeFailed: 'Failed to send verification code'
    },
    passkey: {
      title: 'Passkeys',
      description: 'Use Face ID, Touch ID, Windows Hello, or a security key to sign in without a password.',
      add: 'Add passkey',
      continue: 'Create passkey',
      name: 'Passkey name',
      namePlaceholder: 'For example, MacBook Touch ID',
      passwordPlaceholder: 'Enter your current password to confirm',
      empty: 'No passkeys are registered yet.',
      synced: 'Synced',
      createdAt: 'Created {date}',
      lastUsed: 'Last used {date}',
      featureDisabled: 'Passkeys have not been configured by the administrator.',
      unsupported: 'This browser or device does not support passkeys.',
      loadFailed: 'Failed to load passkeys.',
      added: 'Passkey added.',
      addFailed: 'Failed to add passkey.',
      renamePrompt: 'Enter a new name for this passkey',
      renamed: 'Passkey renamed.',
      renameFailed: 'Failed to rename passkey.',
      deleteTitle: 'Delete passkey',
      deleteConfirm: 'Delete “{name}”? You will no longer be able to sign in with it.',
      deleted: 'Passkey deleted.',
      deleteFailed: 'Failed to delete passkey.'
    },
    balanceNotify: {
      title: 'Balance Low Notification',
      description: 'Send email alert when account balance falls below threshold',
      enabled: 'Enable Balance Low Notification',
      threshold: 'Custom Threshold',
      thresholdHint: 'Leave empty to use system default',
      thresholdPlaceholder: 'Enter amount',
      systemDefault: 'System Default',
      extraEmails: 'Notification Emails',
      extraEmailsHint: 'You must add and verify an email address to receive low balance alerts',
      primaryEmail: 'Primary',
      noExtraEmails: 'No extra notification emails',
      enterEmail: 'Enter email address',
      addEmail: 'Add Email',
      emailPlaceholder: 'Enter email address',
      sendCode: 'Send Code',
      resend: 'Resend',
      codeSent: 'Verification code sent',
      codeSentTo: 'Code sent to {email}',
      enterCode: 'Enter verification code',
      codePlaceholder: '6-digit code',
      verify: 'Verify',
      emailAdded: 'Email added',
      emailRemoved: 'Email removed',
      verifySuccess: 'Email added successfully',
      removeEmail: 'Remove',
      removeSuccess: 'Email removed',
      emailDuplicate: 'This email already exists',
      maxEmailsReached: 'Maximum number of notification emails reached',
      unverified: 'Unverified',
      verified: 'Verified',
    },
    avatar: {
      title: 'Profile Avatar',
      description: 'Upload an avatar image. Static uploads are compressed to 20KB before saving.',
      uploadAction: 'Upload image',
      uploadHint: 'Static uploads are compressed to 20KB when possible. GIF uploads must already be within 20KB.',
      uploadRequired: 'Upload an avatar image first',
      saveSuccess: 'Avatar updated',
      deleteSuccess: 'Avatar removed',
      invalidType: 'Please choose an image file',
      gifTooLarge: 'GIF avatars must already be 20KB or smaller',
      compressTooLarge: 'Unable to compress this image below 20KB. Try a smaller image.',
      compressFailed: 'Failed to compress the selected image.',
      readFailed: 'Failed to read the selected image.',
      emptyDeleteHint: 'Avatar is already empty',
    },
    authBindings: {
      title: 'Connected Sign-In Methods',
      description: 'View current bindings and connect another provider to this account.',
      bindAction: 'Bind {providerName}',
      bindSuccess: 'Account linked successfully',
      emailPlaceholder: 'Enter email address',
      codePlaceholder: 'Enter verification code',
      passwordPlaceholder: 'Set a login password',
      replaceEmailPasswordPlaceholder: 'Enter current password',
      sendCodeAction: 'Send code',
      manageEmailAction: 'Manage email',
      hideEmailFormAction: 'Hide email form',
      confirmEmailBindAction: 'Bind email',
      confirmEmailReplaceAction: 'Replace primary email',
      codeSentTo: 'Code sent to {email}',
      replaceSuccess: 'Primary email updated',
      unbindAction: 'Unbind',
      unbindSuccess: '{providerName} unbound',
      boundCount: '{count} linked records',
      status: {
        bound: 'Bound',
        notBound: 'Not bound',
      },
      providers: {
        email: 'Email',
        linuxdo: 'LinuxDo',
        dingtalk: 'DingTalk',
        oidc: '{providerName}',
        wechat: 'WeChat',
      },
      notes: {
        emailManagedFromProfile: 'Primary email is managed in the profile form',
        canUnbind: 'You can unbind this sign-in method',
        bindAnotherBeforeUnbind: 'Bind another sign-in method before unbinding',
      },
      source: {
        avatar: 'Avatar is currently synced from {providerName}',
        username: 'Nickname is currently synced from {providerName}',
      },
    }
  },

  // Empty States
  empty: {
    noData: 'No data found'
  },

  // Table
  table: {
    expandActions: 'Expand More Actions',
    collapseActions: 'Collapse Actions'
  },

  // Pagination
  pagination: {
    showing: 'Showing',
    to: 'to',
    of: 'of',
    results: 'results',
    page: 'Page',
    pageOf: 'Page {page} of {total}',
    previous: 'Previous',
    next: 'Next',
    perPage: 'Per page',
    goToPage: 'Go to page {page}',
    jumpTo: 'Jump to',
    jumpPlaceholder: 'Page',
    jumpAction: 'Go'
  },

  // Errors
  errors: {
    somethingWentWrong: 'Something went wrong',
    pageNotFound: 'Page not found',
    unauthorized: 'Unauthorized',
    forbidden: 'Forbidden',
    serverError: 'Server error',
    networkError: 'Network error',
    timeout: 'Request timeout',
    tryAgain: 'Please try again'
  },

  // Dates
  dates: {
    today: 'Today',
    yesterday: 'Yesterday',
    thisWeek: 'This Week',
    lastWeek: 'Last Week',
    thisMonth: 'This Month',
    lastMonth: 'Last Month',
    last24Hours: 'Last 24 Hours',
    last7Days: 'Last 7 Days',
    last14Days: 'Last 14 Days',
    last30Days: 'Last 30 Days',
    custom: 'Custom',
    startDate: 'Start Date',
    endDate: 'End Date',
    apply: 'Apply',
    selectDateRange: 'Select date range'
  },

  // Admin
}
