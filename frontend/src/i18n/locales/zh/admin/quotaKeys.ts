/**
 * 激活码管理页（Cursor quota keys）文案。
 *
 * 命名空间与 accounts.ts / promoIntel.ts 同型：顶层挂 `quotaKeys`，
 * 经 admin/index.ts 展开后键为 `admin.quotaKeys.*`。
 */
export default {
  quotaKeys: {
    title: '激活码管理',
    description: '管理 Cursor 站点渠道激活码：创建、编辑、删除、手动拉号、解除耗尽与一键清理过期。',
    create: '创建激活码',
    createTitle: '创建激活码',
    editTitle: '编辑激活码',
    detailTitle: '激活码详情',
    columns: {
      maskedCode: '掩码激活码',
      platform: '平台',
      status: '状态',
      exhausted: '已耗尽',
      accountCount: '账号数',
      workingCount: '可用账号数',
      expiresAt: '过期时间',
      lastRefreshError: '最近错误',
      actions: '操作'
    },
    code: '激活码原文',
    codePlaceholder: '请输入激活码原文（仅提交一次，提交后前端不再持有明文）',
    codeHint: '明文激活码只用于本次提交，不会写入本地存储、日志或地址栏。',
    expiresAt: '过期时间',
    expiresAtPlaceholder: '留空表示永不过期',
    notes: '备注',
    notesPlaceholder: '可选备注',
    never: '永不过期',
    na: '—',
    yes: '是',
    no: '否',
    status: {
      active: '正常',
      exhausted: '已耗尽',
      expired: '已过期',
      inactive: '已停用',
      unknown: '未知'
    },
    actions: {
      detail: '详情',
      edit: '编辑',
      refresh: '手动拉号',
      clearExhausted: '解除耗尽',
      delete: '删除'
    },
    sweepExpired: '一键清理过期',
    confirmDeleteTitle: '删除激活码',
    confirmSweepTitle: '清理过期激活码',
    deleteConfirm: '删除后其下账号将一并软删除，且无法恢复。确定继续？',
    sweepConfirm: '将清理全部已过期的激活码，此操作不可撤销。确定继续？',
    detail: {
      maskedCode: '掩码激活码',
      status: '状态',
      exhausted: '已耗尽',
      accountCount: '账号数',
      workingCount: '可用账号数',
      expiresAt: '过期时间',
      lastRefreshAt: '最近拉号时间',
      lastRefreshError: '最近错误',
      notes: '备注'
    },
    accountsTitle: '归属账号列表',
    noAccounts: '该激活码下暂无账号',
    expandAccounts: '展开账号列表',
    collapseAccounts: '收起账号列表',
    emptyTitle: '暂无激活码',
    emptyDescription: '创建第一个 Cursor 激活码后即可在此管理拉号与归属账号。',
    created: '激活码已创建',
    updated: '激活码已更新',
    deleted: '激活码已删除',
    refreshed: '已触发手动拉号',
    cleared: '已解除耗尽标记',
    swept: '已清理 {count} 个过期激活码',
    loadError: '加载激活码列表失败',
    createError: '创建激活码失败',
    updateError: '更新激活码失败',
    deleteError: '删除激活码失败',
    refreshError: '手动拉号失败',
    clearError: '解除耗尽失败',
    sweepError: '清理过期激活码失败',
    detailLoadError: '加载激活码详情失败'
  }
}
