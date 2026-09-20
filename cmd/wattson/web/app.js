// Vanilla JS, no build step: fetch + Chart.js via CDN.

const css = getComputedStyle(document.documentElement);
const color = (name) => css.getPropertyValue(name).trim();

const CATEGORY_COLORS = {
  system: () => color('--cat-system'),
  user: () => color('--cat-user'),
  unknown: () => color('--cat-unknown'),
};
// Fixed category keys (system/user/unknown) come from the backend; only the
// displayed label is localized.
const categoryLabel = (c) => i18n.t(`category.${c}`) || c;

let currentRange = 'today';
let customFrom = null, customUntil = null; // YYYY-MM-DD, both set together or not at all
let powerChart, cpuChart, categoryChart;
let livePowerChart, liveCpuChart;
let lastPowerTs = null;

// A custom date range (from the two date inputs) takes over from the
// preset buttons entirely; clearing it falls back to the active preset.
function rangeToUnix() {
  if (customFrom) {
    const from = Math.floor(new Date(`${customFrom}T00:00:00`).getTime() / 1000);
    const to = customUntil ? Math.floor(new Date(`${customUntil}T23:59:59`).getTime() / 1000) : Math.floor(Date.now() / 1000);
    return { from, to };
  }
  const now = Math.floor(Date.now() / 1000);
  const days = { today: 1, week: 7, month: 30 }[currentRange] ?? 1;
  return { from: now - days * 86400, to: now };
}

async function api(path) {
  const res = await fetch(path, { credentials: 'same-origin' });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || `error ${res.status}`);
  }
  return res.json();
}

function fmtEur(v) {
  return new Intl.NumberFormat(i18n.intlTag(), { style: 'currency', currency: 'EUR' }).format(v);
}
function fmtWatts(v) {
  return `${Math.round(v)} W`;
}
function fmtKwh(v) {
  return `${v.toFixed(2)} kWh`;
}

// --- KPI row -----------------------------------------------------------

async function loadKpis() {
  try {
    const current = await api('/api/v1/power/current');
    const el = document.getElementById('kpiPower');
    el.innerHTML = `${fmtWatts(current.watts)}<span class="pulse-dot" id="kpiPulse" title="Updates when a new reading arrives"></span>`;
    if (current.stale) {
      el.innerHTML += ` <span class="badge-stale">${i18n.t('badge.stale')}</span>`;
    }
    // Only flash on an actual new reading (a new ts), not on every 30s poll
    // that happens to see the same stale sample again.
    if (current.ts !== lastPowerTs) {
      lastPowerTs = current.ts;
      const dot = document.getElementById('kpiPulse');
      dot.classList.add('pulse-flash');
      dot.addEventListener('animationend', () => dot.classList.remove('pulse-flash'), { once: true });
    }
  } catch (e) {
    document.getElementById('kpiPower').textContent = i18n.t('price.na');
  }

  try {
    const summary = await api('/api/v1/power/summary');
    renderSummaryTile('kpiCostToday', 'kpiKwhToday', summary.last24h);
    renderSummaryTile('kpiCostMonth', 'kpiKwhMonth', summary.month);

    // "Cost this month" is the current calendar month, not a rolling 30-day
    // window: naming the month explicitly avoids that ambiguity.
    const monthName = new Date(summary.month_start * 1000).toLocaleDateString(i18n.intlTag(), { month: 'long', year: 'numeric' });
    document.getElementById('kpiCostMonthLabel').textContent = i18n.t('kpi.cost_month', { month: monthName });

    const dayOfMonth = new Date().getDate();
    const avg = summary.month.cost_eur / Math.max(dayOfMonth, 1);
    document.getElementById('kpiDailyAvg').textContent = summary.month.complete ? fmtEur(avg) : `~${fmtEur(avg)}`;

    renderCurrentPrice(summary.current_price);
  } catch (e) {
    // don't silently blank the UI
    document.getElementById('kpiCostToday').textContent = i18n.t('price.na');
    document.getElementById('kpiCostMonth').textContent = i18n.t('price.na');
    document.getElementById('kpiCurrentPrice').textContent = i18n.t('price.na');
  }
}

