import { createRouter, createWebHashHistory } from 'vue-router'
import Login from './views/Login.vue'
import Settings from './views/Settings.vue'
import Upstreams from './views/Upstreams.vue'
import Audit from './views/Audit.vue'
import AcctMap from './views/AcctMap.vue'
import Updates from './views/Updates.vue'

const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: '/login', component: Login },
    { path: '/settings', component: Settings, meta: { titleKey: 'menu.settings' } },
    { path: '/upstreams', component: Upstreams, meta: { titleKey: 'menu.upstreams' } },
    { path: '/acctmap', component: AcctMap, meta: { titleKey: 'menu.acctmap' } },
    { path: '/audit', component: Audit, meta: { titleKey: 'menu.audit' } },
    { path: '/updates', component: Updates, meta: { titleKey: 'menu.updates' } },
    { path: '/', redirect: '/settings' },
  ],
})

router.beforeEach(async (to) => {
  if (to.path === '/login') return true
  try { await fetch('/api/auth/me', { credentials: 'include' }).then(r => { if (!r.ok) throw 0 }) }
  catch { return '/login' }
  return true
})

export default router
