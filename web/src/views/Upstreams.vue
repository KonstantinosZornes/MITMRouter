<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { useI18n } from 'vue-i18n'
import { api, type UpstreamDTO } from '../api'
import { errText } from '../i18n'

const { t } = useI18n()
const items = ref<UpstreamDTO[]>([])
const dialog = ref(false)
const editing = ref<number | null>(null)
const testing = ref<Record<number, string>>({})
const filterName = ref('')
import { Plus, Search } from '@element-plus/icons-vue'
import { computed } from 'vue'
const filteredItems = computed(() => {
  const k = filterName.value.trim().toLowerCase()
  if (!k) return items.value
  return items.value.filter(i => i.name.toLowerCase().includes(k) || i.platform.includes(k))
})

const platforms = ['dataimpulse', 'decodo', '1024proxy', 'resin', 'generic', 'plain']
// 协议样本与语言无关，放组件常量（含 @ 与 {} 字符，不能走 vue-i18n 消息编译器）
const platformHints: Record<string, string> = {
  dataimpulse: 'http://<user>__cr.us:<pass>@gw.dataimpulse.com:823   (;sessid.<fp> appended)',
  decodo: 'http://user-<login>-session-x-sessionduration-30:<pass>@gate.decodo.com:7000',
  '1024proxy': 'socks5://<apikey>-region-US-sid-z-t-5:<pass>@us.1024proxy.io:3000',
  resin: 'socks5://Default:<RESIN_TOKEN>@resin:2260   (sticky: Default.<fp>)',
  generic: '{user} {sid} {ttl_min} {country}',
  plain: 'http://user:pass@proxy.example.com:8080    socks5://user:pass@proxy.example.com:1080',
}
function hint(p: string) { return platformHints[p] ?? '' }

const form = reactive<{ id: number|null, name: string, platform: string, base_url: string, inject: string, enabled: boolean }>({
  id: null, name: '', platform: 'dataimpulse', base_url: '', inject: '', enabled: true,
})

async function load() {
  const r = await api<{ items: UpstreamDTO[] }>('/api/upstreams')
  items.value = r.items
}
onMounted(load)

function openCreate() {
  editing.value = null
  Object.assign(form, { id: null, name: '', platform: 'dataimpulse', base_url: '', inject: '', enabled: true })
  dialog.value = true
}
function openEdit(row: UpstreamDTO) {
  editing.value = row.id
  // 仅 generic 携带 inject（对象→格式化 JSON）；其他平台不带该字段，避免回写垃圾值
  const injStr = row.platform === 'generic' && row.inject ? JSON.stringify(row.inject) : ''
  Object.assign(form, { id: row.id, name: row.name, platform: row.platform, base_url: row.base_url, inject: injStr, enabled: row.enabled })
  dialog.value = true
}
async function save() {
  try {
    const payload: any = { ...form }
    if (payload.platform === 'generic') {
      // 文本框内容是 JSON 文本，解析成对象再提交（与后端 RawMessage 契约一致）
      const txt = String(payload.inject ?? '').trim()
      if (txt) {
        try { payload.inject = JSON.parse(txt) }
        catch { ElMessage.error(t('upstreams.invalidJson')); return }
      } else {
        payload.inject = null
      }
    } else {
      delete payload.inject
    }
    if (editing.value == null) await api('/api/upstreams', { method: 'POST', body: payload })
    else await api(`/api/upstreams/${form.id}`, { method: 'PUT', body: payload })
    dialog.value = false; await load(); ElMessage.success(t('upstreams.saved'))
  } catch (e: any) { ElMessage.error(errText(e)) }
}
async function del(row: UpstreamDTO) {
  const warn = row.platform === 'plain' ? ' ' + t('acctmap.delEgressCascade') : ''
  try {
    await ElMessageBox.confirm(t('upstreams.delConfirm', { name: row.name }) + warn, t('common.confirm'), { type: 'warning' })
  } catch { return }
  try { await api(`/api/upstreams/${row.id}`, { method: 'DELETE' }); await load(); ElMessage.success(t('upstreams.deleted')) }
  catch (e: any) { ElMessage.error(errText(e)) }
}
async function makeDefault(row: UpstreamDTO) {
  try { await api(`/api/upstreams/${row.id}/default`, { method: 'POST' }); await load(); ElMessage.success(t('upstreams.defaultSwitched')) }
  catch (e: any) { ElMessage.error(errText(e)) }
}
async function test(row: UpstreamDTO) {
  testing.value[row.id] = t('common.loading')
  try {
    const r = await api<any>(`/api/upstreams/${row.id}/test`, { method: 'POST' })
    testing.value[row.id] = r.err ? (t('upstreams.testFailedPrefix') + r.err.slice(0, 40))
      : [r.egress_ip, [r.city, r.region, r.country].filter(Boolean).join(', ')].filter(Boolean).join(' · ')
        + ` (${r.dur_ms}ms)`
  } catch (e: any) { testing.value[row.id] = errText(e) }
}

