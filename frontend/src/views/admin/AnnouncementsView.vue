<template>
  <AppLayout>
    <TablePageLayout>
      <template #filters>
        <div class="flex flex-wrap items-center gap-3">
          <!-- Left: Search + Filters -->
          <div class="flex-1 sm:max-w-64">
            <input
              v-model="searchQuery"
              type="text"
              :placeholder="t('admin.announcements.searchAnnouncements')"
              class="input"
              @input="handleSearch"
            />
          </div>
          <Select
            v-model="filters.status"
            :options="statusFilterOptions"
            class="w-40"
            @change="handleStatusChange"
          />

          <!-- Right: Action buttons -->
          <div class="flex flex-1 flex-wrap items-center justify-end gap-2">
            <button
              @click="loadAnnouncements"
              :disabled="loading"
              class="btn btn-secondary"
              :title="t('common.refresh')"
            >
              <Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />
            </button>
            <button @click="openCreateDialog" class="btn btn-primary">
              <Icon name="plus" size="md" class="mr-1" />
              {{ t('admin.announcements.createAnnouncement') }}
            </button>
          </div>
        </div>
      </template>

      <template #table>
        <DataTable
          :columns="columns"
          :data="announcements"
          :loading="loading"
          :server-side-sort="true"
          default-sort-key="created_at"
          default-sort-order="desc"
          @sort="handleSort"
        >
          <template #cell-title="{ value, row }">
            <div class="min-w-0">
              <div class="flex items-center gap-2">
                <span class="truncate font-medium text-gray-900 dark:text-white">{{ value }}</span>
              </div>
              <div class="mt-1 flex items-center gap-2 text-xs text-gray-500 dark:text-dark-400">
                <span>#{{ row.id }}</span>
                <span class="text-gray-300 dark:text-dark-700">·</span>
                <span>{{ formatDateTime(row.created_at) }}</span>
              </div>
            </div>
          </template>

          <template #cell-status="{ value }">
            <span
              :class="[
                'badge',
                value === 'active'
                  ? 'badge-success'
                  : value === 'draft'
                    ? 'badge-gray'
                    : 'badge-warning'
              ]"
            >
              {{ statusLabel(value) }}
            </span>
          </template>

          <template #cell-notify_mode="{ row }">
            <span
              :class="[
                'badge',
                row.notify_mode === 'popup'
                  ? 'badge-warning'
                  : 'badge-gray'
              ]"
            >
              {{ row.notify_mode === 'popup' ? t('admin.announcements.notifyModeLabels.popup') : t('admin.announcements.notifyModeLabels.silent') }}
            </span>
          </template>

          <template #cell-targeting="{ row }">
            <span class="text-sm text-gray-600 dark:text-gray-300">
              {{ targetingSummary(row.targeting) }}
            </span>
          </template>

          <template #cell-timeRange="{ row }">
            <div class="text-sm text-gray-600 dark:text-gray-300">
              <div>
                <span class="font-medium">{{ t('admin.announcements.form.startsAt') }}:</span>
                <span class="ml-1">{{ row.starts_at ? formatDateTime(row.starts_at) : t('admin.announcements.timeImmediate') }}</span>
              </div>
              <div class="mt-0.5">
                <span class="font-medium">{{ t('admin.announcements.form.endsAt') }}:</span>
                <span class="ml-1">{{ row.ends_at ? formatDateTime(row.ends_at) : t('admin.announcements.timeNever') }}</span>
              </div>
            </div>
          </template>

          <template #cell-created_at="{ value }">
            <span class="text-sm text-gray-500 dark:text-dark-400">{{ formatDateTime(value) }}</span>
          </template>

          <template #cell-actions="{ row }">
            <div class="flex items-center space-x-1">
              <button
                @click="openPreview(row)"
                class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-blue-50 hover:text-blue-600 dark:hover:bg-blue-900/20 dark:hover:text-blue-400"
                :title="t('admin.announcements.preview')"
              >
                <Icon name="eye" size="sm" />
              </button>
              <button
                @click="openReadStatus(row)"
                class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-blue-50 hover:text-blue-600 dark:hover:bg-blue-900/20 dark:hover:text-blue-400"
                :title="t('admin.announcements.readStatus')"
              >
                <Icon name="chartBar" size="sm" />
              </button>
              <button
                @click="openEditDialog(row)"
                class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-gray-100 hover:text-gray-700 dark:hover:bg-dark-600 dark:hover:text-gray-300"
                :title="t('common.edit')"
              >
                <Icon name="edit" size="sm" />
              </button>
              <button
                @click="handleDelete(row)"
                class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20 dark:hover:text-red-400"
                :title="t('common.delete')"
              >
                <Icon name="trash" size="sm" />
              </button>
            </div>
          </template>

          <template #empty>
            <EmptyState
              :title="t('empty.noData')"
              :description="t('admin.announcements.createFirstAnnouncement')"
              :action-text="t('admin.announcements.createAnnouncement')"
              @action="openCreateDialog"
            />
          </template>
        </DataTable>
      </template>

      <template #pagination>
        <Pagination
          v-if="pagination.total > 0"
          :page="pagination.page"
          :total="pagination.total"
          :page-size="pagination.page_size"
          @update:page="handlePageChange"
          @update:pageSize="handlePageSizeChange"
        />
      </template>
    </TablePageLayout>

    <!-- Create/Edit Dialog -->
    <BaseDialog
      :show="showEditDialog"
      :title="isEditing ? t('admin.announcements.editAnnouncement') : t('admin.announcements.createAnnouncement')"
      width="wide"
      @close="closeEdit"
    >
      <form id="announcement-form" @submit.prevent="handleSave" class="space-y-4">
        <div>
          <label class="input-label">{{ t('admin.announcements.form.title') }}</label>
          <input v-model="form.title" type="text" class="input" required />
        </div>

        <div>
          <div class="mb-1 flex items-center justify-between">
            <label class="input-label">{{ t('admin.announcements.form.content') }}</label>
            <div class="flex items-center gap-2">
              <div class="flex items-center gap-0.5 rounded-lg bg-gray-100 p-0.5 dark:bg-dark-700">
                <button
                  type="button"
                  data-testid="announcement-editor-tab-edit"
                  class="rounded-md px-2 py-0.5 text-xs font-medium transition-colors"
                  :class="editorTab === 'edit'
                    ? 'bg-white text-gray-900 shadow-sm dark:bg-dark-600 dark:text-white'
                    : 'text-gray-500 hover:text-gray-700 dark:text-dark-400 dark:hover:text-gray-300'"
                  @click="editorTab = 'edit'"
                >{{ t('common.edit') }}</button>
                <button
                  type="button"
                  data-testid="announcement-editor-tab-preview"
                  class="rounded-md px-2 py-0.5 text-xs font-medium transition-colors"
                  :class="editorTab === 'preview'
                    ? 'bg-white text-gray-900 shadow-sm dark:bg-dark-600 dark:text-white'
                    : 'text-gray-500 hover:text-gray-700 dark:text-dark-400 dark:hover:text-gray-300'"
                  @click="editorTab = 'preview'"
                >{{ t('admin.announcements.preview') }}</button>
              </div>
              <span
                v-if="editorTab === 'edit'"
                data-testid="announcement-image-paste-hint"
                class="text-xs text-gray-400 dark:text-dark-400"
              >{{ t('admin.announcements.pasteHint') }}</span>
            </div>
          </div>
          <template v-if="editorTab === 'edit'">
            <textarea
              ref="contentTextarea"
              v-model="form.content"
              rows="6"
              class="input"
              required
              @paste="handleTextareaPaste"
              @drop="handleTextareaDrop"
              @dragover.prevent
            ></textarea>
            <div class="mt-1 flex items-center gap-2">
            <button
              type="button"
              data-testid="announcement-insert-image-btn"
              class="btn btn-secondary"
              :disabled="uploading"
              @click="openImagePicker"
            >
              <Icon name="upload" size="sm" class="mr-1" />
              {{ t('admin.announcements.insertImage') }}
            </button>
            <span
              v-if="uploading"
              data-testid="announcement-upload-loading"
              class="flex items-center gap-1 text-xs text-blue-600 dark:text-blue-400"
            >
              <Icon name="refresh" size="sm" class="animate-spin" />
              {{ t('admin.announcements.uploading') }}
            </span>
            <span
              v-else-if="uploadError"
              data-testid="announcement-upload-error"
              class="text-xs text-red-600 dark:text-red-400"
            >{{ uploadError }}</span>
            </div>
          </template>
          <div
            v-else
            data-testid="announcement-content-preview"
            class="max-h-80 min-h-[8.75rem] overflow-auto rounded-xl border border-gray-200 bg-white p-3 dark:border-dark-600 dark:bg-dark-800"
          >
            <MarkdownContent :content="form.content" />
          </div>
          <input
            ref="imageFileInput"
            type="file"
            accept="image/*"
            class="hidden"
            @change="handleFileInputChange"
          />
        </div>

        <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
          <div>
            <label class="input-label">{{ t('admin.announcements.form.status') }}</label>
            <Select v-model="form.status" :options="statusOptions" />
          </div>
          <div>
            <label class="input-label">{{ t('admin.announcements.form.notifyMode') }}</label>
            <Select v-model="form.notify_mode" :options="notifyModeOptions" />
            <p class="input-hint">{{ t('admin.announcements.form.notifyModeHint') }}</p>
          </div>
        </div>

        <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
          <div>
            <label class="input-label">{{ t('admin.announcements.form.startsAt') }}</label>
            <input v-model="form.starts_at_str" type="datetime-local" max="9999-12-31T23:59" class="input" />
            <p class="input-hint">{{ t('admin.announcements.form.startsAtHint') }}</p>
          </div>
          <div>
            <label class="input-label">{{ t('admin.announcements.form.endsAt') }}</label>
            <input v-model="form.ends_at_str" type="datetime-local" max="9999-12-31T23:59" class="input" />
            <p class="input-hint">{{ t('admin.announcements.form.endsAtHint') }}</p>
          </div>
        </div>

        <AnnouncementTargetingEditor
          v-model="form.targeting"
          :groups="subscriptionGroups"
        />
      </form>

      <template #footer>
        <div class="flex justify-end gap-3">
          <button type="button" :disabled="uploading" @click="closeEdit" class="btn btn-secondary">
            {{ t('common.cancel') }}
          </button>
          <button type="submit" form="announcement-form" :disabled="saving || uploading" class="btn btn-primary">
            {{ saving ? t('common.saving') : t('common.save') }}
          </button>
        </div>
      </template>
    </BaseDialog>

    <!-- Delete Confirmation -->
    <ConfirmDialog
      :show="showDeleteDialog"
      :title="t('admin.announcements.deleteAnnouncement')"
      :message="t('admin.announcements.deleteConfirm')"
      :confirm-text="t('common.delete')"
      :cancel-text="t('common.cancel')"
      danger
      @confirm="confirmDelete"
      @cancel="showDeleteDialog = false"
    />

    <!-- Read Status Dialog -->
    <AnnouncementReadStatusDialog
      :show="showReadStatusDialog"
      :announcement-id="readStatusAnnouncementId"
      @close="showReadStatusDialog = false"
    />

    <AnnouncementPopup
      :announcement="previewAnnouncement"
      preview
      @close="previewAnnouncement = null"
    />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { getPersistedPageSize } from '@/composables/usePersistedPageSize'