function renderSummaryTile(costId, kwhId, period) {
  const costEl = document.getElementById(costId);
  costEl.textContent = fmtEur(period.cost_eur);
  if (!period.complete) costEl.innerHTML += ` <span class="badge-stale">${i18n.t('badge.incomplete')}</span>`;
  else if (period.provisional) costEl.innerHTML += ` <span class="badge-stale" style="background:var(--cat-user)">${i18n.t('badge.estimate')}</span>`;
  document.getElementById(kwhId).textContent = fmtKwh(period.kwh);
}

// The applied price is never a single "PUN": it's the monthly PUN average
// (real for a concluded month, estimated from the previous month/period
// otherwise) plus a spread, or a custom period. Showing it explicitly
// answers "what's the price right now?", which the rest of the UI didn't.
function renderCurrentPrice(price) {
  const el = document.getElementById('kpiCurrentPrice');
  const sub = document.getElementById('kpiCurrentPriceSub');
  if (!price || !price.complete) {
    el.textContent = i18n.t('price.na');
    sub.textContent = i18n.t('price.no_pun');
    return;
  }
  el.textContent = `${price.eur_per_kwh.toFixed(4)} €/kWh`;
  const sourceLabel = i18n.t(`price.source.${price.source}`) || price.source;
  sub.textContent = price.provisional ? i18n.t('price.provisional_suffix', { source: sourceLabel }) : sourceLabel;
}

// --- Power/energy charts -------------------------------------------------

// No browser API exposes the OS's actual 12h/24h clock preference (that's
// deliberately withheld, a fingerprinting concern) — the closest available
// signal is the locale's own convention via Intl. This drives the chart
// axis ticks; toLocaleString() calls elsewhere already follow it natively.
function use24Hour() {
  try {
    const cycle = new Intl.DateTimeFormat(i18n.intlTag(), { hour: 'numeric' }).resolvedOptions().hourCycle;
    return cycle === 'h23' || cycle === 'h24';
  } catch (e) {
    return i18n.locale === 'it';
  }
}

// Mirrors a zoomed/panned x-axis range onto the paired chart (set via
// chart._pairChart after both are created) — otherwise zooming the power
// chart alone would desync it from the CPU chart it's meant to line up with.
function syncZoomedRange(chart) {
  const target = chart._pairChart;
  if (!target) return;
  target.options.scales.x.min = chart.scales.x.min;
  target.options.scales.x.max = chart.scales.x.max;
  target.update('none');
}

// zoomable: wheel/pinch-to-zoom + drag-to-pan on the x (time) axis only —
// only worth enabling on charts whose data isn't replaced every few
// seconds (the live charts redraw every 10s poll, so any zoom on them
// would just get reset before anyone could use it).
function baseLineOptions(unitLabel, timeUnit = 'hour', zoomable = false) {
  return {
    responsive: true,
    maintainAspectRatio: false,
    interaction: { mode: 'index', intersect: false },
    plugins: {
      legend: { display: false }, // single series: the card title already names it
      tooltip: {
        callbacks: {
          label: (ctx) => `${unitLabel}: ${ctx.parsed.y.toFixed(2)}`,
          title: (items) => new Date(items[0].parsed.x).toLocaleString(i18n.intlTag()),
        },
      },
      ...(zoomable ? {
        zoom: {
          limits: { x: { min: 'original', max: 'original' } },
          pan: { enabled: true, mode: 'x', onPanComplete: ({ chart }) => syncZoomedRange(chart) },
          zoom: {
            wheel: { enabled: true },
            pinch: { enabled: true },
            mode: 'x',
            onZoomComplete: ({ chart }) => syncZoomedRange(chart),
          },
        },
      } : {}),
    },
    scales: {
      x: {
        type: 'time',
        time: {
          unit: timeUnit,
          // The date-fns adapter's default tick formats are hardcoded to
          // 12h ("ha"/"h:mm a"); without this override, ticks stay in
          // English AM/PM even when the rest of the UI is in Italian.
          displayFormats: use24Hour() ? { hour: 'HH:mm', minute: 'HH:mm' } : { hour: 'h a', minute: 'h:mm a' },
        },
        grid: { color: color('--gridline'), drawTicks: false },
        ticks: { color: color('--text-muted'), maxRotation: 0 },
      },
      y: {
        grid: { color: color('--gridline'), drawTicks: false },
        ticks: { color: color('--text-muted') },
        border: { color: color('--baseline') },
      },
    },
  };
}

