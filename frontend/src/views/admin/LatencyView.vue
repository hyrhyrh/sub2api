<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import {
  getLatencyOverview,
  getLatencyByRegion,
  getLatencyByUser,
  getLatencyTrend,
  getSlowRequests,
  type LatencyOverview,
  type LatencyRegionStats,
  type LatencyUserStats,
  type LatencyTrendPoint,
  type SlowRequestRow,
  type LatencyAccessType,
  type LatencyThresholds
} from '@/api/admin/latency'
import {
  Chart as ChartJS,
  LineElement,
  PointElement,
  BarElement,
  CategoryScale,
  LinearScale,
  Tooltip,
  Legend,
  Filler
} from 'chart.js'
import { Line } from 'vue-chartjs'
import AppLayout from '@/components/layout/AppLayout.vue'
import Pagination from '@/components/common/Pagination.vue'
import DateRangePicker from '@/components/common/DateRangePicker.vue'
import { adminAPI } from '@/api/admin'

ChartJS.register(LineElement, PointElement, BarElement, CategoryScale, LinearScale, Tooltip, Legend, Filler)

// ============= 状态 =============
const loading = ref(false)
const overview = ref<LatencyOverview | null>(null)
const thresholds = ref<LatencyThresholds | null>(null)
const regionStats = ref<LatencyRegionStats[]>([])
const userStats = ref<LatencyUserStats[]>([])
const trendPoints = ref<LatencyTrendPoint[]>([])
const slowRows = ref<SlowRequestRow[]>([])
const selectedRow = ref<SlowRequestRow | null>(null)

// 过滤
function todayStr() { return new Date().toISOString().slice(0, 10) }
function daysAgoStr(d: number) {
  const t = new Date(); t.setDate(t.getDate() - d)
  return t.toISOString().slice(0, 10)
}
// DateRangePicker 用 YYYY-MM-DD;默认最近 1 天
const startDate = ref<string>(daysAgoStr(0)) // 今天 00:00 的本地日期
const endDate = ref<string>(todayStr())
const accessType = ref<LatencyAccessType>('')
const modelFilter = ref<string>('')
const modelOptions = ref<string[]>([])
const trendBucket = ref<'hour' | 'day'>('hour')
const slowMode = ref<'top' | 'slow'>('top')

// YYYY-MM-DD → 当天本地 0 点 / 次日 0 点的 ISO,组成 [start, end) 区间
function dateToStartISO(s: string): string {
  if (!s) return ''
  return new Date(s + 'T00:00:00').toISOString()
}
function dateToEndISO(s: string): string {
  if (!s) return ''
  const d = new Date(s + 'T00:00:00')
  d.setDate(d.getDate() + 1) // 把结束日的"全天"也包进去 → 次日 0 点为右开区间
  return d.toISOString()
}

// 分页:每个表独立 page + pageSize(共用 Pagination 组件,与使用记录页风格一致)
const userPage = ref(1)
const userPageSize = ref(20)
const regionPage = ref(1)
const regionPageSize = ref(20)
const slowPage = ref(1)
const slowPageSize = ref(20)

// 国家码 → 中文国名(浏览器内置 ICU 表,精度足够)
const countryNameFmt = (() => {
  try { return new Intl.DisplayNames(['zh-CN'], { type: 'region' }) } catch { return null }
})()
function countryName(code: string): string {
  if (!code || code.length !== 2) return ''
  try { return countryNameFmt?.of(code.toUpperCase()) ?? '' } catch { return '' }
}