import { adminAPI } from '@/api/admin'
import { formatDateTime, formatDateTimeLocalInput, parseDateTimeLocalInput } from '@/utils/format'
import type { AdminGroup, Announcement, AnnouncementTargeting } from '@/types'
import type { Column } from '@/components/common/types'

import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import Pagination from '@/components/common/Pagination.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Select from '@/components/common/Select.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import Icon from '@/components/icons/Icon.vue'

import AnnouncementTargetingEditor from '@/components/admin/announcements/AnnouncementTargetingEditor.vue'
import AnnouncementReadStatusDialog from '@/components/admin/announcements/AnnouncementReadStatusDialog.vue'
import AnnouncementPopup from '@/components/common/AnnouncementPopup.vue'
import MarkdownContent from '@/components/common/MarkdownContent.vue'

const { t } = useI18n()
const appStore = useAppStore()

const announcements = ref<Announcement[]>([])
const loading = ref(false)

const filters = reactive({
  status: '',
})
const searchQuery = ref('')

const pagination = reactive({
  page: 1,
  page_size: getPersistedPageSize(),
  total: 0,
  pages: 0
})

const sortState = reactive({
  sort_by: 'created_at',
  sort_order: 'desc' as 'asc' | 'desc'
})

const statusFilterOptions = computed(() => [
  { value: '', label: t('admin.announcements.allStatus') },
  { value: 'draft', label: t('admin.announcements.statusLabels.draft') },
  { value: 'active', label: t('admin.announcements.statusLabels.active') },
  { value: 'archived', label: t('admin.announcements.statusLabels.archived') }
])

