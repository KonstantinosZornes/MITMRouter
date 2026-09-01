import { createI18n } from 'vue-i18n'
import zh from './zh'
import en from './en'

const saved = localStorage.getItem('locale') as 'zh-CN' | 'en-US' | null
export const i18n = createI18n({
  legacy: false,
  locale: saved ?? 'zh-CN',
  fallbackLocale: 'zh-CN',
  messages: { 'zh-CN': zh, 'en-US': en },
})

export function setLocale(l: 'zh-CN' | 'en-US') {
  i18n.global.locale.value = l
  localStorage.setItem('locale', l)
}

// errText 按后端错误码翻译；未知码回退原始 message。
export function errText(e: any): string {
  const code = e?.code as string | undefined
  const { t, te } = i18n.global as any
  if (code && te(`errors.${code}`)) return t(`errors.${code}`)
  return e?.message ?? t('errors.network_error')
}