// ---------- 出站关联账户（出站方向，docs/011 §4）：服务端分页搜索，不全量拉取 ----------
interface AcctKey { platform: string; account: string }
const bindDialog = ref(false)
const bindTarget = ref<UpstreamDTO | null>(null)
const bindMode = ref('sticky')
const KS = '\u241F'
const acctKey = (a: AcctKey) => a.platform + KS + a.account

// 浏览视图：服务端分页 + 搜索（账号库再大，弹窗开销恒定）
const PAGE_SIZE = 90 // 30 行 × 每行 3 组（平台/账号）
const bindItems = ref<AcctKey[]>([])
const bindTotal = ref(0)
const bindPage = ref(1)
const bindFilter = ref('')
const bindPlatform = ref('')
const bindPlatformOpts = ref<Record<string, number>>({})
const onlySelected = ref(false)
const loadingAccounts = ref(false)

// 已选集合：跨页/跨搜索持久，与浏览视图解耦
const selectedMap = ref(new Map<string, AcctKey>())
const selectedCount = computed(() => selectedMap.value.size)

function mutateSel(fn: (m: Map<string, AcctKey>) => void) {
  const m = new Map(selectedMap.value)
  fn(m)
  selectedMap.value = m
}
const isChecked = (a: AcctKey) => selectedMap.value.has(acctKey(a))
const toggleRow = (a: AcctKey, on: boolean) =>
  mutateSel(m => { if (on) m.set(acctKey(a), a); else m.delete(acctKey(a)) })

// 全选/反选作用于当前页；清空作用于整个已选集合
const selectAllVisible = () => mutateSel(m => bindItems.value.forEach(a => m.set(acctKey(a), a)))
const invertVisible = () => mutateSel(m => bindItems.value.forEach(a => {
  const k = acctKey(a); if (m.has(k)) m.delete(k); else m.set(k, a)
}))
const selectNone = () => mutateSel(m => m.clear())
const clearPageVisible = () => mutateSel(m => bindItems.value.forEach(a => m.delete(acctKey(a))))

// 每个列组（同屏 3 组）的表头框：只作用于该列的 20 个账号
const groupItems = (g: number) => bindItems.value.filter((_, idx) => idx % 3 === g - 1)
const groupAllChecked = (g: number) => {
  const items = groupItems(g)
  return items.length > 0 && items.every(isChecked)
}
const groupSomeChecked = (g: number) => {
  const items = groupItems(g)
  const n = items.filter(isChecked).length
  return n > 0 && n < items.length
}
const toggleGroup = (g: number) => {
  const items = groupItems(g)
  const all = groupAllChecked(g)
  mutateSel(m => items.forEach(a => { const k = acctKey(a); all ? m.delete(k) : m.set(k, a) }))
}

// 6 列网格：当前页条目按 3 个一组切行（每行 = 勾选|平台|账号 ×3）
const bindRows = computed(() => {
  const rows: (AcctKey | null)[][] = []
  for (let i = 0; i < bindItems.value.length; i += 3) {
    rows.push([bindItems.value[i] ?? null, bindItems.value[i + 1] ?? null, bindItems.value[i + 2] ?? null])
  }
  return rows
})

