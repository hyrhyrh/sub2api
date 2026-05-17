<script setup lang="ts">
import { ref, computed, onMounted, onBeforeUnmount } from 'vue'
import { useRouter } from 'vue-router'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import AppLayout from '@/components/layout/AppLayout.vue'
import {
  Chart as ChartJS,
  LineElement,
  PointElement,
  CategoryScale,
  LinearScale,
  Tooltip,
  Legend
} from 'chart.js'
import { Line } from 'vue-chartjs'

ChartJS.register(LineElement, PointElement, CategoryScale, LinearScale, Tooltip, Legend)

const router = useRouter()
const appStore = useAppStore()
const authStore = useAuthStore()

// 站点名 — 取 publicSettings.site_name(动态;SSR 注入或 fetch 后生效)
const siteName = computed(() => appStore.cachedPublicSettings?.site_name || appStore.siteName || 'sub2api')

// 访问控制:setting=true → 任何人;setting=false → 仅管理员可见
// 普通用户 / 未登录访客被踢回首页
const accessChecked = ref(false)
const accessDenied = ref(false)
async function checkAccess() {
  // 确保 publicSettings 已加载
  if (!appStore.cachedPublicSettings) {
    try { await appStore.fetchPublicSettings() } catch { /* 忽略,按 false 处理 */ }
  }
  const publicEnabled = appStore.cachedPublicSettings?.speedtest_public_enabled === true
  if (publicEnabled) {
    accessDenied.value = false
  } else {
    const isAdmin = authStore.user?.role === 'admin'
    accessDenied.value = !isAdmin
  }
  accessChecked.value = true
  if (accessDenied.value) {
    // 不停留在页面;给一个明显提示后跳回首页
    setTimeout(() => router.replace('/'), 1500)
  }
}

interface Sample {
  seq: number
  rttMs: number
  clockSkewMs: number | null
  ts: number
}

const samples = ref<Sample[]>([])
const running = ref(false)
const seq = ref(0)
const attempts = ref(0)
const failures = ref(0)
const clientIP = ref<string>('')
const intervalMs = ref<number>(1000)
const maxSamples = ref<number>(60)
let timer: number | null = null

const min = computed(() => samples.value.length ? Math.min(...samples.value.map(s => s.rttMs)) : 0)
const max = computed(() => samples.value.length ? Math.max(...samples.value.map(s => s.rttMs)) : 0)
const avg = computed(() => samples.value.length ? samples.value.reduce((s, x) => s + x.rttMs, 0) / samples.value.length : 0)
const last = computed(() => samples.value.length ? samples.value[samples.value.length - 1].rttMs : 0)
// 抖动 = 相邻 RTT 差值的平均绝对值
const jitter = computed(() => {
  if (samples.value.length < 2) return 0
  let total = 0
  for (let i = 1; i < samples.value.length; i++) {
    total += Math.abs(samples.value[i].rttMs - samples.value[i - 1].rttMs)
  }
  return total / (samples.value.length - 1)
})
const lossPct = computed(() => attempts.value ? (failures.value / attempts.value) * 100 : 0)

function rttClass(rtt: number): string {
  if (rtt < 80) return 'text-emerald-600 dark:text-emerald-400'
  if (rtt < 200) return 'text-amber-600 dark:text-amber-400'
  return 'text-red-600 dark:text-red-400'
}

function rttLabel(rtt: number): string {
  if (rtt < 80) return '优'
  if (rtt < 200) return '良'
  if (rtt < 500) return '中'
  return '差'
}

async function ping() {
  attempts.value++
  const start = performance.now()
  const clientReq = Date.now()
  const url = `/api/ping?_=${clientReq}-${Math.random().toString(36).slice(2)}`
  try {
    const r = await fetch(url, { method: 'GET', cache: 'no-store' })
    if (!r.ok) {
      failures.value++
      return
    }
    const data = await r.json()
    const end = performance.now()
    const clientRecv = Date.now()
    const rttMs = end - start
    const midServer = (clientReq + clientRecv) / 2
    const skew = data.server_time_ms ? data.server_time_ms - midServer : null
    if (data.client_ip && !clientIP.value) {
      clientIP.value = data.client_ip
    }
    samples.value.push({ seq: ++seq.value, rttMs, clockSkewMs: skew, ts: clientRecv })
    if (samples.value.length > maxSamples.value) {
      samples.value.shift()
    }
  } catch (e) {
    failures.value++
    console.warn('ping failed', e)
  }
}

function start() {
  if (running.value) return
  running.value = true
  ping()
  timer = window.setInterval(ping, intervalMs.value)
}

function stop() {
  running.value = false
  if (timer !== null) {
    clearInterval(timer)
    timer = null
  }
}

function reset() {
  stop()
  samples.value = []
  seq.value = 0
  attempts.value = 0
  failures.value = 0
}

onMounted(async () => {
  await checkAccess()
  if (!accessDenied.value) {
    start()
  }
})

onBeforeUnmount(stop)

