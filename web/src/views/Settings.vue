<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Monitor, Setting, Key, Link, Guide, Lock, Tools, InfoFilled, CopyDocument, Check } from '@element-plus/icons-vue'
import { useI18n } from 'vue-i18n'
import { api, type SettingsDTO } from '../api'
import { errText } from '../i18n'

const { t } = useI18n()
const form = reactive<Partial<SettingsDTO>>({})
const restartTip = ref(false)
const pwDialog = ref(false)
const oldPw = ref(''), newPw = ref('')
const upstreams = ref<{ name: string }[]>([])
// 入站认证拆成 用户名/密码 两个输入；存储与接口仍是 "user:pass" 单串，已保存密码直接回显
const authUser = ref(''), authPass = ref('')

function splitListenAuth(v?: string) {
  if (!v) { authUser.value = ''; authPass.value = ''; return }
  const i = v.indexOf(':')
  authUser.value = i < 0 ? v : v.slice(0, i)
  authPass.value = i < 0 ? '' : v.slice(i + 1)
}

onMounted(async () => {
  Object.assign(form, await api<SettingsDTO>('/api/settings'))
  splitListenAuth(form.listen_auth)
  const u = await api<{ items: { name: string }[] }>('/api/upstreams')
  upstreams.value = u.items
})

async function copyText(v: string | undefined) {
  if (!v) return
  try {
    await navigator.clipboard.writeText(v)
    ElMessage.success(t('common.copied'))
  } catch {
    ElMessage.error(t('common.copyFailed'))
  }
}

async function save() {
  // 入站认证：用户名、密码都填或都留空才合法
  const u = authUser.value.trim(), p = authPass.value
  if (!!u !== !!p) { ElMessage.error(t('settings.authPairErr')); return }
  form.listen_auth = u && p ? `${u}:${p}` : ''
  try {
    const r = await api<any>('/api/settings', { method: 'PUT', body: form })
    Object.assign(form, await api<SettingsDTO>('/api/settings'))
    splitListenAuth(form.listen_auth)
    restartTip.value = !!r.restart_required
    ElMessage.success(restartTip.value ? t('settings.restartBanner') : t('common.saved'))
    // TLS 证书有效期等非致命警告：保存已生效，但需要人工关注
    if (Array.isArray(r.warnings) && r.warnings.length) {
      ElMessage.warning({ message: r.warnings.join('；'), duration: 10000, showClose: true })
    }
  } catch (e: any) { ElMessage.error(errText(e)) }
}

async function resetSalt() {
  try {
    await ElMessageBox.confirm(t('settings.resetSaltConfirm'), t('common.confirm'), { type: 'warning' })
    const r = await api<any>('/api/settings/reset-salt', { method: 'POST' })
    form.hash_salt = r.hash_salt
    ElMessage.success(t('settings.saltResetDone'))
  } catch (e: any) { if (e !== 'cancel') ElMessage.error(errText(e)) }
}

async function changePw() {
  try {
    await api('/api/auth/password', { method: 'POST', body: { old_password: oldPw.value, new_password: newPw.value } })
    pwDialog.value = false; oldPw.value = ''; newPw.value = ''
    ElMessage.success(t('settings.pwdChanged')); location.hash = '#/login'; location.reload()
  } catch (e: any) { ElMessage.error(errText(e)) }
}
</script>