// 省/州英文 → 中文。覆盖中国大陆 31+港澳台、美国 50 州。
// 其他国家命中不到时返回空字符串,模板里就不拼 "/ xxx" 后缀。
const REGION_ZH: Record<string, string> = {
  // 中国大陆
  Beijing: '北京', Shanghai: '上海', Tianjin: '天津', Chongqing: '重庆',
  Hebei: '河北', Shanxi: '山西', Liaoning: '辽宁', Jilin: '吉林', Heilongjiang: '黑龙江',
  Jiangsu: '江苏', Zhejiang: '浙江', Anhui: '安徽', Fujian: '福建', Jiangxi: '江西',
  Shandong: '山东', Henan: '河南', Hubei: '湖北', Hunan: '湖南', Guangdong: '广东',
  Hainan: '海南', Sichuan: '四川', Guizhou: '贵州', Yunnan: '云南', Shaanxi: '陕西',
  Gansu: '甘肃', Qinghai: '青海', Taiwan: '台湾',
  'Inner Mongolia': '内蒙古', 'Guangxi Zhuang Autonomous Region': '广西',
  'Guangxi': '广西', Tibet: '西藏', Xinjiang: '新疆', Ningxia: '宁夏',
  'Hong Kong': '香港', Macau: '澳门', Macao: '澳门',
  // 美国 50 州
  Alabama: '亚拉巴马', Alaska: '阿拉斯加', Arizona: '亚利桑那', Arkansas: '阿肯色',
  California: '加利福尼亚', Colorado: '科罗拉多', Connecticut: '康涅狄格', Delaware: '特拉华',
  Florida: '佛罗里达', Georgia: '佐治亚', Hawaii: '夏威夷', Idaho: '爱达荷',
  Illinois: '伊利诺伊', Indiana: '印第安纳', Iowa: '艾奥瓦', Kansas: '堪萨斯',
  Kentucky: '肯塔基', Louisiana: '路易斯安那', Maine: '缅因', Maryland: '马里兰',
  Massachusetts: '马萨诸塞', Michigan: '密歇根', Minnesota: '明尼苏达', Mississippi: '密西西比',
  Missouri: '密苏里', Montana: '蒙大拿', Nebraska: '内布拉斯加', Nevada: '内华达',
  'New Hampshire': '新罕布什尔', 'New Jersey': '新泽西', 'New Mexico': '新墨西哥', 'New York': '纽约',
  'North Carolina': '北卡罗来纳', 'North Dakota': '北达科他', Ohio: '俄亥俄', Oklahoma: '俄克拉荷马',
  Oregon: '俄勒冈', Pennsylvania: '宾夕法尼亚', 'Rhode Island': '罗德岛',
  'South Carolina': '南卡罗来纳', 'South Dakota': '南达科他', Tennessee: '田纳西',
  Texas: '得克萨斯', Utah: '犹他', Vermont: '佛蒙特', Virginia: '弗吉尼亚',
  Washington: '华盛顿', 'West Virginia': '西弗吉尼亚', Wisconsin: '威斯康星', Wyoming: '怀俄明',
  'District of Columbia': '哥伦比亚特区'
}
function regionZh(name: string): string {
  if (!name) return ''
  return REGION_ZH[name] ?? ''
}

const filters = computed(() => ({
  start: dateToStartISO(startDate.value),
  end: dateToEndISO(endDate.value),
  access_type: accessType.value,
  model: modelFilter.value
}))

// ============= 加载 =============
async function refresh() {
  loading.value = true
  // 每次刷新回到第一页,避免旧 page 越界
  userPage.value = 1
  regionPage.value = 1
  slowPage.value = 1
  const f = filters.value
  // allSettled:单个接口报错不会拖累其他;每个 reject 单独打 console.error 便于排查。
  // 一次拉够,前端切片分页(by-user/by-region 后端上限 200,slow-requests 上限 500)
  const [ovR, regionR, userR, trendR, slowR] = await Promise.allSettled([
    getLatencyOverview(f),
    getLatencyByRegion(f, 200),
    getLatencyByUser(f, 200),
    getLatencyTrend(f, trendBucket.value),
    getSlowRequests(f, 500, slowMode.value)
  ])
  if (ovR.status === 'fulfilled') {
    overview.value = ovR.value.overview
    thresholds.value = ovR.value.thresholds
  } else { console.error('overview failed', ovR.reason) }
  if (regionR.status === 'fulfilled') regionStats.value = regionR.value
  else { console.error('by-region failed', regionR.reason); regionStats.value = [] }
  if (userR.status === 'fulfilled') userStats.value = userR.value
  else { console.error('by-user failed', userR.reason); userStats.value = [] }
  if (trendR.status === 'fulfilled') trendPoints.value = trendR.value
  else { console.error('trend failed', trendR.reason); trendPoints.value = [] }
  if (slowR.status === 'fulfilled') slowRows.value = slowR.value
  else { console.error('slow-requests failed', slowR.reason); slowRows.value = [] }
  loading.value = false
}

async function loadModelOptions() {
  try {
    const end = new Date()
    const start = new Date(end.getTime() - 7 * 24 * 3600 * 1000) // 用最近 7 天作 model 列表来源
    const ms = await adminAPI.dashboard.getModelStats({
      start_date: start.toISOString().slice(0, 10),
      end_date: end.toISOString().slice(0, 10)
    })
    const set = new Set<string>()
    ;(ms.models ?? []).forEach((s: any) => { if (s.model) set.add(s.model) })
    modelOptions.value = Array.from(set).sort()
  } catch (e) {
    console.warn('load model options failed', e)
  }
}

onMounted(() => {
  loadModelOptions()
  refresh()
})

// ============= 派生 =============
const slowRatioPct = computed(() =>
  overview.value && overview.value.total_requests > 0
    ? ((overview.value.slow_count / overview.value.total_requests) * 100).toFixed(2)
    : '0.00'
)

function bottleneckLabel(k: string): string {
  switch (k) {
    case 'upstream_slow': return '上游慢'
    case 'server_internal': return '服务器内部'
    case 'delivery_slow': return '客户端网络(推测)'
    case 'normal': return '正常'
    default: return k
  }
}

function bottleneckColor(k: string): string {
  switch (k) {
    case 'upstream_slow': return 'bg-orange-100 text-orange-700 dark:bg-orange-900/40 dark:text-orange-300'
    case 'server_internal': return 'bg-red-100 text-red-700 dark:bg-red-900/40 dark:text-red-300'
    case 'delivery_slow': return 'bg-blue-100 text-blue-700 dark:bg-blue-900/40 dark:text-blue-300'
    default: return 'bg-gray-100 text-gray-600 dark:bg-gray-800 dark:text-gray-400'
  }
}

