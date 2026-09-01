<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Refresh, Plus, Search } from '@element-plus/icons-vue'
import { useI18n } from 'vue-i18n'
import { api } from '../api'
import { errText } from '../i18n'

const { t } = useI18n()
// ---------- 源列表 ----------
interface SourceDTO {
  id: number; kind: string; name: string; base_url: string
  direct_auth_dir?: string; direct_db_configured?: boolean
  interval_s: number; enabled: boolean
  last_sync_at: number; last_status: string
}
const sources = ref<SourceDTO[]>([])
const testing = ref<Record<number, string>>({})
const testingIncr = ref<Record<number, string>>({})
const syncing = ref<Record<number, boolean>>({})

// hasIncr：是否配置了增量路径（cpa=认证文件目录；sub2api=已保存 DSN）
const hasIncr = (row: SourceDTO) => row.kind === 'cpa' ? !!row.direct_auth_dir : !!row.direct_db_configured

async function loadSources() {
  const r = await api<{ items: SourceDTO[] }>('/api/sources')
  sources.value = r.items
}

// ---------- 映射预览 ----------
interface MapDTO {
  platform: string; account: string
  at_fp: string; rt_fp: string
  at_hint: string; rt_hint: string
  source: string; source_type: string
  updated_at: number
}
interface StatsDTO {
  total: number; accounts: number
  by_platform: Record<string, number>
  by_source: Record<string, number>
}
const rows = ref<MapDTO[]>([])
const total = ref(0)
const stats = ref<StatsDTO | null>(null)
const filterPlatform = ref('')
const filterAccount = ref('')
const filterSourceType = ref('')
const filterBinding = ref('')
const page = ref(1)
const pageSize = ref(20)

// 源类型展示名：kind 存储值(cpa/sub2api) → 产品全名；其余原样展示（可扩展）。
const kindLabel = (k: string) => k === 'cpa' ? 'CLIProxyAPI' : k === 'sub2api' ? 'Sub2API' : k

// 来源实例的辅助标签：'api'=手动登记，否则映射为同步源名称。
const instanceLabel = (s: string) => {
  if (s === 'api') return t('acctmap.manualSource')
  const id = Number(s.slice(4))
  return sources.value.find(x => x.id === id)?.name || s
}

async function loadMap() {
  const q = new URLSearchParams()
  if (filterPlatform.value) q.set('platform', filterPlatform.value)
  if (filterAccount.value) q.set('account', filterAccount.value)
  if (filterSourceType.value) q.set('source_type', filterSourceType.value)
  if (filterBinding.value) q.set('binding', filterBinding.value)
  q.set('page', String(page.value))
  q.set('page_size', String(pageSize.value))
  const r = await api<{ items: MapDTO[]; total: number }>(`/api/acctmap?${q}`)
  rows.value = r.items
  total.value = r.total
}
async function loadStats() {
  stats.value = await api<StatsDTO>('/api/acctmap/stats')
}
async function loadAll() {
  await Promise.all([loadSources(), loadMap(), loadStats(), loadBindings()])
}
onMounted(loadAll)

// ---------- 出站绑定（账户方向，docs/011） ----------
interface BindingDTO {
  platform: string; account: string; mode: string
  egress_ids: number[]; egress_names: string[]
}
interface EgressUp { id: number; name: string; platform: string; enabled: boolean }
const KS = '\u241F'
const bindings = ref<Map<string, BindingDTO>>(new Map())
const egressUps = ref<EgressUp[]>([])

const bindKey = (platform: string, account: string) => `${platform}${KS}${account}`
const bindingOf = (row: { platform: string; account: string }) =>
  bindings.value.get(bindKey(row.platform, row.account)) || null

async function clearAllBindings() {
  try {
    await ElMessageBox.confirm(
      t('acctmap.clearBindingsConfirm', { n: bindings.value.size }),
      t('common.confirm'), { type: 'warning' })
  } catch { return }
  try {
    await api('/api/acctegress', { method: 'DELETE' })
    await Promise.all([loadBindings(), loadMap()])
    ElMessage.success(t('common.saved'))
  } catch (e: any) { ElMessage.error(errText(e)) }
}