const statusOptions = computed(() => [
  { value: 'draft', label: t('admin.announcements.statusLabels.draft') },
  { value: 'active', label: t('admin.announcements.statusLabels.active') },
  { value: 'archived', label: t('admin.announcements.statusLabels.archived') }
])

const notifyModeOptions = computed(() => [
  { value: 'silent', label: t('admin.announcements.notifyModeLabels.silent') },
  { value: 'popup', label: t('admin.announcements.notifyModeLabels.popup') }
])

const columns = computed<Column[]>(() => [
  { key: 'title', label: t('admin.announcements.columns.title'), sortable: true },
  { key: 'status', label: t('admin.announcements.columns.status'), sortable: true },
  { key: 'notify_mode', label: t('admin.announcements.columns.notifyMode'), sortable: true },
  { key: 'targeting', label: t('admin.announcements.columns.targeting') },
  { key: 'timeRange', label: t('admin.announcements.columns.timeRange') },
  { key: 'created_at', label: t('admin.announcements.columns.createdAt'), sortable: true },
  { key: 'actions', label: t('admin.announcements.columns.actions') }
])

const statusLabel = (status: string) => {
  if (status === 'draft') return t('admin.announcements.statusLabels.draft')
  if (status === 'active') return t('admin.announcements.statusLabels.active')
  if (status === 'archived') return t('admin.announcements.statusLabels.archived')
  return status
}

