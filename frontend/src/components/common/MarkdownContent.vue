<template>
  <div
    ref="container"
    class="markdown-body prose prose-sm max-w-none dark:prose-invert"
    data-testid="markdown-content"
    v-html="renderedContent"
    @click="handleContentClick"
  ></div>

  <!-- 图片点击放大：公告内容里的图片按自适应尺寸显示（CSS 上限），点击后全屏查看 -->
  <Teleport to="body">
    <div
      v-if="zoomedSrc"
      data-testid="markdown-image-zoom"
      class="fixed inset-0 z-[100] flex items-center justify-center bg-black/80 p-4"
      @click="zoomedSrc = ''"
      @keydown.esc="zoomedSrc = ''"
    >
      <img
        :src="zoomedSrc"
        alt=""
        class="max-h-[92vh] max-w-[94vw] rounded-lg object-contain shadow-2xl"
      />
      <button
        type="button"
        data-testid="markdown-image-zoom-close"
        class="absolute right-4 top-4 rounded-full bg-white/10 p-2 text-white transition-colors hover:bg-white/20"
        :aria-label="t('common.close')"
        @click.stop="zoomedSrc = ''"
      >
        <svg class="h-5 w-5" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
          <path stroke-linecap="round" stroke-linejoin="round" d="M6 18L18 6M6 6l12 12" />
        </svg>
      </button>
    </div>
  </Teleport>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import '@/styles/announcement-markdown.css'

// 公告 Markdown 渲染的唯一入口：弹窗、铃铛详情、后台编辑预览共用同一管线
// （marked → DOMPurify → announcement-markdown.css），保证三处所见一致。
const props = defineProps<{ content: string }>()

const { t } = useI18n()

marked.setOptions({
  breaks: true,
  gfm: true,
})

const renderedContent = computed(() => {
  if (!props.content) return ''
  const html = marked.parse(props.content) as string
  return DOMPurify.sanitize(html)
})

const zoomedSrc = ref('')

// v-html 内容里的 img 无法绑定监听，用事件委托捕获点击
function handleContentClick(event: MouseEvent) {
  const target = event.target as HTMLElement
  if (target.tagName === 'IMG' && (target as HTMLImageElement).src) {
    event.preventDefault()
    zoomedSrc.value = (target as HTMLImageElement).src
  }
}
</script>
