<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { Setting, Connection, Document, User, Clock } from '@element-plus/icons-vue'
import { setLocale } from './i18n'

const route = useRoute(), router = useRouter(), { t, locale } = useI18n()
const isLogin = computed(() => route.path === '/login')
const pageTitle = computed(() => {
  const key = route.meta.titleKey as string | undefined
  return key ? t(key) : ''
})
const menuItems = [
  { path: '/settings', key: 'menu.settings', icon: Setting },
  { path: '/upstreams', key: 'menu.upstreams', icon: Connection },
  { path: '/acctmap', key: 'menu.acctmap', icon: User },
  { path: '/audit', key: 'menu.audit', icon: Document },
  { path: '/updates', key: 'menu.updates', icon: Clock },
]
function switchLang(l: 'zh-CN' | 'en-US') {
  setLocale(l); location.reload()
}
async function logout() {
  try {
    await fetch('/api/auth/logout', { method: 'POST', credentials: 'include' })
  } finally {
    await router.push('/login')
    location.reload()
  }
}
</script>

<template>
  <router-view v-if="isLogin" />
  <el-container v-else style="height:100vh">
    <el-aside width="224px" class="side">
      <div class="brand">
        <div class="logo">M</div>
        <div>
          <div class="brand-name">MITMRouter</div>
          <div class="brand-sub">Router Console</div>
        </div>
      </div>

      <el-menu :default-active="route.path" router
        background-color="transparent" text-color="#9ca3af" active-text-color="#ffffff">
        <el-menu-item v-for="mi in menuItems" :key="mi.path" :index="mi.path">
          <el-icon><component :is="mi.icon" /></el-icon>
          <span>{{ t(mi.key) }}</span>
        </el-menu-item>
      </el-menu>

      <div class="side-bottom">
        <span class="ver">v0.1.0</span>
      </div>
    </el-aside>

    <el-container>
      <el-header height="58px" class="topbar">
        <div class="topbar-title">{{ pageTitle }}</div>
        <el-dropdown @command="switchLang">
          <span class="lang-btn">{{ locale === 'zh-CN' ? '中文' : 'EN' }}</span>
          <template #dropdown>
            <el-dropdown-menu>
              <el-dropdown-item command="zh-CN">中文</el-dropdown-item>
              <el-dropdown-item command="en-US">English</el-dropdown-item>
            </el-dropdown-menu>
          </template>
        </el-dropdown>
        <el-dropdown>
          <span class="user-chip"><span class="avatar">A</span><span>admin</span></span>
          <template #dropdown>
            <el-dropdown-menu>
              <el-dropdown-item @click="logout">{{ t('common.logout') }}</el-dropdown-item>
            </el-dropdown-menu>
          </template>
        </el-dropdown>
      </el-header>
      <el-main class="main">
        <router-view />
      </el-main>
    </el-container>
  </el-container>
</template>

<style scoped>
.side {
  background: linear-gradient(180deg, #0b1220, #101a30);
  display: flex; flex-direction: column;
}
.brand { display: flex; align-items: center; gap: 10px; padding: 20px 18px 16px; }
.logo {
  width: 38px; height: 38px; border-radius: 11px;
  display: flex; align-items: center; justify-content: center;
  color: #fff; font-weight: 800; font-size: 17px;
  background: linear-gradient(135deg, var(--brand), var(--brand-2));
  box-shadow: 0 6px 18px rgba(79, 70, 229, .45);
}
.brand-name { color: #f9fafb; font-weight: 750; font-size: 15px; letter-spacing:.2px }
.brand-sub { color: #64748b; font-size: 11px; letter-spacing:.4px }
.side :deep(.el-menu) { border-right: none; background: transparent; padding: 0 10px; }
.side :deep(.el-menu-item) { border-radius: 9px; margin-bottom: 4px; }
.side :deep(.el-menu-item:hover) { background: rgba(148, 163, 184, .1); }
.side :deep(.el-menu-item.is-active) {
  background: linear-gradient(90deg, rgba(79, 70, 229, .35), rgba(124, 58, 237, .18));
  box-shadow: inset 0 0 0 1px rgba(99, 102, 241, .35);
}
.side-bottom {
  margin-top: auto; padding: 12px 18px; border-top: 1px solid #1e293b;
  display: flex; align-items: center; justify-content: space-between;
}
.ver { color: #475569; font-size: 11px; }
.topbar {
  position: sticky; top: 0; z-index: 10;
  background: rgba(255,255,255,.85); backdrop-filter: blur(10px);
  border-bottom: 1px solid #eceef2;
  display: flex; align-items: center; justify-content: space-between;
}
.topbar-title { font-size: 16px; font-weight: 700; }
</style>
