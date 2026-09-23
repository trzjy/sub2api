<script setup lang="ts">
import { ref, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'

const appStore = useAppStore()
const { t } = useI18n()

const open = ref(false)
const imageFailed = ref(false)
const rootRef = ref<HTMLElement | null>(null)

function toggleOpen(): void {
  open.value = !open.value
  imageFailed.value = false
}

function close(): void {
  open.value = false
}

function onDocumentClick(event: MouseEvent): void {
  if (!open.value) return
  const target = event.target as Node
  if (rootRef.value && !rootRef.value.contains(target)) {
    close()
  }
}

function onKeydown(event: KeyboardEvent): void {
  if (event.key === 'Escape') {
    close()
  }
}

onMounted(() => {
  document.addEventListener('click', onDocumentClick)
  document.addEventListener('keydown', onKeydown)
})

onUnmounted(() => {
  document.removeEventListener('click', onDocumentClick)
  document.removeEventListener('keydown', onKeydown)
})
</script>

<template>
  <div v-if="appStore.supportQrcodeUrl" ref="rootRef" class="fixed bottom-4 right-4 z-40">
    <!-- Collapsed: floating round button -->
    <button
      v-if="!open"
      type="button"
      class="flex h-12 w-12 items-center justify-center rounded-full bg-primary-600 text-white shadow-lg transition-colors hover:bg-primary-700 focus:outline-none focus:ring-2 focus:ring-primary-500 focus:ring-offset-2 dark:bg-primary-500 dark:hover:bg-primary-600 dark:focus:ring-offset-gray-900"
      :aria-label="t('supportWidget.title')"
      @click="toggleOpen"
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

    <!-- Expanded: card -->
    <div
      v-else
      class="w-64 rounded-xl border border-gray-200 bg-white p-4 shadow-xl dark:border-gray-700 dark:bg-gray-800"
    >
      <div class="mb-3 flex items-center justify-between">
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
        class="h-48 w-48 rounded border border-gray-200 object-contain dark:border-gray-700"
        @error="imageFailed = true"
      />
      <div
        v-else
        class="flex h-48 w-48 items-center justify-center rounded border border-gray-200 text-xs text-gray-400 dark:border-gray-700 dark:text-gray-500"
      >
        {{ t('supportWidget.scanHint') }}
      </div>

      <p class="mt-3 text-center text-xs text-gray-500 dark:text-gray-400">
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
