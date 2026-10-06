<script setup lang="ts">
// 2026-09-24 用户裁定:二维码直接展示(默认展开),不搞点击展开才可见。
// X 收起为悬浮圆钮,点圆钮再展开;不做点外部/ESC 自动关闭(常驻卡片不应误关)。
// 2026-09-24 二次裁定:卡片缩小(192/二维码144);记住收起——点过 X 的设备后续
// 进页面只出圆钮(localStorage),点圆钮展开后恢复默认展示(双向记忆最后选择)。
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'

const appStore = useAppStore()
const { t } = useI18n()

const COLLAPSED_KEY = 'support_widget_collapsed'

function readCollapsed(): boolean {
  try {
    return localStorage.getItem(COLLAPSED_KEY) === '1'
  } catch {
    return false // 存储不可用(隐私模式等)时按未收起处理,回到默认直接展示
  }
}

const open = ref(!readCollapsed())
const imageFailed = ref(false)

function close(): void {
  open.value = false
  imageFailed.value = false
  try {
    localStorage.setItem(COLLAPSED_KEY, '1')
  } catch {
    // 存储不可用时仅本次生效,下次进页面仍默认展示
  }
}

function reopen(): void {
  open.value = true
  imageFailed.value = false
  try {
    localStorage.removeItem(COLLAPSED_KEY)
  } catch {
    // 同上,忽略存储异常
  }
}
</script>

<template>
  <div v-if="appStore.supportQrcodeUrl" class="fixed bottom-4 right-4 z-40">
    <!-- Collapsed: floating round button (only after user closes the card) -->
    <button
      v-if="!open"
      type="button"
      class="flex h-12 w-12 items-center justify-center rounded-full bg-primary-600 text-white shadow-lg transition-colors hover:bg-primary-700 focus:outline-none focus:ring-2 focus:ring-primary-500 focus:ring-offset-2 dark:bg-primary-500 dark:hover:bg-primary-600 dark:focus:ring-offset-gray-900"
      :aria-label="t('supportWidget.title')"
      @click.stop="reopen"
    >
      <svg
        xmlns="http://www.w3.org/2000/svg"
        class="h-6 w-6"
        fill="none"
        viewBox="0 0 24 24"
        stroke="currentColor"
        stroke-width="1.8"
      >
        <path
          stroke-linecap="round"
          stroke-linejoin="round"
          d="M8 10h.01M12 10h.01M16 10h.01M21 12c0 4.418-4.03 8-9 8a9.86 9.86 0 0 1-4.255-.949L3 20l1.395-3.72C3.512 15.042 3 13.574 3 12c0-4.418 4.03-8 9-8s9 3.582 9 8Z"
        />
      </svg>
    </button>

    <!-- Expanded: card with QR shown directly -->
    <div
      v-else
      class="w-48 rounded-xl border border-gray-200 bg-white p-3 shadow-xl dark:border-gray-700 dark:bg-gray-800"
    >
      <div class="mb-2 flex items-center justify-between">
        <h3 class="text-sm font-semibold text-gray-900 dark:text-gray-100">
          {{ t('supportWidget.title') }}
        </h3>
        <button
          type="button"
          class="flex h-6 w-6 items-center justify-center rounded-full text-gray-400 transition-colors hover:bg-gray-100 hover:text-gray-600 dark:hover:bg-gray-700 dark:hover:text-gray-300"
          :aria-label="t('supportWidget.close')"
          @click="close"
        >
          <svg
            xmlns="http://www.w3.org/2000/svg"
            class="h-4 w-4"
            fill="none"
            viewBox="0 0 24 24"
            stroke="currentColor"
            stroke-width="2"
          >
            <path stroke-linecap="round" stroke-linejoin="round" d="M6 18 18 6M6 6l12 12" />
          </svg>
        </button>
      </div>

      <img
        v-if="!imageFailed"
        :src="appStore.supportQrcodeUrl"
        :alt="t('supportWidget.title')"
        class="h-36 w-36 rounded border border-gray-200 object-contain dark:border-gray-700"
        @error="imageFailed = true"
      />
      <div
        v-else
        class="flex h-36 w-36 items-center justify-center rounded border border-gray-200 text-xs text-gray-400 dark:border-gray-700 dark:text-gray-500"
      >
        {{ t('supportWidget.scanHint') }}
      </div>

      <p class="mt-2 text-center text-xs text-gray-500 dark:text-gray-400">
        {{ t('supportWidget.scanHint') }}
      </p>

      <p
        v-if="appStore.contactInfo"
        class="mt-2 break-all border-t border-gray-100 pt-2 text-center text-xs text-gray-500 dark:border-gray-700 dark:text-gray-400"
      >
        {{ appStore.contactInfo }}
      </p>
    </div>
  </div>
</template>