const targetingSummary = (targeting: AnnouncementTargeting) => {
  const anyOf = targeting?.any_of ?? []
  if (!anyOf || anyOf.length === 0) return t('admin.announcements.targetingSummaryAll')
  return t('admin.announcements.targetingSummaryCustom', { groups: anyOf.length })
}

// ===== CRUD / list =====
let currentController: AbortController | null = null

async function loadAnnouncements() {
  currentController?.abort()
  const requestController = new AbortController()
  currentController = requestController
  const { signal } = requestController

  try {
    loading.value = true
    const res = await adminAPI.announcements.list(pagination.page, pagination.page_size, {
      status: filters.status || undefined,
      search: searchQuery.value || undefined,
      sort_by: sortState.sort_by,
      sort_order: sortState.sort_order
    }, { signal })

    if (signal.aborted || currentController !== requestController) return

    announcements.value = res.items
    pagination.total = res.total
    pagination.pages = res.pages
    pagination.page = res.page
    pagination.page_size = res.page_size
  } catch (error: any) {
    if (
      signal.aborted ||
      currentController !== requestController ||
      error?.name === 'AbortError' ||
      error?.code === 'ERR_CANCELED'
    ) {
      return
    }
    console.error('Error loading announcements:', error)
    appStore.showError(error.response?.data?.detail || t('admin.announcements.failedToLoad'))
  } finally {
    if (currentController === requestController) {
      loading.value = false
      currentController = null
    }
  }
}

function handlePageChange(page: number) {
  pagination.page = page
  loadAnnouncements()
}

function handlePageSizeChange(pageSize: number) {
  pagination.page_size = pageSize
  pagination.page = 1
  loadAnnouncements()
}

function handleStatusChange() {
  pagination.page = 1
  loadAnnouncements()
}

function handleSort(key: string, order: 'asc' | 'desc') {
  sortState.sort_by = key
  sortState.sort_order = order
  pagination.page = 1
  loadAnnouncements()
}

let searchDebounceTimer: number | null = null
function handleSearch() {
  if (searchDebounceTimer) window.clearTimeout(searchDebounceTimer)
  searchDebounceTimer = window.setTimeout(() => {
    pagination.page = 1
    loadAnnouncements()
  }, 300)
}

// ===== Create/Edit dialog =====
const showEditDialog = ref(false)
const saving = ref(false)
const editingAnnouncement = ref<Announcement | null>(null)
// 内容区 编辑|预览 页签：每次打开弹窗重置回编辑态
const editorTab = ref<'edit' | 'preview'>('edit')

const isEditing = computed(() => !!editingAnnouncement.value)

const form = reactive({
  title: '',
  content: '',
  status: 'draft',
  notify_mode: 'silent',
  starts_at_str: '',
  ends_at_str: '',
  targeting: { any_of: [] } as AnnouncementTargeting
})