// --- Synced hover crosshair between a power/CPU chart pair ----------------
//
// Chart.js has no built-in way to sync tooltips across two chart instances.
// Listeners are attached once, on the canvas elements, and read the current
// chart instance through a getter — charts get destroyed/recreated often
// (range switch, live poll), a canvas-bound listener would otherwise pile
// up duplicates or point at a stale, destroyed chart.
function linkChartsHover(canvasA, getChartA, canvasB, getChartB) {
  function nearestIndex(chart, xVal) {
    const data = chart.data.datasets[0]?.data || [];
    let idx = -1, min = Infinity;
    for (let i = 0; i < data.length; i++) {
      const d = Math.abs(data[i].x - xVal);
      if (d < min) { min = d; idx = i; }
    }
    return idx;
  }
  function setActive(chart, idx) {
    if (!chart) return;
    const active = idx < 0 ? [] : [{ datasetIndex: 0, index: idx }];
    chart.tooltip.setActiveElements(active, { x: 0, y: 0 });
    chart.setActiveElements(active);
    chart.update('none');
  }
  function onMove(sourceCanvas, sourceGetter, targetGetter, evt) {
    const source = sourceGetter();
    const target = targetGetter();
    if (!source || !target) return;
    const rect = sourceCanvas.getBoundingClientRect();
    const xVal = source.scales.x.getValueForPixel(evt.clientX - rect.left);
    if (xVal == null) return;
    setActive(target, nearestIndex(target, xVal));
  }
  canvasA.addEventListener('mousemove', (e) => onMove(canvasA, getChartA, getChartB, e));
  canvasB.addEventListener('mousemove', (e) => onMove(canvasB, getChartB, getChartA, e));
  canvasA.addEventListener('mouseleave', () => setActive(getChartB(), -1));
  canvasB.addEventListener('mouseleave', () => setActive(getChartA(), -1));
}

async function loadCharts() {
  const { from, to } = rangeToUnix();
  let points = [];
  try {
    points = await api(`/api/v1/power/history?from=${from}&to=${to}`);
  } catch (e) {
    points = [];
  }

  const powerData = points.map((p) => ({ x: p.bucket_start * 1000, y: p.watts_avg }));
  const cpuData = points.map((p) => ({ x: p.bucket_start * 1000, y: p.cpu_avg_pct }));

  powerChart?.destroy();
  powerChart = new Chart(document.getElementById('powerChart'), {
    type: 'line',
    data: {
      datasets: [{
        data: powerData,
        borderColor: color('--power-line'),
        backgroundColor: hexToRgba(color('--power-line'), 0.1),
        borderWidth: 2,
        pointRadius: 0,
        pointHoverRadius: 5,
        pointBackgroundColor: color('--power-line'),
        fill: true,
        tension: 0.15,
      }],
    },
    options: baseLineOptions('W', 'hour', true),
  });

  const cpuOptions = baseLineOptions('%', 'hour', true);
  cpuChart?.destroy();
  cpuChart = new Chart(document.getElementById('cpuChart'), {
    type: 'line',
    data: {
      datasets: [{
        data: cpuData,
        borderColor: color('--cpu-line'),
        backgroundColor: hexToRgba(color('--cpu-line'), 0.1),
        borderWidth: 2,
        pointRadius: 0,
        pointHoverRadius: 5,
        pointBackgroundColor: color('--cpu-line'),
        fill: true,
        tension: 0.15,
      }],
    },
    options: { ...cpuOptions, scales: { ...cpuOptions.scales, y: { ...cpuOptions.scales.y, min: 0 } } },
  });

  // Zoom/pan on either chart mirrors onto the other (see syncZoomedRange).
  powerChart._pairChart = cpuChart;
  cpuChart._pairChart = powerChart;
}

// --- Live (raw, ~10s) charts ----------------------------------------------

// Builds a chart on first call; on every later call for the same `existing`
// instance, updates its data in place instead of destroy()/new Chart(). The
// live charts poll every 10s — recreating them each time briefly cleared
// the canvas before redrawing, which read as a "blink" rather than a
// smoothly scrolling live chart.
function upsertLineChart(existing, canvasId, data, lineColor, options) {
  if (existing) {
    existing.data.datasets[0].data = data;
    existing.update('none');
    return existing;
  }
  return new Chart(document.getElementById(canvasId), {
    type: 'line',
    data: {
      datasets: [{
        data,
        borderColor: lineColor,
        backgroundColor: hexToRgba(lineColor, 0.1),
        borderWidth: 2,
        pointRadius: 0,
        pointHoverRadius: 5,
        pointBackgroundColor: lineColor,
        fill: true,
        tension: 0.15,
      }],
    },
    options,
  });
}