async function loadBindings() {
  const [egRes, upRes] = await Promise.all([
    api<{ items: BindingDTO[] }>('/api/acctegress'),
    api<{ items: EgressUp[] }>('/api/upstreams'),
  ])
  const m = new Map<string, BindingDTO>()
  for (const b of egRes.items) m.set(bindKey(b.platform, b.account), b)
  bindings.value = m
  egressUps.value = upRes.items.filter(u => u.platform === 'plain')
}

const bindDialog = ref(false)
const bindRow = ref<{ platform: string; account: string } | null>(null)
const bindMode = ref('sticky')
const bindFilter = ref('')
const selectedIds = ref(new Set<number>())
const visibleEgress = computed(() => {
  const k = bindFilter.value.trim().toLowerCase()
  if (!k) return egressUps.value
  return egressUps.value.filter(u => u.name.toLowerCase().includes(k))
})
const isCheckedId = (u: EgressUp) => selectedIds.value.has(u.id)
const checkedVisibleCount = () => visibleEgress.value.filter(isCheckedId).length
const allVisibleChecked = computed(() =>
  visibleEgress.value.length > 0 && checkedVisibleCount() === visibleEgress.value.length)
const someVisibleChecked = computed(() => {
  const n = checkedVisibleCount()
  return n > 0 && n < visibleEgress.value.length
})
function mutateIds(fn: (s: Set<number>) => void) {
  const s = new Set(selectedIds.value)
  fn(s)
  selectedIds.value = s
}
const toggleRowId = (u: EgressUp, on: boolean) =>
  mutateIds(s => (on ? s.add(u.id) : s.delete(u.id)))
const selectAllVisible = () => mutateIds(s => visibleEgress.value.forEach(u => s.add(u.id)))
const invertVisible = () => mutateIds(s => visibleEgress.value.forEach(u => {
  const has = s.has(u.id); has ? s.delete(u.id) : s.add(u.id)
}))
const selectNone = () => mutateIds(s => visibleEgress.value.forEach(u => s.delete(u.id)))

function openBind(row: { platform: string; account: string }) {
  bindRow.value = { ...row }
  bindFilter.value = ''
  const cur = bindingOf(row)
  bindMode.value = cur?.mode === 'random' ? 'random' : 'sticky'
  selectedIds.value = new Set(cur?.egress_ids ?? [])
  bindDialog.value = true
}
async function saveBind() {
  if (!bindRow.value) return
  try {
    await api(`/api/acctegress/${encodeURIComponent(bindRow.value.platform)}/${encodeURIComponent(bindRow.value.account)}`,
      { method: 'PUT', body: { mode: bindMode.value, egress_ids: [...selectedIds.value] } })
    bindDialog.value = false
    await loadBindings()
    ElMessage.success(t('common.saved'))
  } catch (e: any) { ElMessage.error(errText(e)) }
}

