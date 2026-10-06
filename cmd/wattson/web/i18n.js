// Minimal i18n: no framework, one JSON file per locale. Detects the
// system/browser language, falls back to English, and lets the viewer
// override it via a dropdown persisted in localStorage.

const SUPPORTED_LOCALES = ['en', 'it'];
const DEFAULT_LOCALE = 'en';
const STORAGE_KEY = 'wattson_locale';
const INTL_TAG = { en: 'en-US', it: 'it-IT' };

// Browsers don't expose the OS's regional format (12/24h clock, day/month
// order) to JS: Intl just follows the UI language, so an English Chrome in
// Italy would format like the US. The time zone is the signal available
// instead: English means en-US in a 12-hour-clock region, en-GB (24h,
// day/month) anywhere else.
const TWELVE_HOUR_ZONES = /^(US\/|Canada\/|America\/(New_York|Chicago|Denver|Los_Angeles|Phoenix|Anchorage|Juneau|Sitka|Nome|Adak|Boise|Detroit|Indiana\/|Kentucky\/|North_Dakota\/|Menominee|Toronto|Vancouver|Edmonton|Winnipeg|Regina|Halifax|St_Johns|Moncton|Whitehorse|Yellowknife)|Pacific\/Honolulu|Australia\/|Pacific\/Auckland|Asia\/(Kolkata|Calcutta|Karachi|Dhaka|Manila)|Africa\/Cairo)/;

function regionalEnglishTag() {
  try {
    return TWELVE_HOUR_ZONES.test(Intl.DateTimeFormat().resolvedOptions().timeZone) ? 'en-US' : 'en-GB';
  } catch (e) {
    return 'en-US';
  }
}

let translations = {};
let currentLocale = DEFAULT_LOCALE;

function detectLocale() {
  try {
    const stored = localStorage.getItem(STORAGE_KEY);
    if (stored && SUPPORTED_LOCALES.includes(stored)) return stored;
  } catch (e) {
    // localStorage unavailable (private mode, etc.): fall through to system detection
  }
  const nav = (navigator.language || DEFAULT_LOCALE).slice(0, 2).toLowerCase();
  return SUPPORTED_LOCALES.includes(nav) ? nav : DEFAULT_LOCALE;
}

async function loadLocale(locale) {
  const res = await fetch(`locales/${locale}.json`);
  translations = await res.json();
  currentLocale = locale;
}

function t(key, params) {
  let str = translations[key] || key;
  if (params) {
    for (const [k, v] of Object.entries(params)) str = str.replace(`{{${k}}}`, v);
  }
  return str;
}

function applyStaticTranslations() {
  document.documentElement.lang = currentLocale;
  document.querySelectorAll('[data-i18n]').forEach((el) => {
    el.textContent = t(el.dataset.i18n);
  });
  document.querySelectorAll('[data-i18n-placeholder]').forEach((el) => {
    el.placeholder = t(el.dataset.i18nPlaceholder);
  });
}

async function setLocale(locale) {
  await loadLocale(locale);
  try {
    localStorage.setItem(STORAGE_KEY, locale);
  } catch (e) {
    // best-effort persistence only
  }
  applyStaticTranslations();
}

async function init() {
  await loadLocale(detectLocale());
  applyStaticTranslations();
}

window.i18n = {
  t,
  setLocale,
  init,
  intlTag: () => (currentLocale === 'en' ? regionalEnglishTag() : INTL_TAG[currentLocale] || INTL_TAG[DEFAULT_LOCALE]),
  get locale() {
    return currentLocale;
  },
  SUPPORTED_LOCALES,
};