const subscriptionGroups = ref<AdminGroup[]>([])

async function loadSubscriptionGroups() {
  try {
    const all = await adminAPI.groups.getAll()
    subscriptionGroups.value = (all || []).filter((g) => g.subscription_type === 'subscription')
  } catch (error: any) {
    console.error('Error loading groups:', error)
    // not fatal
  }
}

function resetForm() {
  form.title = ''
  form.content = ''
  form.status = 'draft'
  form.notify_mode = 'silent'
  form.starts_at_str = ''
  form.ends_at_str = ''
  form.targeting = { any_of: [] }
}

function fillFormFromAnnouncement(a: Announcement) {
  form.title = a.title
  form.content = a.content
  form.status = a.status
  form.notify_mode = a.notify_mode || 'silent'

  // Backend returns RFC3339 strings
  form.starts_at_str = a.starts_at ? formatDateTimeLocalInput(Math.floor(new Date(a.starts_at).getTime() / 1000)) : ''
  form.ends_at_str = a.ends_at ? formatDateTimeLocalInput(Math.floor(new Date(a.ends_at).getTime() / 1000)) : ''

  form.targeting = a.targeting ?? { any_of: [] }
}

function openCreateDialog() {
  editingAnnouncement.value = null
  resetForm()
  resetDraftState()
  editorTab.value = 'edit'
  showEditDialog.value = true
}

function openEditDialog(row: Announcement) {
  editingAnnouncement.value = row
  fillFormFromAnnouncement(row)
  resetDraftState()
  editorTab.value = 'edit'
  showEditDialog.value = true
}

// ===== Image upload (paste/drop + placeholder draft state machine) =====
// R2 串行化：单一 uploading 互斥 + pendingFiles 串行队列；上传期间 handleSave/closeEdit no-op。
const contentTextarea = ref<HTMLTextAreaElement | null>(null)
const imageFileInput = ref<HTMLInputElement | null>(null)
const uploading = ref(false)
const uploadError = ref('')
const pendingFiles = ref<File[]>([])
const draftId = ref<number | null>(null)
const draftCreatedByThisSession = ref(false)
// 占位草稿响应：保存时作为 update-diff 的 original（编辑基线）
const draftBaseline = ref<Announcement | null>(null)
// 草稿标题是否为自动生成的占位文案（用户贴图时标题为空）——取消清理的判定条件
const draftTitleIsPlaceholder = ref(false)
const draftPlaceholderTitle = ref('')

function resetDraftState() {
  draftId.value = null
  draftCreatedByThisSession.value = false
  draftBaseline.value = null
  draftTitleIsPlaceholder.value = false
  draftPlaceholderTitle.value = ''
  pendingFiles.value = []
  uploadError.value = ''
}

function isImageFile(file: File | null | undefined): file is File {
  return !!file && file.type.startsWith('image/')
}

function handleTextareaPaste(event: ClipboardEvent) {
  const items = event.clipboardData?.items
  if (!items) return
  for (let i = 0; i < items.length; i++) {
    const item = items[i]
    if (item.kind === 'file' && item.type.startsWith('image/')) {
      const file = item.getAsFile()
      if (isImageFile(file)) {
        // 仅当确有图片才拦截，不影响纯文本粘贴
        event.preventDefault()
        enqueueImageUpload(file)
        return
      }
    }
  }
}

function handleTextareaDrop(event: DragEvent) {
  const files = event.dataTransfer?.files
  if (!files) return
  const imageFile = Array.from(files).find((f) => isImageFile(f))
  if (imageFile) {
    event.preventDefault()
    enqueueImageUpload(imageFile)
  }
}

function openImagePicker() {
  uploadError.value = ''
  imageFileInput.value?.click()
}

function handleFileInputChange(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (isImageFile(file)) enqueueImageUpload(file)
}

function enqueueImageUpload(file: File) {
  uploadError.value = ''
  pendingFiles.value.push(file)
  if (uploading.value) return // 入队，由进行中的队列依次消费
  void processUploadQueue()
}

async function processUploadQueue() {
  if (uploading.value) return
  uploading.value = true
  try {
    while (pendingFiles.value.length > 0) {
      const file = pendingFiles.value[0]
      try {
        const url = await uploadAnnouncementImage(file)
        insertImageAtCursor(`![announcement-image](${url})`)
        appStore.showSuccess(t('admin.announcements.uploadSuccess'))
      } catch (error: any) {
        console.error('Failed to upload announcement image:', error)
        const detail =
          error?.response?.data?.detail || error?.message || t('admin.announcements.uploadFailed')
        uploadError.value = detail
        appStore.showError(detail)
      }
      pendingFiles.value.shift()
    }
  } finally {
    uploading.value = false
  }
}

