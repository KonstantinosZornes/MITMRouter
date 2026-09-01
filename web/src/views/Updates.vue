<script setup lang="ts">
// 更新记录（docs/013-update-log-design.md）：acct_map 变更事件流水。
// 交互与 Audit.vue 保持一致：时间范围筛选、自动刷新、清空、分页。
import { onMounted, onUnmounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { useI18n } from 'vue-i18n'
import { api } from '../api'
import { errText } from '../i18n'

const { t } = useI18n()
interface UpdateRow { id:number, ts:number, kind:string, source:string, status:string, summary:string, detail:string }

const KINDS = ['direct_file', 'direct_incremental', 'api_sync', 'push', 'delete']
const rows = ref<UpdateRow[]>([]), total = ref(0)
const q = reactive({ range: '24h', kind: '', status: '', source: '', page: 1, page_size: 50 })
const auto = ref(false); let timer: any
const sourceNames = ref<Record<string, string>>({})

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
  if (q.kind) qs.set('kind', q.kind); if (q.status) qs.set('status', q.status)
  if (q.source) qs.set('source', q.source)
  qs.set('page', String(q.page)); qs.set('page_size', String(q.page_size))
  try {
    const r = await api<{ items: UpdateRow[]; total: number }>('/api/updates?' + qs.toString())
    rows.value = r.items; total.value = r.total
  } catch (e: any) { ElMessage.error(errText(e)) }
}
async function loadSources() {
  try {
    const r = await api<{ items: { id:number, name:string }[] }>('/api/sources')
    const m: Record<string, string> = {}
    for (const s of r.items ?? []) m['src:' + s.id] = s.name
    sourceNames.value = m
  } catch { /* 来源列表失败不阻塞记录展示，回退显示 src:<id> */ }
}
function ts(v: number) { return new Date(v).toLocaleString(undefined, { hour12: false }) }
function kindLabel(k: string) { return KINDS.includes(k) ? t('updates.k_' + k) : k }
function sourceLabel(s: string) { return sourceNames.value[s] ?? s }
function tagType(st: string) { return st === 'error' ? 'danger' : 'success' }

async function clearAll() {
  try {
    await ElMessageBox.confirm(t('updates.clearConfirm'), t('common.confirm'), { type: 'warning' })
  } catch { return }
  try {
    await api('/api/updates', { method: 'DELETE' }); load(); ElMessage.success(t('updates.cleared'))
  } catch (e: any) { ElMessage.error(errText(e)) }
}
onMounted(() => { load(); loadSources() })
onUnmounted(() => clearInterval(timer))
function toggleAuto(v: boolean) {
  clearInterval(timer)
  if (v) timer = setInterval(load, 5000)
}
</script>

<template>
  <el-card shadow="never" class="page">
  <el-form inline class="toolbar">
    <el-form-item :label="t('updates.time')">
      <el-select v-model="q.range" style="width:130px" @change="q.page=1;load()">
        <el-option value="1h" :label="t('updates.range1h')" /><el-option value="24h" :label="t('updates.range24h')" />
        <el-option value="7d" :label="t('updates.range7d')" /><el-option value="all" :label="t('updates.rangeAll')" />
      </el-select>
    </el-form-item>
    <el-form-item :label="t('updates.kind')">
      <el-select v-model="q.kind" clearable style="width:150px" @change="q.page=1;load()">
        <el-option v-for="k in KINDS" :key="k" :value="k" :label="t('updates.k_' + k)" />
      </el-select>
    </el-form-item>
    <el-form-item :label="t('updates.status')">
      <el-select v-model="q.status" clearable style="width:110px" @change="q.page=1;load()">
        <el-option value="ok" :label="t('updates.st_ok')" /><el-option value="error" :label="t('updates.st_error')" />
      </el-select>
    </el-form-item>
    <el-form-item :label="t('updates.source')">
      <el-select v-model="q.source" clearable filterable style="width:180px" @change="q.page=1;load()">
        <el-option v-for="(name, s) in sourceNames" :key="s" :value="s" :label="name" />
      </el-select>
    </el-form-item>
    <el-form-item>
      <el-button type="primary" @click="q.page=1;load()">{{ t('common.query') }}</el-button>
      <el-button @click="clearAll">{{ t('common.clear') }}</el-button>
      <el-switch v-model="auto" active-text="" style="margin-left:10px" @change="toggleAuto" />
    </el-form-item>
  </el-form>

  <el-table :data="rows" size="small" stripe>
    <el-table-column :label="t('updates.time')" width="145">
      <template #default="{ row }">{{ ts(row.ts) }}</template>
    </el-table-column>
    <el-table-column :label="t('updates.kind')" width="120">
      <template #default="{ row }">{{ kindLabel(row.kind) }}</template>
    </el-table-column>
    <el-table-column :label="t('updates.source')" width="150" show-overflow-tooltip>
      <template #default="{ row }">{{ sourceLabel(row.source) || '—' }}</template>
    </el-table-column>
    <el-table-column :label="t('updates.status')" width="70">
      <template #default="{ row }">
        <el-tag :type="tagType(row.status)" size="small">{{ row.status === 'error' ? t('updates.st_error') : t('updates.st_ok') }}</el-tag>
      </template>
    </el-table-column>
    <el-table-column :label="t('updates.summary')" min-width="200">
      <template #default="{ row }">{{ row.summary }}</template>
    </el-table-column>
    <el-table-column :label="t('updates.detail')" width="180" show-overflow-tooltip>
      <template #default="{ row }"><span class="detail">{{ row.detail || '—' }}</span></template>
    </el-table-column>
  </el-table>
  <el-pagination style="margin-top:12px" layout="total, prev, pager, next, sizes"
    :total="total" :current-page="q.page" :page-size="q.page_size"
    @current-change="(p:number)=>{q.page=p;load()}" @size-change="(s:number)=>{q.page_size=s;q.page=1;load()}" />
  </el-card>
</template>

<style scoped>
.detail { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 12px; color: #475569; }
</style>
