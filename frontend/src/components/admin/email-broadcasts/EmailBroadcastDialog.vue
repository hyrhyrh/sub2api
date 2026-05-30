<template>
  <BaseDialog
    :show="show"
    :title="t('admin.emailBroadcast.title')"
    width="wide"
    @close="handleClose"
  >
    <div class="space-y-6">
      <!-- Compose form -->
      <div class="space-y-4">
        <div>
          <label class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.emailBroadcast.form.subject') }}
            <span class="text-red-500">*</span>
          </label>
          <input
            v-model="form.subject"
            type="text"
            class="input"
            :maxlength="SUBJECT_MAX_LEN"
            :placeholder="t('admin.emailBroadcast.form.subjectPlaceholder')"
          />
          <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
            {{ form.subject.length }} / {{ SUBJECT_MAX_LEN }}
          </p>
        </div>

        <div>
          <div class="mb-2 flex items-center justify-between">
            <label class="block text-sm font-medium text-gray-700 dark:text-gray-300">
              {{ t('admin.emailBroadcast.form.body') }}
              <span class="text-red-500">*</span>
            </label>
            <div class="flex items-center gap-3 text-sm">
              <span class="text-gray-500 dark:text-gray-400">
                {{ t('admin.emailBroadcast.form.bodyFormat') }}:
              </span>
              <label class="inline-flex items-center gap-1.5">
                <input v-model="form.body_format" type="radio" value="html" class="form-radio" />
                <span>HTML</span>
              </label>
              <label class="inline-flex items-center gap-1.5">
                <input v-model="form.body_format" type="radio" value="text" class="form-radio" />
                <span>{{ t('admin.emailBroadcast.form.bodyFormatText') }}</span>
              </label>
            </div>
          </div>
          <textarea
            v-model="form.body"
            rows="10"
            class="input font-mono text-sm"
            :maxlength="BODY_MAX_LEN"
            :placeholder="bodyPlaceholder"
          />
          <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
            {{ t('admin.emailBroadcast.form.bodyHint') }} ({{ form.body.length }} / {{ BODY_MAX_LEN }})
          </p>
        </div>

        <!-- Recipients -->
        <div>
          <label class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t('admin.emailBroadcast.form.recipients') }}
            <span class="text-red-500">*</span>
          </label>

          <div class="rounded-lg border border-gray-200 p-4 dark:border-dark-700">
            <label class="flex items-center gap-2 text-sm font-medium text-gray-900 dark:text-white">
              <input
                v-model="sendToAll"
                type="checkbox"
                class="form-checkbox"
              />
              <span>{{ t('admin.emailBroadcast.form.sendToAll') }}</span>
              <span class="ml-2 text-xs font-normal text-gray-500 dark:text-gray-400">
                {{ t('admin.emailBroadcast.form.sendToAllHint') }}
              </span>
            </label>

            <div v-if="!sendToAll" class="mt-4 space-y-3">
              <div class="relative">
                <input
                  v-model="recipientSearch"
                  type="text"
                  class="input"
                  :placeholder="t('admin.emailBroadcast.form.searchRecipientsPlaceholder')"
                  @input="handleRecipientSearch"
                  @focus="recipientPickerOpen = true"
                />
                <div
                  v-if="recipientPickerOpen && (searchLoading || recipientCandidates.length > 0 || (recipientSearch && !searchLoading))"
                  class="absolute left-0 right-0 z-10 mt-1 max-h-60 overflow-y-auto rounded-lg border border-gray-200 bg-white shadow-lg dark:border-dark-700 dark:bg-dark-800"
                >
                  <div v-if="searchLoading" class="px-4 py-2 text-sm text-gray-500">
                    {{ t('common.loading') }}
                  </div>
                  <div v-else-if="recipientCandidates.length === 0" class="px-4 py-2 text-sm text-gray-500">
                    {{ t('admin.emailBroadcast.form.noRecipientsFound') }}
                  </div>
                  <ul v-else>
                    <li
                      v-for="candidate in recipientCandidates"
                      :key="candidate.id"
                      class="cursor-pointer border-b border-gray-100 px-4 py-2 hover:bg-gray-50 dark:border-dark-700 dark:hover:bg-dark-700"
                      @mousedown.prevent="addRecipient(candidate)"
                    >
                      <div class="text-sm font-medium text-gray-900 dark:text-white">{{ candidate.email }}</div>
                      <div v-if="candidate.username" class="text-xs text-gray-500 dark:text-gray-400">
                        {{ candidate.username }}
                      </div>
                    </li>
                  </ul>
                </div>
              </div>

              <div v-if="selectedRecipients.length > 0" class="flex flex-wrap gap-2">
                <span
                  v-for="r in selectedRecipients"
                  :key="r.id"
                  class="inline-flex items-center gap-1 rounded-full bg-blue-50 px-3 py-1 text-xs text-blue-700 dark:bg-blue-900/30 dark:text-blue-300"
                >
                  {{ r.email }}
                  <button
                    type="button"
                    class="ml-0.5 text-blue-600 hover:text-blue-800 dark:text-blue-400"
                    @click="removeRecipient(r.id)"
                  >
                    &times;
                  </button>
                </span>
              </div>
              <p v-else class="text-xs text-gray-500 dark:text-gray-400">
                {{ t('admin.emailBroadcast.form.noRecipientsSelected') }}
              </p>
            </div>
          </div>
        </div>

        <div v-if="errorMessage" class="rounded-lg bg-red-50 px-4 py-3 text-sm text-red-700 dark:bg-red-900/30 dark:text-red-300">
          {{ errorMessage }}
        </div>
      </div>

      <!-- History -->
      <div>
        <button
          type="button"
          class="flex w-full items-center justify-between rounded-lg border border-gray-200 px-4 py-3 text-sm font-medium text-gray-700 hover:bg-gray-50 dark:border-dark-700 dark:text-gray-300 dark:hover:bg-dark-700"
          @click="toggleHistory"
        >
          <span>{{ t('admin.emailBroadcast.history.title') }}</span>
          <span class="text-xs text-gray-500">{{ historyExpanded ? '▲' : '▼' }}</span>
        </button>
        <div v-if="historyExpanded" class="mt-3 space-y-2">
          <div v-if="historyLoading" class="py-4 text-center text-sm text-gray-500">
            {{ t('common.loading') }}
          </div>
          <div v-else-if="historyItems.length === 0" class="py-4 text-center text-sm text-gray-500">
            {{ t('admin.emailBroadcast.history.empty') }}
          </div>
          <ul v-else class="divide-y divide-gray-100 dark:divide-dark-700">
            <li
              v-for="item in historyItems"
              :key="item.id"
              class="flex flex-col gap-1 py-3 text-sm sm:flex-row sm:items-center sm:justify-between"
            >
              <div class="min-w-0 flex-1">
                <div class="truncate font-medium text-gray-900 dark:text-white">{{ item.subject }}</div>
                <div class="text-xs text-gray-500 dark:text-gray-400">
                  {{ formatDateTime(item.created_at) }}
                  &middot;
                  {{ t(`admin.emailBroadcast.recipientsMode.${item.recipients_mode}`) }}
                </div>
              </div>
              <div class="flex items-center gap-3 text-xs">
                <span
                  class="rounded-full px-2 py-0.5"
                  :class="statusBadgeClass(item.status)"
                >
                  {{ t(`admin.emailBroadcast.status.${item.status}`) }}
                </span>
                <span class="text-gray-500 dark:text-gray-400">
                  {{ item.success_count }} / {{ item.total_count }}
                </span>
              </div>
            </li>
          </ul>
        </div>
      </div>
    </div>

    <template #footer>
      <div class="flex items-center justify-end gap-3">
        <button type="button" class="btn btn-secondary" @click="handleClose">
          {{ t('common.cancel') }}
        </button>
        <button
          type="button"
          class="btn btn-primary"
          :disabled="sending || !canSend"
          @click="handleSend"
        >
          <svg v-if="sending" class="h-4 w-4 animate-spin" fill="none" viewBox="0 0 24 24">
            <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4" />
            <path
              class="opacity-75"
              fill="currentColor"
              d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"
            />
          </svg>
          {{ sending ? t('admin.emailBroadcast.form.sending') : t('admin.emailBroadcast.form.send') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import { formatDateTime } from '@/utils/format'
import type {
  EmailBroadcastBodyFormat,
  EmailBroadcastRecipientCandidate,
  EmailBroadcastStatus,
  EmailBroadcastSummary
} from '@/api/admin/emailBroadcasts'

interface Props {
  show: boolean
}

interface Emits {
  (e: 'close'): void
  (e: 'sent', broadcastId: number): void
}

const props = defineProps<Props>()
const emit = defineEmits<Emits>()
const { t } = useI18n()
const appStore = useAppStore()

const SUBJECT_MAX_LEN = 200
const BODY_MAX_LEN = 65536

const form = ref<{
  subject: string
  body: string
  body_format: EmailBroadcastBodyFormat
}>({
  subject: '',
  body: '',
  body_format: 'html'
})

const sendToAll = ref(false)
const selectedRecipients = ref<EmailBroadcastRecipientCandidate[]>([])
const recipientSearch = ref('')
const recipientCandidates = ref<EmailBroadcastRecipientCandidate[]>([])
const recipientPickerOpen = ref(false)
const searchLoading = ref(false)
let searchTimer: ReturnType<typeof setTimeout> | null = null

const sending = ref(false)
const errorMessage = ref('')

const historyExpanded = ref(false)
const historyLoading = ref(false)
const historyItems = ref<EmailBroadcastSummary[]>([])

const bodyPlaceholder = computed(() =>
  form.value.body_format === 'html'
    ? t('admin.emailBroadcast.form.bodyPlaceholderHtml')
    : t('admin.emailBroadcast.form.bodyPlaceholderText')
)

const canSend = computed(() => {
  if (!form.value.subject.trim() || !form.value.body.trim()) return false
  if (sendToAll.value) return true
  return selectedRecipients.value.length > 0
})

watch(
  () => props.show,
  show => {
    if (show) {
      resetForm()
    } else {
      recipientPickerOpen.value = false
    }
  }
)

function resetForm() {
  form.value.subject = ''
  form.value.body = ''
  form.value.body_format = 'html'
  sendToAll.value = false
  selectedRecipients.value = []
  recipientSearch.value = ''
  recipientCandidates.value = []
  errorMessage.value = ''
}

function handleClose() {
  emit('close')
}

function handleRecipientSearch() {
  if (searchTimer) clearTimeout(searchTimer)
  const q = recipientSearch.value.trim()
  if (!q) {
    recipientCandidates.value = []
    return
  }
  searchTimer = setTimeout(async () => {
    searchLoading.value = true
    try {
      const { items } = await adminAPI.emailBroadcasts.searchRecipients(q, 20)
      const selectedIds = new Set(selectedRecipients.value.map(r => r.id))
      recipientCandidates.value = items.filter(item => !selectedIds.has(item.id))
    } catch (err) {
      console.error('search recipients failed', err)
      recipientCandidates.value = []
    } finally {
      searchLoading.value = false
    }
  }, 250)
}

function addRecipient(candidate: EmailBroadcastRecipientCandidate) {
  if (selectedRecipients.value.find(r => r.id === candidate.id)) return
  selectedRecipients.value.push(candidate)
  recipientCandidates.value = recipientCandidates.value.filter(c => c.id !== candidate.id)
  recipientSearch.value = ''
  recipientPickerOpen.value = false
}

function removeRecipient(id: number) {
  selectedRecipients.value = selectedRecipients.value.filter(r => r.id !== id)
}

async function handleSend() {
  errorMessage.value = ''
  if (!canSend.value) return

  sending.value = true
  try {
    const broadcast = await adminAPI.emailBroadcasts.create({
      subject: form.value.subject.trim(),
      body: form.value.body,
      body_format: form.value.body_format,
      recipients_mode: sendToAll.value ? 'all' : 'selected',
      recipient_user_ids: sendToAll.value ? undefined : selectedRecipients.value.map(r => r.id)
    })
    appStore.showSuccess(t('admin.emailBroadcast.notifications.sendQueued', { id: broadcast.id }))
    emit('sent', broadcast.id)
    handleClose()
  } catch (err: any) {
    const msg = err?.response?.data?.message || err?.message || t('common.unknownError')
    errorMessage.value = msg
  } finally {
    sending.value = false
  }
}

async function toggleHistory() {
  historyExpanded.value = !historyExpanded.value
  if (historyExpanded.value && historyItems.value.length === 0) {
    await loadHistory()
  }
}

async function loadHistory() {
  historyLoading.value = true
  try {
    const result = await adminAPI.emailBroadcasts.list(1, 10)
    historyItems.value = result.items
  } catch (err) {
    console.error('load broadcast history failed', err)
  } finally {
    historyLoading.value = false
  }
}

function statusBadgeClass(status: EmailBroadcastStatus): string {
  switch (status) {
    case 'completed':
      return 'bg-green-50 text-green-700 dark:bg-green-900/30 dark:text-green-300'
    case 'sending':
      return 'bg-blue-50 text-blue-700 dark:bg-blue-900/30 dark:text-blue-300'
    case 'failed':
      return 'bg-red-50 text-red-700 dark:bg-red-900/30 dark:text-red-300'
    case 'pending':
    default:
      return 'bg-gray-100 text-gray-700 dark:bg-dark-700 dark:text-gray-300'
  }
}
</script>