<template>
  <div class="settings-page">
    <div class="settings-scroll">
      <el-alert v-if="restartTip" type="warning" :title="t('settings.restartBanner')" :closable="false" />

    <!-- 接入地址横幅：最常用的信息置顶；启用认证时直接展示可用的完整地址 -->
    <div v-if="form.ingress_url_auth || form.ingress_url" class="hero">
      <div class="hero-info">
        <div class="hero-label">{{ t('settings.ingressUrl') }}</div>
        <code class="hero-url">{{ form.ingress_url_auth || form.ingress_url }}</code>
        <div class="hero-sub">{{ t('settings.heroHint') }}</div>
      </div>
      <el-button class="hero-copy" size="large" round @click="copyText(form.ingress_url_auth || form.ingress_url)">
        <el-icon style="margin-right:5px"><CopyDocument /></el-icon>{{ t('common.copy') }}
      </el-button>
    </div>

    <div class="sgrid">
      <!-- 接入端口 -->
      <el-card shadow="never" class="sec">
        <template #header>
          <div class="sec-h">
            <span class="sec-ic"><el-icon><Monitor /></el-icon></span>
            <div>
              <div class="sec-t">{{ t('settings.secIngress') }}</div>
              <div class="sec-d">{{ t('settings.secIngressSub') }}</div>
            </div>
          </div>
        </template>
        <el-form label-position="top">
          <el-form-item :label="t('settings.inboundAuth')">
            <div class="pair">
              <div class="pair-col">
                <div class="pair-label">{{ t('settings.authUser') }}</div>
                <el-input v-model="authUser" />
              </div>
              <div class="pair-col">
                <div class="pair-label">{{ t('settings.authPass') }}</div>
                <el-input v-model="authPass" show-password />
              </div>
            </div>
            <div class="hint">{{ t('settings.authPairHint') }}</div>
          </el-form-item>
          <el-form-item :label="t('settings.listenTlsCert')">
            <div class="pair">
              <div class="pair-col">
                <div class="pair-label">{{ t('settings.tlsCertLabel') }}</div>
                <el-input v-model="form.listen_tls_cert" :placeholder="t('settings.tlsCertPh')" />
              </div>
              <div class="pair-col">
                <div class="pair-label">{{ t('settings.tlsKeyLabel') }}</div>
                <el-input v-model="form.listen_tls_key" :placeholder="t('settings.tlsKeyPh')" />
              </div>
            </div>
            <div class="hint">{{ t('settings.tlsHint') }}</div>
          </el-form-item>
        </el-form>
      </el-card>

      <!-- 管理台 -->
      <el-card shadow="never" class="sec">
        <template #header>
          <div class="sec-h">
            <span class="sec-ic"><el-icon><Setting /></el-icon></span>
            <div>
              <div class="sec-t">{{ t('settings.secAdmin') }}</div>
              <div class="sec-d">{{ t('settings.secAdminSub') }}</div>
            </div>
          </div>
        </template>
        <el-form label-position="top">
          <el-form-item :label="t('settings.listenNote')">
            <div class="info">
              <el-icon><InfoFilled /></el-icon>
              <span>{{ t('settings.listenCliOnly') }}</span>
            </div>
          </el-form-item>
          <el-form-item :label="t('settings.adminTlsCert')">
            <div class="pair">
              <div class="pair-col">
                <div class="pair-label">{{ t('settings.tlsCertLabel') }}</div>
                <el-input v-model="form.admin_tls_cert" :placeholder="t('settings.tlsCertPh')" />
              </div>
              <div class="pair-col">
                <div class="pair-label">{{ t('settings.tlsKeyLabel') }}</div>
                <el-input v-model="form.admin_tls_key" :placeholder="t('settings.tlsKeyPh')" />
              </div>
            </div>
            <div class="hint">{{ t('settings.tlsHint') }}</div>
          </el-form-item>
        </el-form>
      </el-card>

      <!-- 标识提取 -->
      <el-card shadow="never" class="sec">
        <template #header>
          <div class="sec-h">
            <span class="sec-ic"><el-icon><Key /></el-icon></span>
            <div>
              <div class="sec-t">{{ t('settings.secMarker') }}</div>
              <div class="sec-d">{{ t('settings.secMarkerSub') }}</div>
            </div>
          </div>
        </template>
        <el-form label-position="top">
          <el-form-item :label="t('settings.pathParts')">
            <el-select v-model="form.marker_path_parts" multiple filterable allow-create
              style="width:100%" :placeholder="t('settings.pathPartsPh')" />
          </el-form-item>
          <el-form-item :label="t('settings.headers')">
            <el-select v-model="form.marker_headers" multiple filterable allow-create style="width:100%" />
          </el-form-item>
          <div class="info">
            <el-icon><InfoFilled /></el-icon>
            <span>{{ t('settings.markerTip') }}</span>
          </div>
        </el-form>
      </el-card>

      <!-- 粘滞 -->
      <el-card shadow="never" class="sec">
        <template #header>
          <div class="sec-h">
            <span class="sec-ic"><el-icon><Link /></el-icon></span>
            <div>
              <div class="sec-t">{{ t('settings.secSticky') }}</div>
              <div class="sec-d">{{ t('settings.secStickySub') }}</div>
            </div>
          </div>
        </template>
        <el-form label-position="top">
          <el-form-item :label="t('settings.salt')">
            <div class="salt-row">
              <el-input v-model="form.hash_salt" disabled />
              <el-button type="danger" plain @click="resetSalt">{{ t('settings.resetSalt') }}</el-button>
            </div>
          </el-form-item>
          <div class="duo">
            <el-form-item :label="t('settings.sidLen')">
              <el-input-number v-model="form.sid_len" :min="4" :max="64" class="num" />
            </el-form-item>
            <el-form-item :label="t('settings.ttl')">
              <el-input-number v-model="form.session_ttl_min" :min="0" :max="1440" class="num" />
              <div class="hint">{{ t('settings.ttlHint') }}</div>
            </el-form-item>
          </div>
          <el-form-item :label="t('settings.saltRotateFailures')">
            <el-input-number v-model="form.salt_rotate_failure_threshold" :min="1" :max="100" class="num" />
            <div class="hint">{{ t('settings.saltRotateFailuresHint') }}</div>
          </el-form-item>
        </el-form>
      </el-card>

      <!-- 默认路由 -->
      <el-card shadow="never" class="sec">
        <template #header>
          <div class="sec-h">
            <span class="sec-ic"><el-icon><Guide /></el-icon></span>
            <div>
              <div class="sec-t">{{ t('settings.secRoute') }}</div>
              <div class="sec-d">{{ t('settings.secRouteSub') }}</div>
            </div>
          </div>
        </template>
        <el-form label-position="top">
          <el-form-item :label="t('settings.defaultUpstream')">
            <el-select v-model="form.default_upstream" clearable style="width:100%">
              <el-option v-for="u in upstreams" :key="u.name" :label="u.name" :value="u.name" />
            </el-select>
          </el-form-item>
          <el-form-item :label="t('settings.noMarkerPolicy')">
            <div class="policy">
              <el-radio-group v-model="form.no_marker_policy">
                <el-radio value="default_session">{{ t('settings.policyDefault') }}</el-radio>
                <el-radio value="client_ip_session">{{ t('settings.policyIP') }}</el-radio>
                <el-radio value="direct">{{ t('settings.policyDirect') }}</el-radio>
              </el-radio-group>
            </div>
          </el-form-item>
          <el-form-item :label="t('settings.blockPrivateTargets')">
            <el-switch v-model="form.block_private_targets" />
            <div class="hint">{{ t('settings.blockPrivateTargetsHint') }}</div>
          </el-form-item>
        </el-form>
      </el-card>

      <!-- 黑白名单 -->
      <el-card shadow="never" class="sec">
        <template #header>
          <div class="sec-h">
            <span class="sec-ic"><el-icon><Lock /></el-icon></span>
            <div>
              <div class="sec-t">{{ t('settings.secACL') }}</div>
              <div class="sec-d">{{ t('settings.secACLSub') }}</div>
            </div>
          </div>
        </template>
        <el-form label-position="top">
          <el-form-item :label="t('settings.aclWhite')">
            <el-select v-model="form.acl_whitelist" multiple filterable allow-create default-first-option
              style="width:100%" />
            <div class="hint">{{ t('settings.aclWhiteHint') }}</div>
          </el-form-item>
          <el-form-item :label="t('settings.aclBlack')">
            <el-select v-model="form.acl_blacklist" multiple filterable allow-create default-first-option
              style="width:100%" />
            <div class="hint">{{ t('settings.aclBlackHint') }}</div>
          </el-form-item>
        </el-form>
      </el-card>

      <!-- 维护 -->
      <el-card shadow="never" class="sec span2">
        <template #header>
          <div class="sec-h">
            <span class="sec-ic"><el-icon><Tools /></el-icon></span>
            <div>
              <div class="sec-t">{{ t('settings.secMaint') }}</div>
              <div class="sec-d">{{ t('settings.secMaintSub') }}</div>
            </div>
          </div>
        </template>
        <div class="maint-grid">
          <el-form label-position="top">
            <el-form-item :label="t('settings.retentionDays')">
              <el-input-number v-model="form.log_retention_days" :min="1" :max="3650" class="num" />
            </el-form-item>
          </el-form>
          <el-form label-position="top">
            <el-form-item :label="t('settings.metrics')">
              <el-switch v-model="form.metrics_enabled" />
            </el-form-item>
          </el-form>
          <el-form label-position="top">
            <el-form-item :label="t('settings.syncEmptyThreshold')">
              <el-input-number v-model="form.sync_empty_clear_threshold" :min="1" :max="100" class="num" />
              <div class="hint">{{ t('settings.syncEmptyThresholdHint') }}</div>
            </el-form-item>
          </el-form>
        </div>
      </el-card>
    </div>
    </div>

    <!-- 底部操作坞：与顶栏样式一致，固定贴住下边界，不遮挡滚动内容 -->
    <div class="actionbar">
      <el-button type="primary" size="large" @click="save">
        <el-icon style="margin-right:6px"><Check /></el-icon>{{ t('settings.saveBtn') }}
      </el-button>
      <span class="grow"></span>
      <a href="/api/ca.pem" download><el-button>{{ t('settings.downloadCa') }}</el-button></a>
      <a href="/api/ca.crt" download><el-button>{{ t('settings.downloadCaCrt') }}</el-button></a>
      <el-button @click="pwDialog = true">{{ t('settings.changePwd') }}</el-button>
    </div>
  </div>

  <el-dialog v-model="pwDialog" :title="t('settings.changePwd')" width="400px">
    <el-form label-width="90px">
      <el-form-item :label="t('settings.dialogOld')"><el-input v-model="oldPw" type="password" show-password /></el-form-item>
      <el-form-item :label="t('settings.dialogNew')"><el-input v-model="newPw" type="password" show-password /></el-form-item>
    </el-form>
    <template #footer>
      <el-button @click="pwDialog = false">{{ t('common.cancel') }}</el-button>
      <el-button type="primary" @click="changePw">{{ t('common.confirm') }}</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