// ---------- 源表单 ----------
const dialog = ref(false)
const editing = ref<number | null>(null)
const form = reactive<{
  id: number|null; kind: string; name: string
  base_url: string; api_key: string; direct_db_dsn: string; direct_auth_dir: string
  interval_s: number; enabled: boolean
}>({
  id: null, kind: 'sub2api', name: '', base_url: '', api_key: '',
  direct_db_dsn: '', direct_auth_dir: '', interval_s: 600, enabled: true,
})
const editDsnConfigured = ref(false) // 编辑的源已保存过增量 DSN
const clearDsn = ref(false)          // 勾选 = 提交 direct_db_clear，停用增量
function openCreate() {
  editing.value = null
  Object.assign(form, {
    id: null, kind: 'sub2api', name: '', base_url: '', api_key: '',
    direct_db_dsn: '', direct_auth_dir: '', interval_s: 600, enabled: true,
  })
  editDsnConfigured.value = false
  clearDsn.value = false
  dialog.value = true
}
function openEdit(row: SourceDTO) {
  editing.value = row.id
  Object.assign(form, {
    id: row.id, kind: row.kind, name: row.name,
    base_url: row.base_url, api_key: '', direct_db_dsn: '',
    direct_auth_dir: row.direct_auth_dir || '', interval_s: row.interval_s, enabled: row.enabled,
  })
  editDsnConfigured.value = row.kind === 'sub2api' && !!row.direct_db_configured
  clearDsn.value = false
  dialog.value = true
}
function onKindChange(kind: string) {
  if (kind === 'cpa') form.direct_db_dsn = ''
  if (kind === 'sub2api') form.direct_auth_dir = ''
}
async function save() {
  try {
    const body: Record<string, unknown> = { ...form }
    if (editing.value != null && form.kind === 'sub2api' && clearDsn.value) {
      body.direct_db_clear = true
      delete body.direct_db_dsn
    }
    if (editing.value == null) await api('/api/sources', { method: 'POST', body })
    else await api(`/api/sources/${form.id}`, { method: 'PUT', body })
    dialog.value = false; await loadSources(); ElMessage.success(t('common.saved'))
  } catch (e: any) { ElMessage.error(errText(e)) }
}
async function del(row: SourceDTO) {
  try {
    await ElMessageBox.confirm(
      t('acctmap.delConfirm', { name: row.name }) + ' ' + t('acctmap.delCascade'),
      t('common.confirm'), { type: 'warning' })
  } catch { return }
  try { await api(`/api/sources/${row.id}`, { method: 'DELETE' }); await loadAll(); ElMessage.success(t('common.saved')) }
  catch (e: any) { ElMessage.error(errText(e)) }
}
async function test(row: SourceDTO) {
  testing.value[row.id] = t('common.loading')
  try {
    const r = await api<{ ok: boolean; summary?: string; error?: string }>(`/api/sources/${row.id}/test`, { method: 'POST' })
    testing.value[row.id] = r.ok ? (r.summary ?? '') : (t('acctmap.testFailed') + (r.error ?? '').slice(0, 60))
  } catch (e: any) { testing.value[row.id] = errText(e) }
}
async function testDirect(row: SourceDTO) {
  testingIncr.value[row.id] = t('common.loading')
  try {
    const r = await api<{ ok: boolean; summary?: string; error?: string }>(`/api/sources/${row.id}/test?target=incremental`, { method: 'POST' })
    testingIncr.value[row.id] = r.ok ? (r.summary ?? '') : (t('acctmap.testFailed') + (r.error ?? '').slice(0, 60))
  } catch (e: any) { testingIncr.value[row.id] = errText(e) }
}
async function syncNow(row: SourceDTO) {
  syncing.value[row.id] = true
  try {
    await api(`/api/sources/${row.id}/sync`, { method: 'POST' })
    ElMessage.success(t('acctmap.syncTriggered'))
    // 稍等拉取完成后刷新视图
    setTimeout(loadAll, 3000)
  } catch (e: any) { ElMessage.error(errText(e)) }
  finally { syncing.value[row.id] = false }
}

// ---------- 手动登记 ----------
const KNOWN_SOURCE_TYPES = ['CLIProxyAPI', 'Sub2API']
const pushDialog = ref(false)
const pushForm = reactive({ platform: 'openai', account: '', source_type: 'CLIProxyAPI', access_token: '', refresh_token: '' })
function openPush() {
  Object.assign(pushForm, { platform: 'openai', account: '', source_type: 'CLIProxyAPI', access_token: '', refresh_token: '' })
  pushDialog.value = true
}
async function doPush() {
  if (!pushForm.account || !pushForm.source_type.trim() ||
    (!pushForm.access_token.trim() && !pushForm.refresh_token.trim())) {
    ElMessage.warning(t('acctmap.pushIncomplete')); return
  }
  try {
    await api(`/api/acctmap/${encodeURIComponent(pushForm.platform)}/${encodeURIComponent(pushForm.account)}`,
      {
        method: 'PUT',
        body: {
          access_token: pushForm.access_token.trim(),
          refresh_token: pushForm.refresh_token.trim(),
          source_type: pushForm.source_type.trim(),
        },
      })
    pushDialog.value = false; await loadAll(); ElMessage.success(t('common.saved'))
  } catch (e: any) { ElMessage.error(errText(e)) }
}
async function delAccount(row: MapDTO) {
  try { await ElMessageBox.confirm(t('acctmap.delAcctConfirm', { acct: row.account }), t('common.confirm'), { type: 'warning' }) }
  catch { return }
  try {
    await api(`/api/acctmap/${encodeURIComponent(row.platform)}/${encodeURIComponent(row.account)}`, { method: 'DELETE' })
    await Promise.all([loadMap(), loadBindings()])
    ElMessage.success(t('common.saved'))
  } catch (e: any) { ElMessage.error(errText(e)) }
}