interface DimMeta {
  label: string
  short: string
  tip: string
  color: string
}
const DIM_META: Record<string, DimMeta> = {
  server_processing: { label: '服务器处理 (T2-T1)', short: '服务器', tip: '请求进入到把数据发往上游之间的内部耗时', color: 'text-red-600 dark:text-red-400' },
  upstream_ttfb:    { label: '上游首字节 TTFB (T3-T2)', short: 'TTFB', tip: '上游收到请求到返回首个 token 的时间', color: 'text-orange-600 dark:text-orange-400' },
  upstream_stream:  { label: '上游流式传输 (T4-T3)', short: '流式', tip: '上游开始返回到响应读完的时间(非流式为 0)', color: 'text-emerald-600 dark:text-emerald-400' },
  response_delivery:{ label: '回传客户端 (T5-T4)', short: '交付', tip: '上游读完到 sub2api 把响应写完的时间,近似客户端网络耗时', color: 'text-blue-600 dark:text-blue-400' },
  total_latency:    { label: '总耗时 (T5-T1)', short: '总', tip: '从请求进入到响应写完的端到端时间', color: 'text-indigo-600 dark:text-indigo-400' }
}

function dimensionLabel(d: string): string {
  return DIM_META[d]?.label ?? d
}

function fmt(n?: number | null): string {
  if (n === null || n === undefined) return '-'
  if (n >= 10_000) return (n / 1000).toFixed(1) + 's'
  return Math.round(n) + 'ms'
}

function fmtTime(iso: string): string {
  return new Date(iso).toLocaleString('zh-CN', { hour12: false })
}

function regionFlag(country: string): string {
  if (!country || country.length !== 2) return '🏳'
  const codeA = 0x1F1E6
  const A = 'A'.charCodeAt(0)
  return String.fromCodePoint(
    codeA + country.toUpperCase().charCodeAt(0) - A,
    codeA + country.toUpperCase().charCodeAt(1) - A
  )
}

// ============= 趋势图 =============
const trendChartData = computed(() => {
  if (!trendPoints.value.length) return null
  const labels = trendPoints.value.map(p =>
    new Date(p.bucket).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit' })
  )
  return {
    labels,
    datasets: [
      { label: '服务器', data: trendPoints.value.map(p => p.avg_server_ms), borderColor: '#ef4444', backgroundColor: '#ef444433', tension: 0.3, fill: false },
      { label: 'TTFB', data: trendPoints.value.map(p => p.avg_upstream_ttfb_ms), borderColor: '#f59e0b', backgroundColor: '#f59e0b33', tension: 0.3, fill: false },
      { label: '流式', data: trendPoints.value.map(p => p.avg_upstream_stream_ms), borderColor: '#10b981', backgroundColor: '#10b98133', tension: 0.3, fill: false },
      { label: '交付', data: trendPoints.value.map(p => p.avg_delivery_ms), borderColor: '#3b82f6', backgroundColor: '#3b82f633', tension: 0.3, fill: false },
      { label: '总', data: trendPoints.value.map(p => p.avg_total_ms), borderColor: '#6366f1', backgroundColor: '#6366f133', tension: 0.3, fill: false, borderDash: [5, 5] }
    ]
  }
})

const trendChartOptions = {
  responsive: true,
  maintainAspectRatio: false,
  plugins: { legend: { position: 'bottom' as const } },
  scales: {
    y: {
      beginAtZero: true,
      ticks: { callback: (v: number | string) => v + 'ms' }
    }
  }
}

// ============= 分页(纯客户端切片) =============
function slice<T>(rows: T[], page: number, size: number): T[] {
  const start = (page - 1) * size
  return rows.slice(start, start + size)
}
const userPaged = computed(() => slice(userStats.value, userPage.value, userPageSize.value))
const regionPaged = computed(() => slice(regionStats.value, regionPage.value, regionPageSize.value))
const slowPaged = computed(() => slice(slowRows.value, slowPage.value, slowPageSize.value))

// ============= 瀑布图 =============
function waterfallSegments(row: SlowRequestRow) {
  const segs = [
    { label: '服务器 (T1→T2)', ms: row.server_processing_ms ?? 0, color: 'bg-red-400' },
    { label: '上游 TTFB (T2→T3)', ms: row.upstream_ttfb_ms ?? 0, color: 'bg-orange-400' },
    { label: '上游流式 (T3→T4)', ms: row.upstream_stream_ms ?? 0, color: 'bg-emerald-400' },
    { label: '交付 (T4→T5)', ms: row.response_delivery_ms ?? 0, color: 'bg-blue-400' }
  ]
  const total = Math.max(segs.reduce((s, x) => s + x.ms, 0), 1)
  return segs.map(s => ({ ...s, pct: (s.ms / total) * 100 }))
}