// 方案 §3.2(c) 唯一时序：新建态先建占位草稿（title/content 占位值必过后端非空校验），
// 编辑基线切换为该草稿响应；编辑态直接用现有公告 id。
async function resolveUploadTargetId(): Promise<number> {
  if (editingAnnouncement.value) return editingAnnouncement.value.id
  if (draftId.value != null) return draftId.value

  const titleIsPlaceholder = !form.title.trim()
  const placeholderTitle = titleIsPlaceholder
    ? `${t('admin.announcements.untitledDraft')} ${formatDraftTimestamp(new Date())}`
    : form.title.trim()
  const placeholderContent = form.content || t('admin.announcements.draftPlaceholder')

  const created = await adminAPI.announcements.create({
    title: placeholderTitle,
    content: placeholderContent,
    status: 'draft',
    notify_mode: form.notify_mode as any,
    targeting: form.targeting,
    starts_at: parseDateTimeLocalInput(form.starts_at_str) ?? undefined,
    ends_at: parseDateTimeLocalInput(form.ends_at_str) ?? undefined
  })

  draftId.value = created.id
  draftCreatedByThisSession.value = true
  draftBaseline.value = created
  draftTitleIsPlaceholder.value = titleIsPlaceholder
  draftPlaceholderTitle.value = placeholderTitle

  // 同步表单：占位值回填空字段，保证 required 校验通过且保存时 update-diff 一致
  if (titleIsPlaceholder) form.title = placeholderTitle
  if (!form.content) form.content = placeholderContent

  return created.id
}

