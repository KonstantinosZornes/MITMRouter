<script setup lang="ts">
import { onMounted, onUnmounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { useI18n } from 'vue-i18n'
import { api } from '../api'
import { errText } from '../i18n'

const { t } = useI18n()
interface LogRow { id:number, ts:number, req_id:string, method:string, host:string, path:string, status:number, dur_ms:number, ttfb_ms?:number, bytes_out:number, has_marker:boolean, account?:string, account_fp:string, upstream:string, internal_error?:string }

const rows = ref<LogRow[]>([]), total = ref(0)
const q = reactive({ range: '24h', q: '', account: '', upstream: '', cls: '', page: 1, page_size: 50 })
const auto = ref(false); let timer: any

function rangeMs(): { from: number; to: number } {
  const now = Date.now()
  const map: Record<string, number> = { '1h': 3600e3, '24h': 86400e3, '7d': 7 * 86400e3 }
  if (q.range === 'all') return { from: 0, to: 0 }
  return { from: now - (map[q.range] ?? 86400e3), to: 0 }
}
async function load() {
  const { from, to } = rangeMs()
  const qs = new URLSearchParams()
  if (from) qs.set('from', String(from)); if (to) qs.set('to', String(to))
  if (q.q) qs.set('q', q.q); if (q.account) qs.set('account', q.account)
  if (q.upstream) qs.set('upstream', q.upstream); if (q.cls) qs.set('class', q.cls)
  qs.set('page', String(q.page)); qs.set('page_size', String(q.page_size))
  try {
    const r = await api<{ items: LogRow[]; total: number }>('/api/logs?' + qs.toString())
    rows.value = r.items; total.value = r.total
  } catch (e: any) { ElMessage.error(errText(e)) }
}
function ts(v: number) { return new Date(v).toLocaleString(undefined, { hour12: false }) }
function color(s: number) { return s === 0 ? 'info' : s >= 500 ? 'danger' : s >= 400 ? 'warning' : 'success' }
function statusText(s: number) { return s === 0 ? '—' : String(s) }
function formatLatencyMS(ttfbMS?: number) { return ttfbMS === undefined ? '—' : `${ttfbMS}ms` }

async function clearAll() {
  try {
    await ElMessageBox.confirm(t('audit.clearConfirm'), t('common.confirm'), { type: 'warning' })
  } catch { return }
  try {
    await api('/api/logs', { method: 'DELETE' }); load(); ElMessage.success(t('audit.cleared'))
  } catch (e: any) { ElMessage.error(errText(e)) }
}
onMounted(load)
onUnmounted(() => clearInterval(timer))
function toggleAuto(v: boolean) {
  clearInterval(timer)
  if (v) timer = setInterval(load, 5000)
}
</script>

<template>
  <el-card shadow="never" class="page">
  <el-form inline class="toolbar">
    <el-form-item :label="t('audit.time')">
      <el-select v-model="q.range" style="width:130px" @change="q.page=1;load()">
        <el-option value="1h" :label="t('audit.range1h')" /><el-option value="24h" :label="t('audit.range24h')" />
        <el-option value="7d" :label="t('audit.range7d')" /><el-option value="all" :label="t('audit.rangeAll')" />
      </el-select>
    </el-form-item>
    <el-form-item :label="t('audit.kw')"><el-input v-model="q.q" :placeholder="t('audit.kwPh')" clearable @keyup.enter="q.page=1;load()" /></el-form-item>
    <el-form-item :label="t('audit.accountFp')"><el-input v-model="q.account" clearable @keyup.enter="q.page=1;load()" /></el-form-item>
    <el-form-item :label="t('audit.upstreamCol')"><el-input v-model="q.upstream" clearable style="width:120px" @keyup.enter="q.page=1;load()" /></el-form-item>
    <el-form-item :label="t('audit.status')">
      <el-select v-model="q.cls" clearable style="width:110px" @change="q.page=1;load()">
        <el-option value="2xx" label="2xx" /><el-option value="4xx" label="4xx" />
        <el-option value="5xx" label="5xx" /><el-option value="err" label="error" />
      </el-select>
    </el-form-item>
    <el-form-item>
      <el-button type="primary" @click="q.page=1;load()">{{ t('common.query') }}</el-button>
      <el-button @click="clearAll">{{ t('common.clear') }}</el-button>
      <el-switch v-model="auto" active-text="" style="margin-left:10px" @change="toggleAuto" />
    </el-form-item>
  </el-form>

  <el-table :data="rows" size="small" stripe>
    <el-table-column :label="t('audit.colTime')" width="145">
      <template #default="{ row }">{{ ts(row.ts) }}</template>
    </el-table-column>
    <el-table-column :label="t('audit.colReqID')" prop="req_id" width="110" show-overflow-tooltip />
    <el-table-column :label="t('audit.colMethod')" prop="method" width="65" />
    <el-table-column :label="t('audit.colTarget')" min-width="150">
      <template #default="{ row }">{{ row.host }}<span style="color:#999">{{ row.path }}</span></template>
    </el-table-column>
    <el-table-column :label="t('audit.colStatus')" width="70">
      <template #default="{ row }"><el-tag :type="color(row.status)" size="small">{{ statusText(row.status) }}</el-tag></template>
    </el-table-column>
    <el-table-column :label="t('audit.colDur')" width="75">
      <template #default="{ row }">{{ row.dur_ms }}ms</template>
    </el-table-column>
    <el-table-column :label="t('audit.colTTFB')" width="95">
      <template #default="{ row }">{{ formatLatencyMS(row.ttfb_ms) }}</template>
    </el-table-column>
    <el-table-column :label="t('audit.colBytes')" width="75">
      <template #default="{ row }">{{ row.bytes_out }}B</template>
    </el-table-column>
    <el-table-column :label="t('audit.colMarker')" width="60">
      <template #default="{ row }">{{ row.has_marker ? '✓' : '-' }}</template>
    </el-table-column>
    <el-table-column :label="t('audit.colAccount')" width="130" show-overflow-tooltip>
      <template #default="{ row }">{{ row.account || row.account_fp }}</template>
    </el-table-column>
    <el-table-column :label="t('audit.colHash')" width="80">
      <template #default="{ row }">
        <span class="fp" :title="row.account_fp">{{ (row.account_fp || '').slice(-5) }}</span>
      </template>
    </el-table-column>
    <el-table-column :label="t('audit.colUpstream')" prop="upstream" width="100" show-overflow-tooltip />
    <el-table-column :label="t('audit.colInternalError')" width="145" show-overflow-tooltip>
      <template #default="{ row }">{{ row.internal_error || '—' }}</template>
    </el-table-column>
  </el-table>
  <el-pagination style="margin-top:12px" layout="total, prev, pager, next, sizes"
    :total="total" :current-page="q.page" :page-size="q.page_size"
    @current-change="(p:number)=>{q.page=p;load()}" @size-change="(s:number)=>{q.page_size=s;q.page=1;load()}" />
  </el-card>
</template>

<style scoped>
.fp { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 12px; color: #475569; }
</style>