/* 页面 = 上下固定（顶栏在壳层、底栏是操作坞）+ 中间滚动框 */
.settings-page {
  height: 100%;
  display: flex; flex-direction: column;
}
.settings-scroll {
  flex: 1 1 auto; min-height: 0;
  overflow-y: auto;
  padding: 22px 26px;
  display: flex; flex-direction: column; gap: 16px;
}

/* ---------- 接入地址横幅 ---------- */
.hero {
  display: flex; align-items: center; gap: 18px;
  padding: 18px 22px;
  border-radius: var(--card-radius);
  background: linear-gradient(135deg, var(--brand), var(--brand-2));
  color: #fff;
  box-shadow: 0 12px 28px rgba(79, 70, 229, .28);
}
.hero-info { flex: 1; min-width: 0; }
.hero-label { font-size: 12px; font-weight: 600; letter-spacing: .08em; text-transform: uppercase; opacity: .8; }
.hero-url {
  display: block; margin-top: 6px;
  font-family: ui-monospace, 'SF Mono', SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 15px; font-weight: 650; line-height: 1.4; word-break: break-all;
}
.hero-sub { margin-top: 6px; font-size: 12px; opacity: .72; }
.hero-copy { background: #fff; border-color: #fff; color: var(--brand); font-weight: 600; }

/* ---------- 分区栅格：两列等高配对，窄屏回退单列 ---------- */
.sgrid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px; }
.sgrid .span2 { grid-column: 1 / -1; }
@media (max-width: 1120px) { .sgrid { grid-template-columns: 1fr; } }