const statusText = (row: SourceDTO) => {
  if (!row.last_sync_at) {
    // 从未全量同步过的源（如缺 API 配置的存量直读源）：优先显示提示或增量错误
    return row.last_status ? row.last_status.slice(0, 60) : t('acctmap.neverSynced')
  }
  const d = new Date(row.last_sync_at).toLocaleString()
  return row.last_status.startsWith('ok')
    ? `${d} · ${t('acctmap.okPrefix')}${row.last_status.slice(3)}`
    : `${d} · ${row.last_status.slice(0, 60)}`
}
const statLines = computed(() => {
  if (!stats.value) return []
  const s = stats.value
  const lines = [
    `${t('acctmap.statTotal')}: ${s.total}`,
    `${t('acctmap.statAccounts')}: ${s.accounts}`,
  ]
  // 按来源类型全名分组统计（CLIProxyAPI / Sub2API / 自定义）
  const bySrc = Object.entries(s.by_source ?? {}).sort((a, b) => a[0].localeCompare(b[0]))
  for (const [k, v] of bySrc) lines.push(`${k}: ${v}`)
  return lines
})
</script>

<template>
  <el-card shadow="never" class="page">
    <div class="toolbar">
      <span class="stat" v-for="l in statLines" :key="l">{{ l }}</span>
      <span class="grow"></span>
      <el-button @click="loadAll" :icon="Refresh">{{ t('acctmap.refresh') }}</el-button>
      <el-button type="primary" @click="openPush">{{ t('acctmap.pushBtn') }}</el-button>
      <el-button type="primary" @click="openCreate" :icon="Plus">{{ t('acctmap.createSource') }}</el-button>
    </div>

    <h4>{{ t('acctmap.secSources') }}</h4>
    <el-table :data="sources" stripe border size="small">
      <el-table-column :label="t('acctmap.colName')" width="140">
        <template #default="{ row }"><b>{{ row.name }}</b></template>
      </el-table-column>
      <el-table-column :label="t('acctmap.colKind')" width="130">
        <template #default="{ row }">{{ kindLabel(row.kind) }}</template>
      </el-table-column>
      <el-table-column :label="t('acctmap.colSync')" width="130">
        <template #default="{ row }">
          <el-tag size="small" type="info">{{ t('acctmap.syncFull') }}</el-tag>
          <el-tag v-if="hasIncr(row)" size="small" type="success" style="margin-left:4px">{{ t('acctmap.syncIncremental') }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column :label="t('acctmap.colUrl')" min-width="200" show-overflow-tooltip>
        <template #default="{ row }">{{ row.base_url || '—' }}</template>
      </el-table-column>
      <el-table-column :label="t('acctmap.colIncrPath')" min-width="170" show-overflow-tooltip>
        <template #default="{ row }">
          <span v-if="row.kind === 'cpa'">{{ row.direct_auth_dir || '—' }}</span>
          <span v-else>{{ row.direct_db_configured ? t('acctmap.configured') : '—' }}</span>
        </template>
      </el-table-column>
      <el-table-column :label="t('acctmap.colInterval')" width="100">
        <template #default="{ row }">{{ `${row.interval_s}s` }}</template>
      </el-table-column>
      <el-table-column :label="t('acctmap.colEnabled')" width="90">
        <template #default="{ row }"><el-tag :type="row.enabled ? 'success' : 'info'" size="small">{{ row.enabled ? t('common.yes') : t('common.no') }}</el-tag></template>
      </el-table-column>
      <el-table-column :label="t('acctmap.colStatus')" min-width="220">
        <template #default="{ row }">
          <div style="font-size:12px;line-height:1.5">{{ statusText(row) }}</div>
          <el-button size="small" style="margin-top:2px" @click="test(row)">{{ t('common.testBtn') }}</el-button>
          <el-button v-if="hasIncr(row)" size="small" style="margin-top:2px" @click="testDirect(row)">{{ t('acctmap.testIncr') }}</el-button>
          <span v-if="testing[row.id]" style="margin-left:8px;font-size:12px">{{ testing[row.id] }}</span>
          <span v-if="testingIncr[row.id]" style="margin-left:8px;font-size:12px">{{ testingIncr[row.id] }}</span>
        </template>
      </el-table-column>
      <el-table-column :label="t('acctmap.colOps')" width="210">
        <template #default="{ row }">
          <el-button size="small" type="primary" plain :loading="!!syncing[row.id]" @click="syncNow(row)">
            {{ t('acctmap.syncNow') }}
          </el-button>
          <el-button size="small" link type="primary" @click="openEdit(row)">{{ t('common.edit') }}</el-button>
          <el-button size="small" link type="danger" @click="del(row)">{{ t('common.delete') }}</el-button>
        </template>
      </el-table-column>
    </el-table>

    <h4 style="margin-top:18px">{{ t('acctmap.secMap') }}</h4>
    <div class="toolbar" style="margin-bottom:8px">
      <el-select v-model="filterPlatform" clearable :placeholder="t('acctmap.colPlatform')" style="width:140px" @change="page = 1; loadMap()">
        <el-option v-for="(v, k) in stats?.by_platform ?? {}" :key="k" :value="k" :label="`${k} (${v})`" />
      </el-select>
      <el-select v-model="filterSourceType" clearable :placeholder="t('acctmap.colSource')" style="width:170px" @change="page = 1; loadMap()">
        <el-option v-for="(v, k) in stats?.by_source ?? {}" :key="k" :value="k" :label="`${k || '—'} (${v})`" />
      </el-select>
      <el-select v-model="filterBinding" clearable :placeholder="t('acctmap.colEgress')" style="width:120px" @change="page = 1; loadMap()">
        <el-option value="bound" :label="t('acctmap.bindingBound')" />
        <el-option value="unbound" :label="t('acctmap.bindingUnbound')" />
      </el-select>
      <el-input v-model="filterAccount" :placeholder="t('acctmap.filterAcctPh')" clearable
        style="width:220px" :prefix-icon="Search" @keyup.enter="page = 1; loadMap()" />
      <el-button @click="page = 1; loadMap()">{{ t('common.query') }}</el-button>
      <el-button type="danger" plain :disabled="bindings.size === 0" @click="clearAllBindings">
        {{ t('acctmap.clearBindings') }}
      </el-button>
    </div>
    <el-table :data="rows" stripe border size="small">
      <el-table-column :label="t('acctmap.colPlatform')" prop="platform" width="120" />
      <el-table-column :label="t('acctmap.colAccount')" prop="account" min-width="200" />
      <el-table-column :label="t('acctmap.colAtTail')" width="100">
        <template #default="{ row }">
          <span v-if="row.at_fp">{{ row.at_hint || '…' }}</span>
          <span v-else style="color:#cbd5e1">—</span>
        </template>
      </el-table-column>
      <el-table-column :label="t('acctmap.colRtTail')" width="100">
        <template #default="{ row }">
          <span v-if="row.rt_fp">{{ row.rt_hint || '…' }}</span>
          <span v-else style="color:#cbd5e1">—</span>
        </template>
      </el-table-column>
      <el-table-column :label="t('acctmap.colSource')" min-width="170">
        <template #default="{ row }">
          <el-tag size="small" :type="row.source === 'api' ? 'warning' : 'success'" style="margin-right:6px">
            {{ row.source_type || row.source }}
          </el-tag>
          <span style="font-size:12px;color:#94a3b8">{{ instanceLabel(row.source) }}</span>
        </template>
      </el-table-column>
      <el-table-column :label="t('acctmap.colEgress')" width="130">
        <template #default="{ row }">
          <el-tag v-if="bindingOf(row)" size="small" :type="bindingOf(row)!.mode === 'random' ? 'warning' : 'success'"
            style="cursor:pointer" @click="openBind(row)">
            {{ bindingOf(row)!.mode === 'random' ? t('acctmap.modeRandom') : t('acctmap.modeSticky') }} ·
            {{ bindingOf(row)!.egress_ids.length }}
          </el-tag>
          <span v-else style="color:#cbd5e1">—</span>
        </template>
      </el-table-column>
      <el-table-column :label="t('acctmap.colUpdatedAt')" width="160">
        <template #default="{ row }">
          <span v-if="row.updated_at" style="font-size:12px;color:#64748b">{{ new Date(row.updated_at).toLocaleString() }}</span>
          <span v-else style="color:#cbd5e1">—</span>
        </template>
      </el-table-column>
      <el-table-column :label="t('acctmap.colOps')" width="150">
        <template #default="{ row }">
          <el-button size="small" link type="primary" @click="openBind(row)">{{ t('acctmap.bindEgressShort') }}</el-button>
          <el-button size="small" link type="danger" @click="delAccount(row)">{{ t('common.delete') }}</el-button>
        </template>
      </el-table-column>
    </el-table>
    <el-pagination v-model:current-page="page" :page-size="pageSize" :total="total"
      layout="prev, pager, next, total" @current-change="loadMap()" style="margin-top:10px" />

    <el-dialog v-model="dialog" :title="editing == null ? t('acctmap.createSource') : t('acctmap.editSource')" width="600px">
      <el-form label-width="150px">
        <el-form-item :label="t('acctmap.kind')">
          <el-select v-model="form.kind" style="width:100%" @change="onKindChange">
            <el-option value="cpa" label="CLIProxyAPI" />
            <el-option value="sub2api" label="Sub2API" />
          </el-select>
        </el-form-item>
        <el-form-item :label="t('acctmap.name')"><el-input v-model="form.name" /></el-form-item>
        <el-divider content-position="left">{{ t('acctmap.fullSection') }}</el-divider>
        <el-form-item :label="t('acctmap.baseUrl')">
          <el-input v-model="form.base_url" placeholder="https://gw.example.com:9966" />
        </el-form-item>
        <el-form-item :label="t('acctmap.apiKey')">
          <el-input v-model="form.api_key" show-password
            :placeholder="editing == null ? (form.kind === 'cpa' ? 'management key' : 'admin api key (x-api-key)') : t('acctmap.keyKeepHint')" />
        </el-form-item>
        <el-form-item :label="t('acctmap.interval')">
          <el-input-number v-model="form.interval_s" :min="60" :step="1" />
        </el-form-item>
        <el-divider content-position="left">{{ t('acctmap.incrSection') }}</el-divider>
        <div class="hint" style="margin:-6px 0 10px">{{ t('acctmap.incrHint') }}</div>
        <el-form-item v-if="form.kind === 'cpa'" :label="t('acctmap.directAuthDir')">
          <el-input v-model="form.direct_auth_dir" placeholder="/opt/cliproxyapi/auths" />
          <div class="hint">{{ t('acctmap.directAuthDirHint') }}</div>
        </el-form-item>
        <template v-else>
          <el-form-item :label="t('acctmap.directDBDSN')">
            <el-input v-model="form.direct_db_dsn" type="password" show-password :disabled="clearDsn"
              :placeholder="editing == null || !editDsnConfigured ? 'postgres://user:password@host:5432/db?sslmode=verify-full' : t('acctmap.keyKeepHint')" />
            <div class="hint">{{ t('acctmap.directDBDSNHint') }}</div>
          </el-form-item>
          <el-form-item v-if="editing != null && editDsnConfigured" label-width="0">
            <el-checkbox v-model="clearDsn">{{ t('acctmap.clearDsn') }}</el-checkbox>
          </el-form-item>
        </template>
        <el-form-item :label="t('acctmap.enableSwitch')"><el-switch v-model="form.enabled" /></el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialog = false">{{ t('common.cancel') }}</el-button>
        <el-button type="primary" @click="save">{{ t('common.save') }}</el-button>
      </template>
    </el-dialog>

    <!-- 绑定出站：整体替换该账户的出站集合与模式 -->
    <el-dialog v-model="bindDialog" :title="t('acctmap.bindAcctTitle', { acct: bindRow?.account ?? '' })" width="560px">
      <div class="bind-block">
        <div class="bind-label">
          {{ t('acctmap.mode') }}
          <span class="bind-hint">{{ t('acctmap.bindModeHint') }}</span>
        </div>
        <el-radio-group v-model="bindMode">
          <el-radio-button value="sticky">{{ t('acctmap.modeSticky') }}</el-radio-button>
          <el-radio-button value="random">{{ t('acctmap.modeRandom') }}</el-radio-button>
        </el-radio-group>
      </div>

      <div class="bind-block">
        <div class="bind-toolbar">
          <el-input v-model="bindFilter" clearable :prefix-icon="Search"
            :placeholder="t('acctmap.bindSearchEgressPh')" style="flex:1" />
          <el-button :disabled="!visibleEgress.length" @click="selectAllVisible">{{ t('acctmap.selAll') }}</el-button>
          <el-button :disabled="!visibleEgress.length" @click="invertVisible">{{ t('acctmap.selInvert') }}</el-button>
          <el-button :disabled="!visibleEgress.length" @click="selectNone">{{ t('acctmap.selNone') }}</el-button>
          <span class="bind-cnt">{{ t('acctmap.selectedCount', { n: selectedIds.size }) }}</span>
        </div>
        <el-table v-if="egressUps.length" :data="visibleEgress" max-height="300" size="small" border stripe
          style="width:100%" @row-click="(row: any) => toggleRowId(row, !isCheckedId(row))">
          <el-table-column width="44" align="center">
            <template #header>
              <el-checkbox :model-value="allVisibleChecked" :indeterminate="someVisibleChecked"
                @change="(v: any) => (v ? selectAllVisible() : selectNone())" />
            </template>
            <template #default="{ row }">
              <el-checkbox :model-value="isCheckedId(row)" @change="(v: any) => toggleRowId(row, v)" @click.stop />
            </template>
          </el-table-column>
          <el-table-column prop="name" :label="t('acctmap.colName')" min-width="180">
            <template #default="{ row }"><b>{{ row.name }}</b></template>
          </el-table-column>
          <el-table-column :label="t('acctmap.colEnabled')" width="90" align="center">
            <template #default="{ row }">
              <el-tag size="small" :type="row.enabled ? 'success' : 'info'">{{ row.enabled ? t('common.yes') : t('common.no') }}</el-tag>
            </template>
          </el-table-column>
        </el-table>
        <div v-else class="bind-empty">{{ t('acctmap.noEgressHint') }}</div>
      </div>

      <template #footer>
        <el-button @click="bindDialog = false">{{ t('common.cancel') }}</el-button>
        <el-button type="primary" @click="saveBind">{{ t('common.save') }}</el-button>
      </template>
    </el-dialog>

    <el-dialog v-model="pushDialog" :title="t('acctmap.pushTitle')" width="560px">
      <el-form label-width="90px">
        <el-form-item :label="t('acctmap.colPlatform')">
          <el-select v-model="pushForm.platform" style="width:100%" filterable allow-create>
            <el-option v-for="p in ['openai','anthropic','gemini','grok','kimi','deepseek','glm','qwen','iflow','ollama']"
              :key="p" :value="p" :label="p" />
          </el-select>
        </el-form-item>
        <el-form-item :label="t('acctmap.account')">
          <el-input v-model="pushForm.account" placeholder="user@example.com" />
        </el-form-item>
        <el-form-item :label="t('acctmap.sourceType')">
          <el-select v-model="pushForm.source_type" style="width:100%" filterable allow-create
            default-first-option :placeholder="t('acctmap.sourceTypePh')">
            <el-option v-for="st in KNOWN_SOURCE_TYPES" :key="st" :value="st" :label="st" />
          </el-select>
        </el-form-item>
        <el-form-item :label="t('acctmap.accessToken')">
          <el-input v-model="pushForm.access_token" show-password
            :placeholder="t('acctmap.tokenOptionalPh')" />
        </el-form-item>
        <el-form-item :label="t('acctmap.refreshToken')">
          <el-input v-model="pushForm.refresh_token" show-password
            :placeholder="t('acctmap.tokenOptionalPh')" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="pushDialog = false">{{ t('common.cancel') }}</el-button>
        <el-button type="primary" @click="doPush">{{ t('common.save') }}</el-button>
      </template>
    </el-dialog>
  </el-card>
</template>

<style scoped>
.toolbar { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; margin-bottom: 10px; }
.grow { flex: 1; }
.stat { font-size: 13px; color: #64748b; margin-right: 6px; }
h4 { margin: 6px 0 10px; color: #334155; }
.bind-block { margin-bottom: 18px; }
.bind-label { font-size: 13px; font-weight: 600; color: #334155; margin-bottom: 8px; }
.bind-hint { font-weight: 400; font-size: 12px; color: #94a3b8; margin-left: 8px; }
.bind-toolbar { display: flex; align-items: center; gap: 8px; margin-bottom: 8px; }
.bind-cnt { font-size: 12px; color: #64748b; white-space: nowrap; margin-left: auto; }
.bind-empty { color: #999; font-size: 13px; padding: 24px 0; text-align: center;
  border: 1px dashed #e2e8f0; border-radius: 4px; }
</style>
