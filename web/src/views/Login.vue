<script setup lang="ts">
import { ref, computed } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { Lock } from '@element-plus/icons-vue'
import { useI18n } from 'vue-i18n'
import { setLocale, errText } from '../i18n'

const { t, locale } = useI18n()
const router = useRouter()
const pw = ref(''), loading = ref(false)
const other = computed(() => (locale.value === 'zh-CN' ? 'en-US' : 'zh-CN'))

async function submit() {
  if (!pw.value) return
  loading.value = true
  try {
    const r = await fetch('/api/auth/login', {
      method: 'POST', headers: { 'content-type': 'application/json' },
      credentials: 'include', body: JSON.stringify({ password: pw.value }),
    })
    if (!r.ok) { ElMessage.error(errText({ code: (await r.json())?.error?.code })) ; return }
    router.push('/settings')
  } catch (e: any) { ElMessage.error(errText(e)) } finally { loading.value = false }
}
</script>

<template>
  <div class="login-wrap">
    <div class="orb o1" />
    <div class="orb o2" />

    <el-card class="login-card" shadow="always">
      <div class="login-logo"><el-icon :size="24" color="#fff"><Lock /></el-icon></div>
      <h3 class="login-title">{{ t('login.title') }}</h3>
      <p class="login-sub">{{ t('menu.settings') }} · {{ t('menu.upstreams') }} · {{ t('menu.audit') }}</p>

      <form @submit.prevent="submit">
        <el-input v-model="pw" type="password" size="large" :placeholder="t('login.password')"
          show-password :prefix-icon="Lock" autocomplete="current-password" />
        <el-button native-type="submit" type="primary" size="large" class="login-btn"
          :loading="loading">{{ t('login.submit') }}</el-button>
      </form>

      <div class="lang-row">
        <el-button text size="small" @click="setLocale(other); location.reload()">
          {{ locale === 'zh-CN' ? 'English' : '中文' }}
        </el-button>
      </div>
    </el-card>
  </div>
</template>

<style scoped>
.login-wrap {
  min-height: 100vh; position: relative; overflow: hidden;
  display: flex; align-items: center; justify-content: center;
  background: linear-gradient(160deg, #0b1220, #101a30);
}
.orb { position: absolute; border-radius: 50%; filter: blur(80px); opacity: .5; pointer-events: none; }
.o1 { width: 480px; height: 480px; background: #6d28d9; top: -160px; left: -100px; }
.o2 { width: 420px; height: 420px; background: #4338ca; bottom: -150px; right: -80px; }

.login-card {
  width: 392px; border-radius: 20px;
  background: rgba(255, 255, 255, .96);
  border: 1px solid rgba(255, 255, 255, .6);
  text-align: center; padding: 10px 8px;
  box-shadow: 0 24px 70px rgba(2, 6, 23, .55);
  position: relative; z-index: 1;
}
.login-logo {
  width: 54px; height: 54px; margin: 6px auto 14px; border-radius: 16px;
  display: flex; align-items: center; justify-content: center;
  background: linear-gradient(135deg, var(--brand), var(--brand-2));
  box-shadow: 0 0 0 8px rgba(99, 102, 241, .15), 0 10px 24px rgba(79, 70, 229, .45);
}
.login-title { margin: 0 0 4px; font-size: 19px; font-weight: 750; letter-spacing:.3px }
.login-sub { margin: 0 0 20px; color: #9ca3af; font-size: 12px; }
.login-btn {
  width: 100%; margin-top: 16px; border-radius: 11px;
  background: linear-gradient(135deg, var(--brand), var(--brand-2));
  border: none;
}
.lang-row { margin-top: 12px; }
</style>