function formatDraftTimestamp(date: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`
}

async function uploadAnnouncementImage(file: File): Promise<string> {
  const targetId = await resolveUploadTargetId()
  const { url } = await adminAPI.announcements.uploadImage(targetId, file)
  return url
}

function insertImageAtCursor(markdown: string) {
  const textarea = contentTextarea.value
  if (!textarea) {
    form.content = form.content ? `${form.content}\n${markdown}` : markdown
    return
  }
  const start = textarea.selectionStart ?? form.content.length
  const end = textarea.selectionEnd ?? start
  const before = form.content.slice(0, start)
  const after = form.content.slice(end)
  const needsSpaceBefore = before.length > 0 && !before.endsWith('\n') && !before.endsWith(' ')
  const needsSpaceAfter = after.length > 0 && !after.startsWith('\n') && !after.startsWith(' ')
  form.content = `${before}${needsSpaceBefore ? ' ' : ''}${markdown}${needsSpaceAfter ? ' ' : ''}${after}`
}

// 取消清理（方案 §3.2(c)）：仅当草稿由本会话自动创建且标题仍为占位文案时删除草稿（级联删图）。
async function cleanupPlaceholderDraft() {
  const shouldCleanup =
    draftCreatedByThisSession.value &&
    draftId.value != null &&
    draftTitleIsPlaceholder.value &&
    form.title === draftPlaceholderTitle.value
  const draftIdToClean = draftId.value
  resetDraftState()
  if (!shouldCleanup || draftIdToClean == null) return
  try {
    await adminAPI.announcements.delete(draftIdToClean)
  } catch (error) {
    // 清理失败静默记 console，不阻塞关闭
    console.error('Failed to clean up placeholder draft:', error)
  }
}

function closeEdit() {
  if (uploading.value) return // R2: 上传期间取消 no-op
  void cleanupPlaceholderDraft().finally(() => {
    showEditDialog.value = false
    editingAnnouncement.value = null
  })
}

function buildCreatePayload() {
  const startsAt = parseDateTimeLocalInput(form.starts_at_str)
  const endsAt = parseDateTimeLocalInput(form.ends_at_str)

  return {
    title: form.title,
    content: form.content,
    status: form.status as any,
    notify_mode: form.notify_mode as any,
    targeting: form.targeting,
    starts_at: startsAt ?? undefined,
    ends_at: endsAt ?? undefined
  }
}

function buildUpdatePayload(original: Announcement) {
  const payload: any = {}

  if (form.title !== original.title) payload.title = form.title
  if (form.content !== original.content) payload.content = form.content
  if (form.status !== original.status) payload.status = form.status
  if (form.notify_mode !== (original.notify_mode || 'silent')) payload.notify_mode = form.notify_mode

  // starts_at / ends_at: distinguish unchanged vs clear(0) vs set
  const originalStarts = original.starts_at ? Math.floor(new Date(original.starts_at).getTime() / 1000) : null
  const originalEnds = original.ends_at ? Math.floor(new Date(original.ends_at).getTime() / 1000) : null

  const newStarts = parseDateTimeLocalInput(form.starts_at_str)
  const newEnds = parseDateTimeLocalInput(form.ends_at_str)

  if (newStarts !== originalStarts) {
    payload.starts_at = newStarts === null ? 0 : newStarts
  }
  if (newEnds !== originalEnds) {
    payload.ends_at = newEnds === null ? 0 : newEnds
  }

  // targeting: do shallow compare by JSON
  if (JSON.stringify(form.targeting ?? {}) !== JSON.stringify(original.targeting ?? {})) {
    payload.targeting = form.targeting
  }

  return payload
}

async function handleSave() {
  // R2: 上传期间保存 no-op（保证建草稿→传图→插入完成后才允许状态转换）
  if (uploading.value) return

  // Frontend validation for targeting (to avoid ANNOUNCEMENT_INVALID_TARGET)
  const anyOf = form.targeting?.any_of ?? []
  if (anyOf.length > 50) {
    appStore.showError(t('admin.announcements.failedToCreate'))
    return
  }
  for (const g of anyOf) {
    const allOf = g?.all_of ?? []
    if (allOf.length > 50) {
      appStore.showError(t('admin.announcements.failedToCreate'))
      return
    }
  }

  saving.value = true
  try {
    if (!editingAnnouncement.value) {
      if (draftId.value != null && draftBaseline.value) {
        // 本会话贴图已建占位草稿 → 走 update-diff 保存（不再 POST create）
        const payload = buildUpdatePayload(draftBaseline.value)
        await adminAPI.announcements.update(draftId.value, payload)
      } else {
        const payload = buildCreatePayload()
        await adminAPI.announcements.create(payload)
      }
      appStore.showSuccess(t('common.success'))
      resetDraftState()
      showEditDialog.value = false
      await loadAnnouncements()
      return
    }

    const original = editingAnnouncement.value
    const payload = buildUpdatePayload(original)
    await adminAPI.announcements.update(original.id, payload)
    appStore.showSuccess(t('common.success'))
    resetDraftState()
    showEditDialog.value = false
    editingAnnouncement.value = null
    await loadAnnouncements()
  } catch (error: any) {
    console.error('Failed to save announcement:', error)
    appStore.showError(error.response?.data?.detail || (editingAnnouncement.value ? t('admin.announcements.failedToUpdate') : t('admin.announcements.failedToCreate')))
  } finally {
    saving.value = false
  }
}

// ===== Delete =====
const showDeleteDialog = ref(false)
const deletingAnnouncement = ref<Announcement | null>(null)

function handleDelete(row: Announcement) {
  deletingAnnouncement.value = row
  showDeleteDialog.value = true
}

async function confirmDelete() {
  if (!deletingAnnouncement.value) return

  try {
    await adminAPI.announcements.delete(deletingAnnouncement.value.id)
    appStore.showSuccess(t('common.success'))
    showDeleteDialog.value = false
    deletingAnnouncement.value = null
    await loadAnnouncements()
  } catch (error: any) {
    console.error('Failed to delete announcement:', error)
    appStore.showError(error.response?.data?.detail || t('admin.announcements.failedToDelete'))
  }
}

// ===== Read status =====
const showReadStatusDialog = ref(false)
const readStatusAnnouncementId = ref<number | null>(null)
const previewAnnouncement = ref<Announcement | null>(null)

function openPreview(row: Announcement) {
  previewAnnouncement.value = row
}

function openReadStatus(row: Announcement) {
  readStatusAnnouncementId.value = row.id
  showReadStatusDialog.value = true
}

onMounted(async () => {
  await loadSubscriptionGroups()
  await loadAnnouncements()
})

onUnmounted(() => {
  if (searchDebounceTimer) window.clearTimeout(searchDebounceTimer)
  currentController?.abort()
})
</script>