// 「只看已选」：纯客户端分页渲染已选集合
const selectedSlice = computed(() => {
  const arr = [...selectedMap.value.values()]
  const start = (bindPage.value - 1) * PAGE_SIZE
  return { items: arr.slice(start, start + PAGE_SIZE), total: arr.length }
})

let searchTimer: number | undefined
function onSearchInput() {
  window.clearTimeout(searchTimer)
  searchTimer = window.setTimeout(() => { bindPage.value = 1; loadPage() }, 300)
}

async function loadPage() {
  loadingAccounts.value = true
  try {
    if (onlySelected.value) {
      const s = selectedSlice.value
      bindItems.value = s.items
      bindTotal.value = s.total
      return
    }
    const q = new URLSearchParams({ page: String(bindPage.value), page_size: String(PAGE_SIZE) })
    const kw = bindFilter.value.trim().toLowerCase() // 账号统一小写，后端子串匹配
    if (kw) q.set('account', kw)
    if (bindPlatform.value) q.set('platform', bindPlatform.value)
    const r = await api<{ items: AcctKey[]; total: number }>(`/api/acctmap?${q}`)
    const seen = new Set<string>() // 同账号多来源行：页内去重
    bindItems.value = r.items.filter(i => {
      const k = acctKey(i); if (seen.has(k)) return false; seen.add(k); return true
    })
    bindTotal.value = r.total
  } catch (e: any) { ElMessage.error(errText(e)) }
  finally { loadingAccounts.value = false }
}

async function openBind(row: UpstreamDTO) {
  bindTarget.value = row
  bindMode.value = 'sticky'
  bindFilter.value = ''; bindPlatform.value = ''; onlySelected.value = false
  bindPage.value = 1
  selectedMap.value = new Map()
  bindItems.value = []
  bindTotal.value = 0
  bindDialog.value = true
  loadingAccounts.value = true
  try {
    const [egRes, stats] = await Promise.all([
      api<{ items: { platform: string; account: string; egress_ids: number[] }[] }>('/api/acctegress'),
      api<{ by_platform: Record<string, number> }>('/api/acctmap/stats'),
    ])
    bindPlatformOpts.value = stats.by_platform ?? {}
    for (const b of egRes.items) {
      if (!b.egress_ids.includes(row.id)) continue
      selectedMap.value = new Map(selectedMap.value)
        .set(`${b.platform}${KS}${b.account}`, { platform: b.platform, account: b.account })
    }
    await loadPage()
  } catch (e: any) {
    ElMessage.error(errText(e))
    loadingAccounts.value = false
  }
}

async function saveBind() {
  if (!bindTarget.value) return
  const accounts = [...selectedMap.value.values()]
  try {
    await api(`/api/acctegress/egress/${bindTarget.value.id}`, { method: 'PUT', body: { accounts, mode: bindMode.value } })
    bindDialog.value = false
    ElMessage.success(t('common.saved'))
  } catch (e: any) { ElMessage.error(errText(e)) }
}
</script>