/* ---------- 卡片头：图标徽标 + 标题 + 描述 ---------- */
.sec :deep(.el-card__header) { padding: 14px 20px; border-bottom: 1px solid #f1f2f5; }
.sec :deep(.el-card__body) { padding: 18px 20px 8px; }
.sec-h { display: flex; align-items: center; gap: 10px; }
.sec-ic {
  flex: none; width: 30px; height: 30px; border-radius: 9px;
  display: flex; align-items: center; justify-content: center;
  background: rgba(79, 70, 229, .09); color: var(--brand); font-size: 15px;
}
.sec-t { font-size: 14px; font-weight: 650; color: #111827; line-height: 1.2; }
.sec-d { margin-top: 2px; font-size: 12px; color: #8b94a3; line-height: 1.4; }

/* ---------- 字段 ---------- */
.hint { margin-top: 6px; font-size: 12px; color: #98a1ad; line-height: 1.55; }
.pair { display: grid; grid-template-columns: 1fr 1fr; gap: 10px; width: 100%; }
/* 小控件并排：紧凑排版，替代每个字段独占一行 */
.duo { display: grid; grid-template-columns: 1fr 1fr; gap: 0 10px; }
.duo .num { width: 100%; }
.pair-label { margin-bottom: 5px; font-size: 12px; color: #6b7280; }
.salt-row { display: flex; gap: 8px; width: 100%; }
.salt-row :deep(.el-input) { flex: 1; }
.num { width: 150px; }
.info {
  display: flex; align-items: flex-start; gap: 8px;
  padding: 10px 12px;
  border: 1px solid #eef0f4; border-radius: 10px; background: #f8f9fb;
  font-size: 12.5px; color: #5c6675; line-height: 1.6;
}
.info :deep(.el-icon) { flex: none; margin-top: 2px; color: var(--brand); }
.policy :deep(.el-radio) { margin-right: 18px; }

/* ---------- 维护：三项一行三列，与其他卡片的行节奏一致 ---------- */
.maint-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 24px; }
@media (max-width: 640px) { .maint-grid { grid-template-columns: 1fr; } }

/* ---------- 底部操作坞（与顶栏一致：毛玻璃 + 上边框，固定贴住下边界） ---------- */
.actionbar {
  flex: none;
  display: flex; align-items: center; gap: 10px; flex-wrap: wrap;
  padding: 12px 26px;
  background: rgba(255, 255, 255, .9);
  backdrop-filter: blur(10px);
  border-top: 1px solid #eceef2;
}
.actionbar .grow { flex: 1; }
</style>
