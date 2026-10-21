export interface ModelAllowlistConfig {
  enabled: boolean
  models: string[]
}

// 条目来源：实供候选（本次成功响应）、已保存规则（既有白名单存储）、手工添加。
// 仅用于页面区分展示，不进入保存载荷；保存载荷始终只含 id + selected。
export type ModelAllowlistItemSource = 'candidate' | 'saved' | 'custom'

export interface ModelAllowlistItem {
  id: string
  selected: boolean
  source: ModelAllowlistItemSource
}

export interface ModelAllowlistState {
  enabled: boolean
  savedModels: string[]
  items: ModelAllowlistItem[]
  // 最近一次成功响应得到的实供候选（账号真实供给）。失败/刷新前用于失效判定。
  candidates: string[]
}

// 自定义条目校验错误码，由视图映射为 i18n 提示。
export type ModelAllowlistAddError = 'empty' | 'duplicate'

export const createModelAllowlistState = (
  config?: Partial<ModelAllowlistConfig> | null,
): ModelAllowlistState => ({
  enabled: config?.enabled ?? false,
  savedModels: normalizeModels(config?.models ?? []),
  items: [],
  candidates: [],
})

export const hydrateModelAllowlistState = (
  config: Partial<ModelAllowlistConfig> | null | undefined,
  candidates: string[],
): ModelAllowlistState => {
  const state = createModelAllowlistState(config)
  setModelAllowlistCandidates(state, candidates)
  return state
}

// setModelAllowlistCandidates 把"实供候选"与"已保存/手工规则"在本地状态中明确区分：
// - 已保存规则保留其规则身份（source=saved），顺序沿用存储顺序；
// - 手工添加条目保留其身份（source=custom），顺序不受刷新影响；
// - 实供候选只来自本次成功响应（source=candidate），刷新时旧的自动候选身份被移除，
//   已存在条目中 source=candidate 的项被本次响应整体替换，避免旧组/旧平台数据残留；
// - 用户对某候选的显式勾选/取消会被 prevSelected 记忆，刷新不会重选已取消的项。
export const setModelAllowlistCandidates = (
  state: ModelAllowlistState,
  candidates: string[],
) => {
  const newCandidates = normalizeModels(candidates)
  const prevSelected = new Map(state.items.map(item => [item.id, item.selected]))
  const seen = new Set<string>()
  // 已存在的手工添加条目身份优先：刷新时同名候选不得改标其身份、也不得被 seen 吞并，
  // 留待第 3 步以 custom 身份保留（发现 2 整改）。
  const customIds = new Set(
    state.items.filter(item => item.source === 'custom').map(item => item.id),
  )
  const result: ModelAllowlistItem[] = []

  // 1. 已保存规则优先，保住规则身份与既有勾选状态。
  for (const id of state.savedModels) {
    if (seen.has(id)) continue
    result.push({ id, selected: prevSelected.get(id) ?? true, source: 'saved' })
    seen.add(id)
  }

  // 2. 实供候选随后；若某 id 已是保存规则或手工添加条目则不再单列候选身份
  //    （同名手工添加条目身份不被改标，留待第 3 步保留）。
  //    全新候选的默认勾选：仅在无任何已保存规则时（首次加载）默认全选，
  //    其余情况（刷新）沿用 prevSelected，已取消的项不会被重选。
  for (const id of newCandidates) {
    if (seen.has(id) || customIds.has(id)) continue
    result.push({
      id,
      selected: prevSelected.get(id) ?? state.savedModels.length === 0,
      source: 'candidate',
    })
    seen.add(id)
  }

  // 3. 手工添加条目最后，保住其位置与勾选状态，不受候选刷新影响。
  for (const item of state.items) {
    if (item.source === 'custom' && !seen.has(item.id)) {
      result.push({ ...item })
      seen.add(item.id)
    }
  }

  state.candidates = newCandidates
  state.items = result
}

// invalidateModelAllowlistCandidates 在目录加载失败时调用：使旧自动候选失效，
// 仅保留已保存规则与手工添加条目，避免把旧组/旧平台的候选当作当前结果。
export const invalidateModelAllowlistCandidates = (
  state: ModelAllowlistState,
) => {
  state.candidates = []
  state.items = state.items.filter(item => item.source !== 'candidate')
}

export const toggleModelAllowlistItem = (
  state: ModelAllowlistState,
  modelID: string,
) => {
  const item = state.items.find(item => item.id === modelID)
  if (item) {
    item.selected = !item.selected
  }
}

export const selectAllModelAllowlistItems = (state: ModelAllowlistState) => {
  state.items.forEach(item => {
    item.selected = true
  })
}

export const invertModelAllowlistSelection = (state: ModelAllowlistState) => {
  state.items.forEach(item => {
    item.selected = !item.selected
  })
}

export const moveModelAllowlistItem = (
  state: ModelAllowlistState,
  fromIndex: number,
  toIndex: number,
) => {
  if (
    fromIndex === toIndex ||
    fromIndex < 0 ||
    toIndex < 0 ||
    fromIndex >= state.items.length ||
    toIndex >= state.items.length
  ) {
    return
  }
  const [item] = state.items.splice(fromIndex, 1)
  state.items.splice(toIndex, 0, item)
}

// addCustomModelAllowlistItem 把手工输入的条目追加到白名单末尾（选中状态，source=custom）。
// 去重；`*` 可出现在任意位置。返回错误码或 null（成功）。
export const addCustomModelAllowlistItem = (
  state: ModelAllowlistState,
  raw: string,
): ModelAllowlistAddError | null => {
  const entry = raw.trim()
  if (!entry) {
    return 'empty'
  }
  if (
    state.items.some(item => item.id.toLowerCase() === entry.toLowerCase()) ||
    state.savedModels.some(model => model.toLowerCase() === entry.toLowerCase())
  ) {
    return 'duplicate'
  }
  state.items.push({ id: entry, selected: true, source: 'custom' })
  return null
}

export const buildModelAllowlistConfig = (
  state: ModelAllowlistState,
): ModelAllowlistConfig => ({
  enabled: state.enabled,
  models: state.items.length > 0
    ? state.items.filter(item => item.selected).map(item => item.id)
    : [...state.savedModels],
})

export const selectedModelAllowlistCount = (state: ModelAllowlistState): number =>
  state.items.filter(item => item.selected).length

const normalizeModels = (models: string[]): string[] => {
  const seen = new Set<string>()
  const out: string[] = []
  for (const raw of models) {
    const model = raw.trim()
    if (!model || seen.has(model)) {
      continue
    }
    seen.add(model)
    out.push(model)
  }
  return out
}