async function loadLive() {
  let data = { power: [], cpu: [] };
  try {
    data = await api('/api/v1/power/live');
  } catch (e) {
    return; // keep whatever was last rendered rather than clearing it
  }

  const powerData = (data.power || []).map((p) => ({ x: p.ts * 1000, y: p.value }));
  const cpuData = (data.cpu || []).map((p) => ({ x: p.ts * 1000, y: p.value }));

  livePowerChart = upsertLineChart(livePowerChart, 'livePowerChart', powerData, color('--power-line'), baseLineOptions('W', 'minute'));

  const cpuOptions = baseLineOptions('%', 'minute');
  liveCpuChart = upsertLineChart(liveCpuChart, 'liveCpuChart', cpuData, color('--cpu-line'),
    { ...cpuOptions, scales: { ...cpuOptions.scales, y: { ...cpuOptions.scales.y, min: 0 } } });
}

function hexToRgba(hex, alpha) {
  const h = hex.replace('#', '');
  const bigint = parseInt(h, 16);
  const r = (bigint >> 16) & 255, g = (bigint >> 8) & 255, b = bigint & 255;
  return `rgba(${r}, ${g}, ${b}, ${alpha})`;
}

// --- Category/container breakdown ----------------------------------------

async function loadBreakdown() {
  const { from, to } = rangeToUnix();
  let data = { by_category: [], by_container: [] };
  try {
    data = await api(`/api/v1/power/attribution?from=${from}&to=${to}`);
  } catch (e) {
    // leave the sections empty rather than breaking the rest of the page
  }

  const total = data.by_category.reduce((s, c) => s + c.kwh_allocated, 0) || 1;
  const order = ['system', 'user', 'unknown'];
  const byCat = Object.fromEntries(data.by_category.map((c) => [c.category, c.kwh_allocated]));

  document.getElementById('categoryLegend').innerHTML = order
    .filter((c) => byCat[c] !== undefined)
    .map((c) => `<span><span class="legend-swatch" style="background:${CATEGORY_COLORS[c]()}"></span>${categoryLabel(c)} &mdash; ${fmtKwh(byCat[c])} (${((byCat[c] / total) * 100).toFixed(0)}%)</span>`)
    .join('');

  categoryChart?.destroy();
  categoryChart = new Chart(document.getElementById('categoryChart'), {
    type: 'bar',
    data: {
      labels: [i18n.t('table.energy_allocated')],
      datasets: order
        .filter((c) => byCat[c] !== undefined)
        .map((c) => ({
          label: categoryLabel(c),
          data: [byCat[c]],
          backgroundColor: CATEGORY_COLORS[c](),
          borderRadius: 4,
        })),
    },
    options: {
      indexAxis: 'y',
      responsive: true,
      maintainAspectRatio: false,
      scales: {
        x: { stacked: true, grid: { color: color('--gridline') }, ticks: { color: color('--text-muted') } },
        y: { stacked: true, grid: { display: false }, ticks: { display: false } },
      },
      plugins: { legend: { display: false }, tooltip: { callbacks: { label: (ctx) => `${ctx.dataset.label}: ${fmtKwh(ctx.parsed.x)}` } } },
    },
  });

  const tbody = document.getElementById('containerTableBody');
  tbody.innerHTML = data.by_container.map((c) => `
    <tr>
      <td><span class="legend-swatch" style="background:${(CATEGORY_COLORS[c.category] || CATEGORY_COLORS.unknown)()}"></span>${c.container === '__baseline__' ? i18n.t('table.baseline_row') : c.container}</td>
      <td>${categoryLabel(c.category)}</td>
      <td class="num">${fmtKwh(c.kwh_allocated)}</td>
      <td class="num">${((c.kwh_allocated / total) * 100).toFixed(1)}%</td>
    </tr>
  `).join('') || `<tr><td colspan="4" style="color:var(--text-muted)">${i18n.t('table.no_data')}</td></tr>`;
}

// --- Range picker --------------------------------------------------------