// 用于行内的迷你 5 段分布
function isSlowRow(r: SlowRequestRow): boolean {
  const t = thresholds.value
  if (!t) return false
  return (
    (r.server_processing_ms ?? 0) > t.ServerProcessingMs ||
    (r.upstream_ttfb_ms ?? 0) > t.UpstreamTTFBMs ||
    (r.response_delivery_ms ?? 0) > t.ResponseDeliveryMs
  )
}
</script>

<template>
  <AppLayout>
    <div class="space-y-6 p-4">
      <!-- Toolbar(全局)-->
      <div class="flex items-center justify-between flex-wrap gap-2">
        <p class="text-sm text-gray-500 dark:text-gray-400">把端到端延迟拆为 5 段,定位"谁慢"</p>
        <div class="flex items-center gap-3">
          <DateRangePicker
            v-model:start-date="startDate"
            v-model:end-date="endDate"
            @change="refresh"
          />
          <select v-model="accessType" @change="refresh" class="rounded-lg border border-gray-300 px-3 py-1.5 text-sm dark:border-gray-600 dark:bg-gray-800" title="按入口类型过滤">
            <option value="">入口: 全部</option>
            <option value="domain">仅域名</option>
            <option value="direct_ip">仅 IP 直连</option>
          </select>
          <button @click="refresh" :disabled="loading" class="rounded-lg bg-blue-600 px-4 py-1.5 text-sm font-medium text-white hover:bg-blue-700 disabled:opacity-50">
            {{ loading ? '加载中...' : '刷新' }}
          </button>
        </div>
      </div>

      <!-- T1-T5 链路图示 -->
      <div class="rounded-2xl border border-blue-200 bg-blue-50/40 p-4 dark:border-blue-900/50 dark:bg-blue-900/10">
        <h2 class="mb-3 text-sm font-semibold text-gray-700 dark:text-gray-200">
          🔍 链路说明 — 一次请求被拆为 5 个时间点 T1~T5
        </h2>
        <div class="flex flex-wrap items-center gap-1 text-xs">
          <span class="rounded bg-gray-200 px-2 py-1 font-mono dark:bg-gray-700" title="客户端">客户端</span>
          <span class="text-gray-400">┄</span>
          <span class="rounded bg-blue-200 px-2 py-1 font-mono text-blue-800 dark:bg-blue-800 dark:text-blue-100" title="T1: sub2api 收到请求">T1 收到</span>
          <span class="text-red-500 font-mono" title="T2-T1: 服务器内部处理">→ 服务器 →</span>
          <span class="rounded bg-blue-200 px-2 py-1 font-mono text-blue-800 dark:bg-blue-800 dark:text-blue-100" title="T2: 开始发往上游">T2 发上游</span>
          <span class="text-orange-500 font-mono" title="T3-T2: 上游首字节时间">→ TTFB →</span>
          <span class="rounded bg-blue-200 px-2 py-1 font-mono text-blue-800 dark:bg-blue-800 dark:text-blue-100" title="T3: 收到上游首字节">T3 首字节</span>
          <span class="text-emerald-500 font-mono" title="T4-T3: 流式传输">→ 流式 →</span>
          <span class="rounded bg-blue-200 px-2 py-1 font-mono text-blue-800 dark:bg-blue-800 dark:text-blue-100" title="T4: 上游响应读完">T4 上游完成</span>
          <span class="text-blue-500 font-mono" title="T5-T4: 写回客户端">→ 交付 →</span>
          <span class="rounded bg-blue-200 px-2 py-1 font-mono text-blue-800 dark:bg-blue-800 dark:text-blue-100" title="T5: 响应全部写完">T5 写完</span>
          <span class="text-gray-400">┄</span>
          <span class="rounded bg-gray-200 px-2 py-1 font-mono dark:bg-gray-700">客户端</span>
        </div>
        <div class="mt-3 grid grid-cols-1 gap-2 text-xs text-gray-600 dark:text-gray-300 sm:grid-cols-2 lg:grid-cols-3">
          <div><span class="font-mono text-red-500">服务器 (T2-T1)</span>: 内部处理(路由/中间件)</div>
          <div><span class="font-mono text-orange-500">TTFB (T3-T2)</span>: 上游耗时,主导总延迟</div>
          <div><span class="font-mono text-emerald-500">流式 (T4-T3)</span>: 流式响应包总时长(非流式=0)</div>
          <div><span class="font-mono text-blue-500">交付 (T5-T4)</span>: ≈ 客户端→sub2api 网络</div>
          <div><span class="font-mono text-indigo-500">总 (T5-T1)</span>: 用户感知的端到端</div>
        </div>
      </div>

      <!-- 整体概览 -->
      <div v-if="overview" class="grid grid-cols-1 gap-4 md:grid-cols-3">
        <div class="rounded-2xl border border-gray-200 bg-white p-4 dark:border-gray-700 dark:bg-gray-800">
          <div class="text-xs text-gray-500 dark:text-gray-400">总请求数</div>
          <div class="mt-1 text-2xl font-bold text-gray-900 dark:text-white">{{ overview.total_requests.toLocaleString() }}</div>
        </div>
        <div class="rounded-2xl border border-gray-200 bg-white p-4 dark:border-gray-700 dark:bg-gray-800">
          <div class="text-xs text-gray-500 dark:text-gray-400">慢请求数</div>
          <div class="mt-1 text-2xl font-bold text-orange-600 dark:text-orange-400">
            {{ overview.slow_count.toLocaleString() }}
            <span class="text-sm font-normal text-gray-500">({{ slowRatioPct }}%)</span>
          </div>
          <div v-if="thresholds" class="mt-1 text-xs text-gray-400">
            阈值: 服务器 &gt; {{ thresholds.ServerProcessingMs }}ms /
            TTFB &gt; {{ thresholds.UpstreamTTFBMs }}ms /
            交付 &gt; {{ thresholds.ResponseDeliveryMs }}ms
          </div>
        </div>
        <div class="rounded-2xl border border-gray-200 bg-white p-4 dark:border-gray-700 dark:bg-gray-800">
          <div class="text-xs text-gray-500 dark:text-gray-400">瓶颈归因分布</div>
          <div class="mt-2 flex flex-wrap gap-2">
            <span v-for="k in ['upstream_slow', 'server_internal', 'delivery_slow', 'normal']" :key="k"
                  class="inline-flex items-center gap-1 rounded px-2 py-0.5 text-xs"
                  :class="bottleneckColor(k)">
              {{ bottleneckLabel(k) }} {{ overview.bottleneck_breakdown?.[k] || 0 }}
            </span>
          </div>
        </div>
      </div>

      <!-- 5 段分布 -->
      <div v-if="overview" class="rounded-2xl border border-gray-200 bg-white p-4 dark:border-gray-700 dark:bg-gray-800">
        <h2 class="mb-3 font-semibold text-gray-900 dark:text-white">5 段延迟分布(平均与分位数)</h2>
        <div class="overflow-x-auto">
          <table class="min-w-full text-sm">
            <thead class="bg-gray-50 text-gray-500 dark:bg-gray-900 dark:text-gray-400">
              <tr>
                <th class="px-3 py-2 text-left font-medium">维度</th>
                <th class="px-3 py-2 text-right font-medium" title="算术平均">平均 (avg)</th>
                <th class="px-3 py-2 text-right font-medium" title="中位数:50% 请求 ≤ 此值">中位 (p50)</th>
                <th class="px-3 py-2 text-right font-medium" title="95% 请求 ≤ 此值">P95</th>
                <th class="px-3 py-2 text-right font-medium" title="99% 请求 ≤ 此值,极端值">P99</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-200 dark:divide-gray-700">
              <tr v-for="d in (overview.distribution ?? [])" :key="d.dimension">
                <td class="px-3 py-2 text-gray-700 dark:text-gray-200" :title="DIM_META[d.dimension]?.tip ?? ''">{{ dimensionLabel(d.dimension) }}</td>
                <td class="px-3 py-2 text-right font-mono">{{ fmt(d.avg) }}</td>
                <td class="px-3 py-2 text-right font-mono">{{ fmt(d.p50) }}</td>
                <td class="px-3 py-2 text-right font-mono">{{ fmt(d.p95) }}</td>
                <td class="px-3 py-2 text-right font-mono">{{ fmt(d.p99) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <!-- 入口对比 -->
      <div v-if="overview?.access_type_breakdown?.length" class="rounded-2xl border border-gray-200 bg-white p-4 dark:border-gray-700 dark:bg-gray-800">
        <h2 class="mb-3 font-semibold text-gray-900 dark:text-white">入口对比 (域名 vs IP 直连)</h2>
        <table class="min-w-full text-sm">
          <thead class="bg-gray-50 text-gray-500 dark:bg-gray-900 dark:text-gray-400">
            <tr>
              <th class="px-3 py-2 text-left font-medium">入口类型</th>
              <th class="px-3 py-2 text-right font-medium">请求数</th>
              <th class="px-3 py-2 text-right font-medium" title="所有请求总耗时平均">平均总耗时</th>
              <th class="px-3 py-2 text-right font-medium" title="P95 总耗时">P95 总耗时</th>
              <th class="px-3 py-2 text-right font-medium">慢请求占比</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-gray-200 dark:divide-gray-700">
            <tr v-for="a in overview.access_type_breakdown" :key="a.access_type">
              <td class="px-3 py-2 text-gray-700 dark:text-gray-200">{{ a.access_type || '(未知)' }}</td>
              <td class="px-3 py-2 text-right font-mono">{{ a.requests.toLocaleString() }}</td>
              <td class="px-3 py-2 text-right font-mono">{{ fmt(a.avg_total_ms) }}</td>
              <td class="px-3 py-2 text-right font-mono">{{ fmt(a.p95_total_ms) }}</td>
              <td class="px-3 py-2 text-right font-mono" :class="a.slow_ratio > 0.05 ? 'text-orange-600 font-bold' : ''">
                {{ (a.slow_ratio * 100).toFixed(1) }}%
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- 用户延迟排行 -->
      <div class="rounded-2xl border border-gray-200 bg-white p-4 dark:border-gray-700 dark:bg-gray-800">
        <div class="mb-3 flex items-center justify-between flex-wrap gap-3">
          <h2 class="font-semibold text-gray-900 dark:text-white">
            用户延迟汇总(按请求数倒序)
            <span class="ml-2 text-xs font-normal text-gray-500">共 {{ userStats.length }} 条</span>
          </h2>
          <div class="flex items-center gap-2 text-xs">
            <span class="text-gray-500">模型:</span>
            <select v-model="modelFilter" @change="refresh"
                    class="rounded border border-gray-300 px-2 py-1 dark:border-gray-600 dark:bg-gray-800 max-w-[200px]"
                    title="按 model 字段精确过滤(取最近 7 天出现过的)。作用于本页所有区块。">
              <option value="">全部</option>
              <option v-for="m in modelOptions" :key="m" :value="m">{{ m }}</option>
            </select>
          </div>
        </div>
        <div class="overflow-x-auto">
          <table class="min-w-full text-sm">
            <thead class="bg-gray-50 text-gray-500 dark:bg-gray-900 dark:text-gray-400">
              <tr>
                <th class="px-3 py-2 text-left font-medium">用户</th>
                <th class="px-3 py-2 text-right font-medium" title="该用户在选中时间窗 + 入口/模型 过滤下产生的 usage_log 行数(每次成功的 API 调用 = 1 行)。不是 token 也不是费用。">请求数</th>
                <th class="px-3 py-2 text-right font-medium" title="服务器处理 T2-T1">平均·服务器</th>
                <th class="px-3 py-2 text-right font-medium" title="上游 TTFB+流式 = T4-T2">平均·上游</th>
                <th class="px-3 py-2 text-right font-medium" title="交付 T5-T4 ≈ 该用户客户端网络">平均·交付</th>
                <th class="px-3 py-2 text-right font-medium" title="总耗时 T5-T1">平均·总</th>
                <th class="px-3 py-2 text-right font-medium" title="该用户 95% 请求总耗时 ≤ 此值">P95·总</th>
                <th class="px-3 py-2 text-right font-medium" title="超阈值的请求占比">慢占比</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-200 dark:divide-gray-700">
              <tr v-if="!userStats.length"><td colspan="8" class="px-3 py-6 text-center text-gray-400">当前时间窗口暂无数据</td></tr>
              <tr v-for="u in userPaged" :key="u.user_id" class="hover:bg-gray-50 dark:hover:bg-gray-700">
                <td class="px-3 py-2 text-gray-700 dark:text-gray-200">
                  <div class="font-medium">{{ u.username || `用户#${u.user_id}` }}</div>
                  <div class="text-xs text-gray-400" :title="u.email">{{ u.email || '-' }}</div>
                </td>
                <td class="px-3 py-2 text-right font-mono">{{ u.requests.toLocaleString() }}</td>
                <td class="px-3 py-2 text-right font-mono text-red-600 dark:text-red-400">{{ fmt(u.avg_server_ms) }}</td>
                <td class="px-3 py-2 text-right font-mono text-orange-600 dark:text-orange-400">{{ fmt(u.avg_upstream_ms) }}</td>
                <td class="px-3 py-2 text-right font-mono text-blue-600 dark:text-blue-400">{{ fmt(u.avg_delivery_ms) }}</td>
                <td class="px-3 py-2 text-right font-mono font-bold">{{ fmt(u.avg_total_ms) }}</td>
                <td class="px-3 py-2 text-right font-mono">{{ fmt(u.p95_total_ms) }}</td>
                <td class="px-3 py-2 text-right font-mono" :class="u.slow_ratio > 0.1 ? 'text-orange-600 font-bold' : ''">
                  {{ (u.slow_ratio * 100).toFixed(1) }}%
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <Pagination
          v-if="userStats.length"
          class="mt-3 -mx-4 -mb-4 rounded-b-2xl"
          :total="userStats.length"
          :page="userPage"
          :page-size="userPageSize"
          @update:page="(p: number) => userPage = p"
          @update:page-size="(s: number) => { userPageSize = s; userPage = 1 }"
        />
      </div>

      <!-- 趋势图 -->
      <div class="rounded-2xl border border-gray-200 bg-white p-4 dark:border-gray-700 dark:bg-gray-800">
        <div class="mb-3 flex items-center justify-between">
          <h2 class="font-semibold text-gray-900 dark:text-white">延迟趋势</h2>
          <select v-model="trendBucket" @change="refresh" class="rounded border px-2 py-1 text-xs dark:border-gray-600 dark:bg-gray-700" title="时间桶大小">
            <option value="hour">按小时</option>
            <option value="day">按天</option>
          </select>
        </div>
        <div class="h-64">
          <Line v-if="trendChartData" :data="trendChartData" :options="trendChartOptions" />
          <div v-else class="flex h-full items-center justify-center text-sm text-gray-400">当前时间窗口暂无数据</div>
        </div>
      </div>

      <!-- 地区延迟排行 -->
      <div class="rounded-2xl border border-gray-200 bg-white p-4 dark:border-gray-700 dark:bg-gray-800">
        <h2 class="mb-3 font-semibold text-gray-900 dark:text-white">
          地区延迟排行(按 平均交付时间 降序)
          <span class="ml-2 text-xs font-normal text-gray-500">共 {{ regionStats.length }} 条</span>
        </h2>
        <div class="overflow-x-auto">
          <table class="min-w-full text-sm">
            <thead class="bg-gray-50 text-gray-500 dark:bg-gray-900 dark:text-gray-400">
              <tr>
                <th class="px-3 py-2 text-left font-medium">国家</th>
                <th class="px-3 py-2 text-left font-medium">省/区</th>
                <th class="px-3 py-2 text-right font-medium">请求数</th>
                <th class="px-3 py-2 text-right font-medium" title="T5-T4 平均值,≈ 客户端→sub2api 网络耗时">平均·交付</th>
                <th class="px-3 py-2 text-right font-medium" title="P95 交付">P95·交付</th>
                <th class="px-3 py-2 text-right font-medium">平均·总</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-200 dark:divide-gray-700">
              <tr v-if="!regionStats.length"><td colspan="6" class="px-3 py-6 text-center text-gray-400">当前时间窗口无 GeoIP 解析结果(可能是 mmdb 未就绪 或 IP 多为内网)</td></tr>
              <tr v-for="(r, i) in regionPaged" :key="i">
                <td class="px-3 py-2 text-gray-700 dark:text-gray-200">
                  <span class="mr-1">{{ regionFlag(r.country) }}</span>
                  <span class="font-mono">{{ r.country || '-' }}</span>
                  <span v-if="countryName(r.country)" class="text-gray-500"> / {{ countryName(r.country) }}</span>
                </td>
                <td class="px-3 py-2 text-gray-700 dark:text-gray-200">
                  <template v-if="r.region">
                    {{ r.region }}
                    <span v-if="regionZh(r.region)" class="text-gray-500"> / {{ regionZh(r.region) }}</span>
                  </template>
                  <span v-else class="text-gray-400">-</span>
                </td>
                <td class="px-3 py-2 text-right font-mono">{{ r.requests.toLocaleString() }}</td>
                <td class="px-3 py-2 text-right font-mono">{{ fmt(r.avg_delivery_ms) }}</td>
                <td class="px-3 py-2 text-right font-mono">{{ fmt(r.p95_delivery_ms) }}</td>
                <td class="px-3 py-2 text-right font-mono">{{ fmt(r.avg_total_ms) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
        <Pagination
          v-if="regionStats.length"
          class="mt-3 -mx-4 -mb-4 rounded-b-2xl"
          :total="regionStats.length"
          :page="regionPage"
          :page-size="regionPageSize"
          @update:page="(p: number) => regionPage = p"
          @update:page-size="(s: number) => { regionPageSize = s; regionPage = 1 }"
        />
      </div>

      <!-- 慢请求 / Top N -->
      <div class="rounded-2xl border border-gray-200 bg-white p-4 dark:border-gray-700 dark:bg-gray-800">
        <div class="mb-3 flex items-center justify-between flex-wrap gap-2">
          <h2 class="font-semibold text-gray-900 dark:text-white">
            {{ slowMode === 'slow' ? '违反阈值的慢请求' : '最耗时请求' }}
            <span class="ml-2 text-xs font-normal text-gray-500">共 {{ slowRows.length }} 条 · 点击行查看 5 段瀑布图</span>
          </h2>
          <select v-model="slowMode" @change="refresh" class="rounded border px-2 py-1 text-xs dark:border-gray-600 dark:bg-gray-700" title="慢请求口径">
            <option value="top">Top N(按总耗时倒序)</option>
            <option value="slow">超阈值(违规)</option>
          </select>
        </div>
        <div class="overflow-x-auto">
          <table class="min-w-full text-xs">
            <thead class="bg-gray-50 text-gray-500 dark:bg-gray-900 dark:text-gray-400">
              <tr>
                <th class="px-2 py-2 text-left font-medium">时间</th>
                <th class="px-2 py-2 text-left font-medium">用户/Key</th>
                <th class="px-2 py-2 text-left font-medium">模型</th>
                <th class="px-2 py-2 text-left font-medium">地区</th>
                <th class="px-2 py-2 text-left font-medium">入口</th>
                <th class="px-2 py-2 text-right font-medium" title="T2-T1 服务器处理">服务器</th>
                <th class="px-2 py-2 text-right font-medium" title="T3-T2 上游首字节">TTFB</th>
                <th class="px-2 py-2 text-right font-medium" title="T4-T3 流式包">流式</th>
                <th class="px-2 py-2 text-right font-medium" title="T5-T4 交付">交付</th>
                <th class="px-2 py-2 text-right font-medium" title="T5-T1 总耗时">总</th>
                <th class="px-2 py-2 text-left font-medium">主因</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-200 dark:divide-gray-700">
              <tr v-if="!slowRows.length"><td colspan="11" class="px-3 py-6 text-center text-gray-400">当前时间窗口暂无数据</td></tr>
              <tr v-for="r in slowPaged" :key="r.id"
                  @click="selectedRow = r"
                  :class="['cursor-pointer hover:bg-gray-50 dark:hover:bg-gray-700', isSlowRow(r) ? 'bg-orange-50/40 dark:bg-orange-900/10' : '']">
                <td class="px-2 py-1.5 text-gray-600 dark:text-gray-300">{{ fmtTime(r.created_at) }}</td>
                <td class="px-2 py-1.5">{{ r.user_id }}/{{ r.api_key_id }}</td>
                <td class="px-2 py-1.5">{{ r.model }}</td>
                <td class="px-2 py-1.5">
                  <span v-if="r.client_country">{{ regionFlag(r.client_country) }} {{ r.client_country }}</span>
                  <span v-else class="text-gray-400">-</span>
                </td>
                <td class="px-2 py-1.5">{{ r.access_type || '-' }}</td>
                <td class="px-2 py-1.5 text-right font-mono">{{ fmt(r.server_processing_ms) }}</td>
                <td class="px-2 py-1.5 text-right font-mono">{{ fmt(r.upstream_ttfb_ms) }}</td>
                <td class="px-2 py-1.5 text-right font-mono">{{ fmt(r.upstream_stream_ms) }}</td>
                <td class="px-2 py-1.5 text-right font-mono">{{ fmt(r.response_delivery_ms) }}</td>
                <td class="px-2 py-1.5 text-right font-mono font-bold">{{ fmt(r.total_latency_ms) }}</td>
                <td class="px-2 py-1.5">
                  <span class="inline-block rounded px-1.5 py-0.5 text-[10px]" :class="bottleneckColor(r.bottleneck)">
                    {{ bottleneckLabel(r.bottleneck) }}
                  </span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <Pagination
          v-if="slowRows.length"
          class="mt-3 -mx-4 -mb-4 rounded-b-2xl"
          :total="slowRows.length"
          :page="slowPage"
          :page-size="slowPageSize"
          @update:page="(p: number) => slowPage = p"
          @update:page-size="(s: number) => { slowPageSize = s; slowPage = 1 }"
        />
      </div>
    </div>

    <!-- 瀑布图弹窗 -->
    <div v-if="selectedRow" class="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4" @click="selectedRow = null">
      <div class="w-full max-w-2xl rounded-2xl bg-white p-6 dark:bg-gray-800" @click.stop>
        <div class="mb-4 flex items-center justify-between">
          <h3 class="text-lg font-semibold">请求 #{{ selectedRow.id }} 的延迟瀑布图</h3>
          <button @click="selectedRow = null" class="text-gray-400 hover:text-gray-600">✕</button>
        </div>
        <div class="space-y-2 text-sm">
          <div class="text-gray-500">{{ fmtTime(selectedRow.created_at) }} · {{ selectedRow.model }}</div>
          <div class="text-gray-500">
            入口: {{ selectedRow.access_type || '-' }} ·
            地区: {{ selectedRow.client_country ? regionFlag(selectedRow.client_country) + ' ' + selectedRow.client_country : '-' }}
            <span v-if="selectedRow.client_region">/ {{ selectedRow.client_region }}</span>
            · IP: {{ selectedRow.ip_address || '-' }}
          </div>
          <div class="mt-4 space-y-2">
            <div v-for="seg in waterfallSegments(selectedRow)" :key="seg.label" class="flex items-center gap-3">
              <div class="w-32 text-xs text-gray-600 dark:text-gray-300">{{ seg.label }}</div>
              <div class="flex-1 overflow-hidden rounded bg-gray-100 dark:bg-gray-700">
                <div :class="seg.color" class="h-6" :style="{ width: seg.pct + '%' }"></div>
              </div>
              <div class="w-20 text-right font-mono text-xs">{{ fmt(seg.ms) }}</div>
            </div>
          </div>
          <div class="mt-4 border-t pt-3 dark:border-gray-700">
            <div class="text-sm">
              <strong>总耗时:</strong> {{ fmt(selectedRow.total_latency_ms) }}
              <span class="ml-2 inline-block rounded px-2 py-0.5 text-xs" :class="bottleneckColor(selectedRow.bottleneck)">
                主因: {{ bottleneckLabel(selectedRow.bottleneck) }}
              </span>
            </div>
            <div v-if="selectedRow.bottleneck === 'delivery_slow'" class="mt-2 text-xs text-blue-600 dark:text-blue-300">
              💡 推测客户端网络较差。建议结合「地区延迟排行」交叉验证是否有地域性规律。
            </div>
          </div>
        </div>
      </div>
    </div>
  </AppLayout>
</template>
