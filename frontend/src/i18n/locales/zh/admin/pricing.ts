export default {
  pricing: {
    title: '价格管理',
    description: '价格表同步、全局价格目录、未覆盖模型检测与自定义价格层',
    mtok: '百万 token',
    actions: '操作',
    tabs: {
      status: '同步状态',
      catalog: '价格目录',
      uncovered: '未覆盖模型',
      custom: '自定义价格'
    },
    columns: {
      model: '模型',
      source: '来源',
      requests: '请求数'
    },
    source: {
      custom: '自定义',
      remote: '远程同步',
      builtin: '内置兜底',
      channel: '渠道价',
      group: '分组价'
    },
    status: {
      syncTitle: '远程价格表同步',
      syncNow: '立即同步',
      syncDone: '同步完成',
      modelCount: '模型总数',
      lastUpdated: '上次成功加载',
      lastAttempt: '上次同步尝试',
      noError: '无错误',
      scheduler: '定时同步',
      customLayer: '自定义价格层',
      customLayerHint: '启用数 / 总数',
      localHash: '本地哈希',
      gapsTitle: '线上计费缺口',
      gapsHint: '计费时找不到任何价格的模型（按 $0 记账，存在漏费）。进程内数据，重启后清零。',
      noGaps: '暂无缺口，所有计费模型均有价格'
    },
    gaps: {
      count: '次数',
      firstSeen: '首次出现',
      lastSeen: '最近出现',
      addPrice: '补价'
    },
    catalog: {
      search: '搜索模型',
      searchPlaceholder: '输入模型名关键词',
      source: '来源筛选',
      allSources: '全部来源',
      sourceHint: '来源分层：自定义 > 远程同步 > 内置兜底；价格目录只含全局层，分组/渠道覆盖请到对应页面查看。',
      imageOnly: '仅图片价',
      override: '自定义覆盖',
      empty: '没有匹配的模型'
    },
    preview: {
      title: '生效价试算',
      modelPlaceholder: '模型名',
      run: '试算',
      input: '输入',
      output: '输出',
      cacheWrite: '缓存写入',
      cacheRead: '缓存读取'
    },
    uncovered: {
      windowDays: '用量扫描窗口',
      days7: '近 7 天',
      days30: '近 30 天',
      days90: '近 90 天',
      hint: '多通道扫描：分组声明 + 渠道模型 + 实际计费用量；零调用模型需在分组"模型列表配置"或渠道中登记才会进入扫描。',
      verdict: '判定',
      verdictUncovered: '无价（按 $0 记账）',
      verdictFuzzy: '模糊计价（按近似价）',
      references: '引用来源',
      usage: '近窗用量',
      requests: '次',
      tokens: 'tokens',
      currentPrice: '当前计价 $入/$出 (每百万)',
      zeroCost: '有量无费',
      allCovered: '全部精确覆盖（扫描候选 {scanned} 个模型）'
    },
    custom: {
      hint: '自定义价格层全局生效，优先级介于渠道价与远程同步表之间；支持 * 后缀通配；远程同步不会覆盖本层配置。',
      add: '新增定价',
      addTitle: '新增自定义定价',
      editTitle: '编辑自定义定价',
      models: '生效模型',
      modelsPlaceholder: 'gpt-5.6-sol, my-alias-*',
      modelsHint: '逗号分隔，支持 * 后缀通配',
      mode: '计费模式',
      priceUnit: '价格单位：$/百万 token（保存时自动换算为每 token 存储）。留空表示沿用更低价层（合并语义）。',
      perRequest: '按次价格',
      remark: '备注',
      enabled: '启用',
      empty: '暂无自定义定价',
      deleteTitle: '删除自定义定价',
      deleteMessage: '确定删除「{models}」的自定义定价？删除后这些模型将回退到远程表/内置价。'
    }
  }
}