document.getElementById('rangePicker').addEventListener('click', (e) => {
  const btn = e.target.closest('button[data-range]');
  if (!btn) return;
  document.querySelectorAll('#rangePicker button[data-range]').forEach((b) => b.classList.remove('active'));
  btn.classList.add('active');
  currentRange = btn.dataset.range;
  customFrom = customUntil = null;
  document.getElementById('customFrom').value = '';
  document.getElementById('customUntil').value = '';
  loadCharts();
  loadBreakdown();
});

// A custom "from" date overrides the preset entirely; "until" defaults to
// now if left empty (an open-ended custom range).
['customFrom', 'customUntil'].forEach((id) => {
  document.getElementById(id).addEventListener('change', () => {
    customFrom = document.getElementById('customFrom').value || null;
    customUntil = document.getElementById('customUntil').value || null;
    if (!customFrom) return;
    document.querySelectorAll('#rangePicker button[data-range]').forEach((b) => b.classList.remove('active'));
    loadCharts();
    loadBreakdown();
  });
});

document.getElementById('resetZoomBtn').addEventListener('click', () => {
  powerChart?.resetZoom();
  cpuChart?.resetZoom();
});

// --- Pricing periods -------------------------------------------------------

async function loadPeriods() {
  let periods = [];
  try {
    periods = (await api('/api/v1/pricing/periods')) || [];
  } catch (e) {
    return;
  }
  const tbody = document.getElementById('periodsTableBody');
  tbody.innerHTML = periods.map((p) => `
    <tr>
      <td>${p.ValidFrom}</td>
      <td>${p.ValidUntil || '&mdash;'}</td>
      <td>${p.Mode === 'fixed_override' ? i18n.t('mode.fixed') : i18n.t('mode.spread')}</td>
      <td class="num">${p.Mode === 'fixed_override' ? fmtEur(p.FixedPrice) : `+${p.SpreadValue.toFixed(3)} &euro;`}</td>
      <td>${p.Note || ''}</td>
      <td><button class="icon-btn" data-edit="${p.ID}">${i18n.t('action.edit')}</button> &middot; <button class="icon-btn" data-delete="${p.ID}">${i18n.t('action.delete')}</button></td>
    </tr>
  `).join('') || `<tr><td colspan="6" style="color:var(--text-muted)">${i18n.t('table.no_periods')}</td></tr>`;

  tbody.querySelectorAll('[data-delete]').forEach((btn) => {
    btn.addEventListener('click', async () => {
      if (!confirm(i18n.t('confirm.delete_period'))) return;
      await fetch(`/api/v1/pricing/periods/${btn.dataset.delete}`, { method: 'DELETE' });
      loadPeriods();
      refreshAfterPricingChange();
    });
  });
  tbody.querySelectorAll('[data-edit]').forEach((btn) => {
    btn.addEventListener('click', () => {
      const p = periods.find((x) => String(x.ID) === btn.dataset.edit);
      if (p) fillForm(p);
    });
  });
}

// The create/edit form stays hidden until needed: it used to always show
// next to the table, confusing "a period is active" with "you can add one".
const periodFormCard = document.getElementById('periodFormCard');

function showPeriodForm() {
  periodFormCard.style.display = 'block';
  periodFormCard.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
}
function hidePeriodForm() {
  periodFormCard.style.display = 'none';
  document.getElementById('periodForm').reset();
  document.getElementById('periodId').value = '';
  document.getElementById('periodFormError').textContent = '';
  document.getElementById('previewBox').style.display = 'none';
  toggleModeFields();
}

document.getElementById('addPeriodBtn').addEventListener('click', () => {
  hidePeriodForm();
  showPeriodForm();
});
document.getElementById('cancelPeriodBtn').addEventListener('click', hidePeriodForm);

function fillForm(p) {
  document.getElementById('periodId').value = p.ID;
  document.getElementById('validFrom').value = p.ValidFrom;
  document.getElementById('validUntil').value = p.ValidUntil || '';
  document.getElementById('mode').value = p.Mode;
  document.getElementById('spreadValue').value = p.SpreadValue || 0.10;
  document.getElementById('fixedPrice').value = p.FixedPrice || 0.25;
  document.getElementById('note').value = p.Note || '';
  toggleModeFields();
  showPeriodForm();
}

