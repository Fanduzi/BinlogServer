import { createI18n } from "vue-i18n";
import zhCN from "./zh-CN.json";
import en from "./en.json";

const SAVED_LOCALE_KEY = "locale";

/**
 * Get saved locale from localStorage, fallback to browser language or zh-CN
 */
function getDefaultLocale() {
  const saved = localStorage.getItem(SAVED_LOCALE_KEY);
  if (saved && ["zh-CN", "en"].includes(saved)) {
    return saved;
  }

  // Try to detect browser language
  const browserLang = navigator.language || navigator.userLanguage;
  if (browserLang && browserLang.startsWith("zh")) {
    return "zh-CN";
  }

  return "zh-CN";
}

export const i18n = createI18n({
  legacy: false, // Use Composition API
  locale: getDefaultLocale(),
  fallbackLocale: "en",
  messages: {
    "zh-CN": zhCN,
    en,
  },
});

/**
 * Set current locale and persist to localStorage
 * @param {string} locale - Locale code ('zh-CN' or 'en')
 */
export function setLocale(locale) {
  if (!["zh-CN", "en"].includes(locale)) {
    console.warn(`Invalid locale: ${locale}`);
    return;
  }
  i18n.global.locale.value = locale;
  localStorage.setItem(SAVED_LOCALE_KEY, locale);
  document.documentElement.lang = locale === "zh-CN" ? "zh-CN" : "en";
}

/**
 * Get current locale
 * @returns {string} Current locale code
 */
export function getLocale() {
  return i18n.global.locale.value;
}