<template>
  <el-card shadow="never" class="page">
  <div class="toolbar">
    <el-input v-model="filterName" :placeholder="t('upstreams.filterPh')" clearable
      style="width:220px" :prefix-icon="Search" />
    <span class="grow"></span>
    <el-button type="primary" @click="openCreate">
      <el-icon style="margin-right:4px"><Plus /></el-icon>{{ t('upstreams.create') }}
    </el-button>
  </div>
  <el-table :data="filteredItems" stripe>
    <el-table-column :label="t('upstreams.colName')" width="150">
      <template #default="{ row }"><b>{{ row.name }}</b><el-tag v-if="row.default" size="small" type="success" style="margin-left:6px">{{ t('upstreams.defaultTag') }}</el-tag></template>
    </el-table-column>
    <el-table-column :label="t('upstreams.colPlatform')" width="130">
      <template #default="{ row }">
        <el-tag v-if="row.platform === 'plain'" size="small" type="warning">plain</el-tag>
        <span v-else>{{ row.platform }}</span>
      </template>
    </el-table-column>
    <el-table-column :label="t('upstreams.colCred')" prop="base_url" min-width="260" />
    <el-table-column :label="t('upstreams.colEnabled')" width="90">
      <template #default="{ row }"><el-tag :type="row.enabled ? 'success' : 'info'">{{ row.enabled ? t('common.yes') : t('common.no') }}</el-tag></template>
    </el-table-column>
    <el-table-column :label="t('upstreams.colTest')" min-width="200">
      <template #default="{ row }">
        <el-button size="small" @click="test(row)">{{ t('common.testBtn') }}</el-button>
        <span v-if="testing[row.id]" style="margin-left:8px;font-size:13px">{{ testing[row.id] }}</span>
      </template>
    </el-table-column>
    <el-table-column :label="t('upstreams.colOps')" width="290">
      <template #default="{ row }">
        <el-button v-if="row.platform === 'plain'" size="small" link type="primary" @click="openBind(row)">{{ t('acctmap.bindEgressShort') }}</el-button>
        <el-button size="small" link type="primary" :disabled="row.default" @click="makeDefault(row)">{{ t('upstreams.setDefault') }}</el-button>
        <el-button size="small" link type="primary" @click="openEdit(row)">{{ t('common.edit') }}</el-button>
        <el-button size="small" link type="danger" :disabled="row.default" @click="del(row)">{{ t('common.delete') }}</el-button>
      </template>
    </el-table-column>
  </el-table>

  <el-dialog v-model="dialog" :title="editing == null ? t('upstreams.create') : t('upstreams.editTitle')" width="640px">
    <el-form label-width="100px">
      <el-form-item :label="t('upstreams.name')"><el-input v-model="form.name" /></el-form-item>
      <el-form-item :label="t('upstreams.platform')">
        <el-select v-model="form.platform" style="width:100%">
          <el-option v-for="p in platforms" :key="p" :value="p" :label="p" />
        </el-select>
      </el-form-item>
      <el-form-item :label="t('upstreams.baseUrl')">
        <el-input v-model="form.base_url" placeholder="http://user:pass@gw.example.com:823" />
        <div style="color:#999;font-size:12px;margin-top:4px">{{ hint(form.platform) }}</div>
      </el-form-item>
      <el-form-item v-if="form.platform === 'generic'" :label="t('upstreams.templateInject')">
        <el-input v-model="form.inject" placeholder='{"username_template":"{user}-sessid-{sid}","password":"xxx"}' />
      </el-form-item>
      <el-form-item :label="t('upstreams.enableSwitch')"><el-switch v-model="form.enabled" /></el-form-item>
    </el-form>
    <template #footer>
      <el-button @click="dialog = false">{{ t('common.cancel') }}</el-button>
      <el-button type="primary" @click="save">{{ t('common.save') }}</el-button>
    </template>
  </el-dialog>

  <!-- 出站关联账户：服务端分页搜索（不全量拉取），6 列密排网格；全选/反选作用于当前页 -->
  <el-drawer v-model="bindDialog" :title="t('acctmap.bindEgressTitle', { name: bindTarget?.name ?? '' })"
    size="calc(100% - 244px)" append-to-body>
    <template #header>
      <h3 style="margin:0;font-size:16px">{{ t('acctmap.bindEgressTitle', { name: bindTarget?.name ?? '' }) }}</h3>
    </template>
    <div class="bind-block">
      <div class="bind-label">
        {{ t('acctmap.mode') }}
        <span class="bind-hint">{{ t('acctmap.modeOnlyNewHint') }}</span>
      </div>
      <el-radio-group v-model="bindMode">
        <el-radio-button value="sticky">{{ t('acctmap.modeSticky') }}</el-radio-button>
        <el-radio-button value="random">{{ t('acctmap.modeRandom') }}</el-radio-button>
      </el-radio-group>
    </div>

    <div class="bind-block">
      <div class="bind-toolbar">
        <el-input v-model="bindFilter" clearable :prefix-icon="Search"
          :placeholder="t('acctmap.bindSearchPh')" style="width:190px" @input="onSearchInput" />
        <el-select v-model="bindPlatform" clearable :placeholder="t('acctmap.colPlatform')"
          style="width:140px" @change="bindPage = 1; loadPage()">
          <el-option v-for="(cnt, pf) in bindPlatformOpts" :key="pf" :value="pf" :label="`${pf} (${cnt})`" />
        </el-select>
        <el-checkbox v-model="onlySelected" style="margin:0 6px"
          @change="bindPage = 1; loadPage()">{{ t('acctmap.onlySelected') }}</el-checkbox>
        <el-button :disabled="loadingAccounts || !bindItems.length" @click="selectAllVisible">{{ t('acctmap.selAll') }}</el-button>
        <el-button :disabled="loadingAccounts || !bindItems.length" @click="invertVisible">{{ t('acctmap.selInvert') }}</el-button>
        <el-button :disabled="!selectedCount" @click="selectNone">{{ t('acctmap.selNone') }}</el-button>
        <span class="bind-cnt">{{ t('acctmap.selectedCount', { n: selectedCount }) }}</span>
      </div>

      <el-table v-loading="loadingAccounts" :data="bindRows" max-height="calc(100vh - 330px)" size="small" border
        style="width:100%" :empty-text="t('acctmap.bindNoMatch')">
        <template v-for="g in 3" :key="g">
          <el-table-column width="42" align="center">
            <template #header>
              <el-checkbox :model-value="groupAllChecked(g)" :indeterminate="groupSomeChecked(g)"
                :title="t('acctmap.selAll')" @change="toggleGroup(g)" />
            </template>
            <template #default="{ row }">
              <el-checkbox v-if="row[g - 1]" :model-value="isChecked(row[g - 1])"
                @change="(v: any) => toggleRow(row[g - 1], v)" @click.stop />
            </template>
          </el-table-column>
          <el-table-column :label="t('acctmap.colPlatform')" width="96">
            <template #default="{ row }">
              <div v-if="row[g - 1]" class="bind-cell" @click.stop="toggleRow(row[g - 1], !isChecked(row[g - 1]))">
                <el-tag size="small" type="info">{{ row[g - 1].platform }}</el-tag>
              </div>
            </template>
          </el-table-column>
          <el-table-column :label="t('acctmap.colAccount')" min-width="220">
            <template #default="{ row }">
              <div v-if="row[g - 1]" class="bind-cell" :title="`${row[g - 1].platform} / ${row[g - 1].account}`"
                @click.stop="toggleRow(row[g - 1], !isChecked(row[g - 1]))">
                <span class="bind-acct">{{ row[g - 1].account }}</span>
              </div>
            </template>
          </el-table-column>
        </template>
      </el-table>

      <div class="bind-foot">
        <el-pagination small layout="total, prev, pager, next" :total="bindTotal" :page-size="PAGE_SIZE"
          :current-page="bindPage" @current-change="(p: number) => { bindPage = p; loadPage() }" />
        <span class="bind-hint">{{ t('acctmap.pageScopeHint') }}</span>
      </div>
    </div>

    <template #footer>
      <div style="display:flex;justify-content:flex-end;gap:10px">
        <el-button @click="bindDialog = false">{{ t('common.cancel') }}</el-button>
        <el-button type="primary" @click="saveBind">{{ t('common.save') }}</el-button>
      </div>
    </template>
  </el-drawer>
  </el-card>
</template>

<style scoped>
.bind-block { margin-bottom: 18px; }
.bind-label { font-size: 13px; font-weight: 600; color: #334155; margin-bottom: 8px; }
.bind-hint { font-weight: 400; font-size: 12px; color: #94a3b8; margin-left: 8px; }
.bind-toolbar { display: flex; align-items: center; gap: 8px; margin-bottom: 8px; }
.bind-cnt { font-size: 12px; color: #64748b; white-space: nowrap; margin-left: auto; }
.bind-acct { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 12px;
  display: block; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.bind-empty { color: #999; font-size: 13px; padding: 24px 0; text-align: center;
  border: 1px dashed #e2e8f0; border-radius: 4px; }
.bind-cell { cursor: pointer; display: flex; align-items: center; min-height: 20px; overflow: hidden; max-width: 100%; }
.bind-foot { display: flex; align-items: center; gap: 12px; margin-top: 8px; }
.bind-foot .bind-hint { font-weight: 400; font-size: 12px; color: #94a3b8; }
</style>