function toggleModeFields() {
  const isFixed = document.getElementById('mode').value === 'fixed_override';
  document.getElementById('spreadField').style.display = isFixed ? 'none' : 'block';
  document.getElementById('fixedField').style.display = isFixed ? 'block' : 'none';
}
document.getElementById('mode').addEventListener('change', () => { toggleModeFields(); schedulePreview(); });

document.getElementById('periodForm').addEventListener('submit', async (e) => {
  e.preventDefault();
  const id = document.getElementById('periodId').value;
  const mode = document.getElementById('mode').value;
  const body = {
    valid_from: document.getElementById('validFrom').value,
    valid_until: document.getElementById('validUntil').value,
    mode,
    spread_value: parseFloat(document.getElementById('spreadValue').value) || 0,
    fixed_price: parseFloat(document.getElementById('fixedPrice').value) || 0,
    note: document.getElementById('note').value,
  };
  const errEl = document.getElementById('periodFormError');
  errEl.textContent = '';
  try {
    const res = await fetch(id ? `/api/v1/pricing/periods/${id}` : '/api/v1/pricing/periods', {
      method: id ? 'PUT' : 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!res.ok) {
      const data = await res.json().catch(() => ({}));
      throw new Error(data.error || i18n.t('form.save_failed'));
    }
    hidePeriodForm();
    loadPeriods();
    refreshAfterPricingChange();
  } catch (err) {
    errEl.textContent = err.message;
  }
});

// Creating/editing/deleting a period changes the resolved price for hours
// already shown (KPIs, energy chart, breakdown): without this the viewer
// saw stale numbers until the next periodic loadKpis refresh (30s).
function refreshAfterPricingChange() {
  loadKpis();
  loadCharts();
}

// Live preview: recomputed as the viewer edits, without saving (debounced).
let previewTimer;
function schedulePreview() {
  clearTimeout(previewTimer);
  previewTimer = setTimeout(runPreview, 300);
}
['validFrom', 'validUntil', 'spreadValue', 'fixedPrice'].forEach((id) => {
  document.getElementById(id).addEventListener('input', schedulePreview);
});

async function runPreview() {
  const from = document.getElementById('validFrom').value;
  const to = document.getElementById('validUntil').value || from;
  const box = document.getElementById('previewBox');
  if (!from) { box.style.display = 'none'; return; }

  const mode = document.getElementById('mode').value;
  const param = mode === 'fixed_override'
    ? `fixed_price=${document.getElementById('fixedPrice').value}`
    : `spread=${document.getElementById('spreadValue').value}`;

  try {
    const result = await api(`/api/v1/pricing/preview?from=${from}&to=${to}&${param}`);
    box.style.display = 'block';
    document.getElementById('previewCost').textContent = result.complete ? fmtEur(result.cost_eur) : `${fmtEur(result.cost_eur)} ${i18n.t('preview.incomplete_suffix')}`;
    document.getElementById('previewKwh').textContent = fmtKwh(result.kwh);
  } catch (e) {
    box.style.display = 'none';
  }
}

// --- Language switcher -----------------------------------------------------

function loadAll() {
  loadKpis();
  loadLive();
  loadCharts();
  loadBreakdown();
  loadPeriods();
}

document.getElementById('localeSwitcher').addEventListener('change', async (e) => {
  await i18n.setLocale(e.target.value);
  // The live charts are updated in place (not recreated) on every poll to
  // avoid a flicker — but that means a locale change wouldn't otherwise
  // reach their axis tick format until torn down and rebuilt once here.
  livePowerChart?.destroy();
  liveCpuChart?.destroy();
  livePowerChart = liveCpuChart = null;
  loadAll(); // re-render dynamic content (KPIs, table rows) in the new language
});

// --- startup -----------------------------------------------------------------

async function main() {
  await i18n.init();
  document.getElementById('localeSwitcher').value = i18n.locale;

  linkChartsHover(document.getElementById('powerChart'), () => powerChart, document.getElementById('cpuChart'), () => cpuChart);
  linkChartsHover(document.getElementById('livePowerChart'), () => livePowerChart, document.getElementById('liveCpuChart'), () => liveCpuChart);

  loadAll();
  setInterval(loadKpis, 30000); // "current power" KPI + freshness pulse
  setInterval(loadLive, 10000); // matches the wattmeter's ~10s publish interval
}
main();