const chartData = computed(() => ({
  labels: samples.value.map(s => `#${s.seq}`),
  datasets: [
    {
      label: 'RTT (ms)',
      data: samples.value.map(s => s.rttMs),
      borderColor: '#3b82f6',
      backgroundColor: '#3b82f633',
      tension: 0.3,
      pointRadius: 2
    }
  ]
}))

const chartOptions = {
  responsive: true,
  maintainAspectRatio: false,
  animation: { duration: 200 } as const,
  plugins: { legend: { display: false } },
  scales: {
    y: {
      beginAtZero: true,
      ticks: { callback: (v: number | string) => v + 'ms' }
    }
  }
}
</script>

<template>
  <AppLayout>
    <!-- 权限检查中 -->
    <div v-if="!accessChecked" class="flex min-h-[60vh] items-center justify-center text-sm text-gray-400">
      正在检查访问权限…
    </div>

    <!-- 被拒绝(setting=false 且非管理员) -->
    <div v-else-if="accessDenied" class="mx-auto mt-20 max-w-md rounded-2xl border border-red-200 bg-white p-8 text-center dark:border-red-900 dark:bg-gray-800">
      <div class="text-5xl">🔒</div>
      <h2 class="mt-3 text-lg font-semibold text-gray-900 dark:text-white">无权访问</h2>
      <p class="mt-2 text-sm text-gray-500 dark:text-gray-400">
        测速页面仅对管理员开放,管理员可在「系统设置 → 公开测速页面」打开公开访问。
      </p>
      <p class="mt-3 text-xs text-gray-400">即将跳转回首页…</p>
    </div>

    <div v-else>
      <!-- Header -->
      <div class="mb-6">
        <h1 class="text-2xl font-bold text-gray-900 dark:text-white">网络测速</h1>
        <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
          测量你的客户端到 {{ siteName }} 节点的往返时延 (RTT)
          <span v-if="clientIP" class="ml-2 text-xs text-gray-400">· 你的 IP: <span class="font-mono">{{ clientIP }}</span></span>
        </p>
      </div>

      <!-- 实时数字 -->
      <div class="grid grid-cols-2 gap-3 sm:grid-cols-6">
        <div class="rounded-2xl border border-gray-200 bg-white p-4 text-center dark:border-gray-700 dark:bg-gray-800">
          <div class="text-xs text-gray-500 dark:text-gray-400">最新</div>
          <div class="mt-1 text-2xl font-bold font-mono" :class="rttClass(last)">
            {{ last ? last.toFixed(0) + 'ms' : '-' }}
          </div>
          <div v-if="last" class="text-xs" :class="rttClass(last)">{{ rttLabel(last) }}</div>
        </div>
        <div class="rounded-2xl border border-gray-200 bg-white p-4 text-center dark:border-gray-700 dark:bg-gray-800">
          <div class="text-xs text-gray-500 dark:text-gray-400">最小</div>
          <div class="mt-1 text-2xl font-bold font-mono text-emerald-600 dark:text-emerald-400">
            {{ samples.length ? min.toFixed(0) + 'ms' : '-' }}
          </div>
        </div>
        <div class="rounded-2xl border border-gray-200 bg-white p-4 text-center dark:border-gray-700 dark:bg-gray-800">
          <div class="text-xs text-gray-500 dark:text-gray-400">平均</div>
          <div class="mt-1 text-2xl font-bold font-mono" :class="rttClass(avg)">
            {{ samples.length ? avg.toFixed(0) + 'ms' : '-' }}
          </div>
        </div>
        <div class="rounded-2xl border border-gray-200 bg-white p-4 text-center dark:border-gray-700 dark:bg-gray-800">
          <div class="text-xs text-gray-500 dark:text-gray-400">最大</div>
          <div class="mt-1 text-2xl font-bold font-mono text-red-600 dark:text-red-400">
            {{ samples.length ? max.toFixed(0) + 'ms' : '-' }}
          </div>
        </div>
        <div class="rounded-2xl border border-gray-200 bg-white p-4 text-center dark:border-gray-700 dark:bg-gray-800">
          <div class="text-xs text-gray-500 dark:text-gray-400">抖动</div>
          <div class="mt-1 text-2xl font-bold font-mono text-gray-700 dark:text-gray-300">
            {{ samples.length > 1 ? jitter.toFixed(0) + 'ms' : '-' }}
          </div>
        </div>
        <div class="rounded-2xl border border-gray-200 bg-white p-4 text-center dark:border-gray-700 dark:bg-gray-800" :title="`${failures}/${attempts} 失败`">
          <div class="text-xs text-gray-500 dark:text-gray-400">丢包率</div>
          <div class="mt-1 text-2xl font-bold font-mono" :class="lossPct > 1 ? 'text-red-600 dark:text-red-400' : 'text-gray-700 dark:text-gray-300'">
            {{ attempts ? lossPct.toFixed(1) + '%' : '-' }}
          </div>
        </div>
      </div>

      <!-- 控制条 -->
      <div class="mt-4 flex items-center justify-between flex-wrap gap-3 rounded-2xl border border-gray-200 bg-white p-4 dark:border-gray-700 dark:bg-gray-800">
        <div class="flex items-center gap-2 text-sm">
          <label class="text-gray-500 dark:text-gray-400">采样间隔:</label>
          <select v-model.number="intervalMs" @change="() => { if (running) { stop(); start(); } }"
                  class="rounded border px-2 py-1 dark:border-gray-600 dark:bg-gray-700" title="发送 ping 的间隔">
            <option :value="500">500ms</option>
            <option :value="1000">1s</option>
            <option :value="2000">2s</option>
            <option :value="5000">5s</option>
          </select>
          <label class="ml-3 text-gray-500 dark:text-gray-400">保留样本:</label>
          <select v-model.number="maxSamples" class="rounded border px-2 py-1 dark:border-gray-600 dark:bg-gray-700">
            <option :value="30">30</option>
            <option :value="60">60</option>
            <option :value="120">120</option>
            <option :value="300">300</option>
          </select>
          <span class="ml-3 text-xs text-gray-400">已采样 {{ samples.length }}</span>
        </div>
        <div class="flex items-center gap-2">
          <button v-if="!running" @click="start"
                  class="rounded-lg bg-emerald-600 px-4 py-1.5 text-sm font-medium text-white hover:bg-emerald-700">
            ▶ 开始
          </button>
          <button v-else @click="stop"
                  class="rounded-lg bg-orange-600 px-4 py-1.5 text-sm font-medium text-white hover:bg-orange-700">
            ⏸ 暂停
          </button>
          <button @click="reset"
                  class="rounded-lg bg-gray-200 px-4 py-1.5 text-sm font-medium text-gray-700 hover:bg-gray-300 dark:bg-gray-700 dark:text-gray-300">
            ↺ 重置
          </button>
        </div>
      </div>

      <!-- 曲线 -->
      <div class="mt-4 rounded-2xl border border-gray-200 bg-white p-4 dark:border-gray-700 dark:bg-gray-800">
        <h3 class="mb-2 text-sm font-semibold text-gray-700 dark:text-gray-200">RTT 曲线</h3>
        <div class="h-64">
          <Line v-if="samples.length" :data="chartData" :options="chartOptions" />
          <div v-else class="flex h-full items-center justify-center text-sm text-gray-400">采样中…</div>
        </div>
      </div>

      <!-- 说明 -->
      <div class="mt-4 rounded-2xl border border-blue-200 bg-blue-50/40 p-4 text-sm text-gray-600 dark:border-blue-900/50 dark:bg-blue-900/10 dark:text-gray-300">
        <div class="mb-2 text-base font-semibold text-gray-700 dark:text-gray-200">💡 RTT 评判标准</div>
        <div class="grid grid-cols-2 gap-2 sm:grid-cols-4">
          <div><span class="rounded bg-emerald-100 px-1.5 py-0.5 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300">优</span> &lt; 80ms — 同城/同区域</div>
          <div><span class="rounded bg-amber-100 px-1.5 py-0.5 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300">良</span> 80~200ms — 跨区/正常出海</div>
          <div><span class="rounded bg-red-100 px-1.5 py-0.5 text-red-700 dark:bg-red-900/40 dark:text-red-300">中</span> 200~500ms — 弱网/绕路</div>
          <div><span class="rounded bg-red-200 px-1.5 py-0.5 text-red-800 dark:bg-red-900/60 dark:text-red-200">差</span> &gt; 500ms — 严重抖动,建议换网络</div>
        </div>
        <div class="mt-4 mb-2 text-base font-semibold text-gray-700 dark:text-gray-200">📉 丢包率评判标准</div>
        <div class="grid grid-cols-2 gap-2 sm:grid-cols-4">
          <div><span class="rounded bg-emerald-100 px-1.5 py-0.5 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300">优</span> 0% — 链路稳定</div>
          <div><span class="rounded bg-amber-100 px-1.5 py-0.5 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300">良</span> 0~1% — 偶发丢包,可接受</div>
          <div><span class="rounded bg-red-100 px-1.5 py-0.5 text-red-700 dark:bg-red-900/40 dark:text-red-300">中</span> 1~5% — 影响交互体验</div>
          <div><span class="rounded bg-red-200 px-1.5 py-0.5 text-red-800 dark:bg-red-900/60 dark:text-red-200">差</span> &gt; 5% — 严重影响,建议换网络</div>
        </div>
        <div class="mt-3 text-gray-500">
          RTT(往返时延)= HTTP 请求发起 → 收到服务器响应的耗时。包含 TCP/TLS(已建立连接后忽略) + HTTP 头部往返。
          抖动反映网络稳定性,&gt; 50ms 通常说明链路不稳。
          丢包率 = 失败请求数 / 总请求数,反映链路可达性,流式 LLM 响应对丢包尤其敏感。
        </div>
      </div>
    </div>
  </AppLayout>
</template>
