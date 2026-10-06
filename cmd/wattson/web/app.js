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
let customRange = null; // {from, to} Dates from the range picker, or null
let historyChart, costChart, categoryChart;
let liveChart;
let rangePicker;
let lastPowerTs = null;

// A custom range (from the range picker) takes over from the presets
// entirely; picking a preset clears it. The presets are rolling windows
// ending now, e.g. "Last 24h" is literally the last 24 hours, not "since
// local midnight".
function rangeToUnix() {
  const now = Math.floor(Date.now() / 1000);
  if (customRange) {
    return { from: Math.floor(customRange.from.getTime() / 1000), to: Math.min(now, Math.floor(customRange.to.getTime() / 1000)) };
  }
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
  return `${v.toLocaleString(i18n.intlTag(), { minimumFractionDigits: 2, maximumFractionDigits: 2 })} kWh`;
}

// --- KPI row -----------------------------------------------------------

// Drives the "Current power" tile from the live feed (polled every 2s, see
// loadLive) instead of a separate /power/current poll, so the freshness
// pulse flashes at the same cadence as the live chart actually updates.
function updateCurrentPowerKpi(tsSeconds, watts) {
  const el = document.getElementById('kpiPower');
  const stale = (Date.now() / 1000 - tsSeconds) > 120;
  el.innerHTML = `${fmtWatts(watts)}<span class="pulse-dot" id="kpiPulse" title="Updates when a new reading arrives"></span>`;
  if (stale) el.innerHTML += ` <span class="badge-stale">${i18n.t('badge.stale')}</span>`;
  if (tsSeconds !== lastPowerTs) {
    lastPowerTs = tsSeconds;
    const dot = document.getElementById('kpiPulse');
    dot.classList.add('pulse-flash');
    dot.addEventListener('animationend', () => dot.classList.remove('pulse-flash'), { once: true });
  }
}

async function loadKpis() {
  try {
    const summary = await api('/api/v1/power/summary');
    renderSummaryTile('kpiCostToday', 'kpiKwhToday', summary.last24h);
    renderSummaryTile('kpiCostMonth', 'kpiKwhMonth', summary.month);

    // Name the month explicitly: it's the calendar month, not a rolling 30 days.
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

function renderCurrentPrice(price) {
  const el = document.getElementById('kpiCurrentPrice');
  const sub = document.getElementById('kpiCurrentPriceSub');
  if (!price || !price.complete) {
    el.textContent = i18n.t('price.na');
    sub.textContent = i18n.t('price.no_pun');
    return;
  }
  el.textContent = `${price.eur_per_kwh.toLocaleString(i18n.intlTag(), { minimumFractionDigits: 4, maximumFractionDigits: 4 })} €/kWh`;
  const sourceLabel = i18n.t(`price.source.${price.source}`) || price.source;
  sub.textContent = price.provisional ? i18n.t('price.provisional_suffix', { source: sourceLabel }) : sourceLabel;
}

// --- Power/CPU and cost charts --------------------------------------------

// One series as a [min, max, avg] trio: max fills back to min ('-1') for
// the shaded band, avg draws on top. Only avg (_avg) shows in the legend
// and tooltip, with the band's min–max read from the two datasets before it.
function seriesTrio(s, lineColor, yAxisID, label, unit) {
  const base = { yAxisID, pointRadius: 0, _unit: unit };
  return [
    { ...base, data: s.min, borderWidth: 0, fill: false },
    { ...base, data: s.max, borderWidth: 0, backgroundColor: hexToRgba(lineColor, 0.12), fill: '-1' },
    {
      ...base, _avg: true, label, data: s.avg, borderColor: lineColor, backgroundColor: lineColor,
      borderWidth: 2, pointHoverRadius: 5, pointBackgroundColor: lineColor, fill: false, tension: 0.15,
    },
  ];
}

// Power on the left axis (W), CPU on the right (%), same time axis.
function powerCpuDatasets(power, cpu) {
  return [
    ...seriesTrio(power, color('--power-line'), 'y', i18n.t('chart.power'), 'W'),
    ...seriesTrio(cpu, color('--cpu-line'), 'y1', i18n.t('chart.cpu'), '%'),
  ];
}

// No browser API exposes the OS's 12h/24h preference; fall back to the
// locale's own convention via Intl.
function use24Hour() {
  try {
    const cycle = new Intl.DateTimeFormat(i18n.intlTag(), { hour: 'numeric' }).resolvedOptions().hourCycle;
    return cycle === 'h23' || cycle === 'h24';
  } catch (e) {
    return i18n.locale === 'it';
  }
}

// No fixed `unit`: Chart.js auto-picks it from the visible (possibly
// zoomed) span, so ticks adapt when zooming in.
function timeAxis() {
  return {
    type: 'time',
    time: {
      displayFormats: use24Hour()
        ? { second: 'HH:mm:ss', minute: 'HH:mm', hour: 'HH:mm', day: 'MMM d' }
        : { second: 'h:mm:ss a', minute: 'h:mm a', hour: 'h a', day: 'MMM d' },
    },
    grid: { color: color('--gridline'), drawTicks: false },
    ticks: { color: color('--text-muted'), maxRotation: 0, autoSkipPadding: 24 },
  };
}

// Always anchored at 0 (beginAtZero still extends below it if a reading
// is ever negative, which would point at a data problem).
function valueAxis(extra = {}) {
  return {
    beginAtZero: true,
    grid: { color: color('--gridline'), drawTicks: false },
    ticks: { color: color('--text-muted') },
    border: { color: color('--baseline') },
    ...extra,
  };
}

const fmtNum = (v) => (v == null ? '–' : v.toLocaleString(i18n.intlTag(), { maximumFractionDigits: 1 }));

// Deepest zoom allowed: below this span the minute-level rollup has
// nothing more to show anyway.
const MIN_ZOOM_RANGE_MS = 30 * 60 * 1000;

// Mirrors the history chart's zoomed/panned x range onto the cost chart.
function syncCostRange(chart) {
  if (!costChart) return;
  costChart.options.scales.x.min = chart.scales.x.min;
  costChart.options.scales.x.max = chart.scales.x.max;
  costChart.update('none');
}

// range: {from, to} in ms the x axis is pinned to. Zoom only on history:
// the live chart redraws every 2s.
function powerCpuOptions(range, zoomable) {
  const x = timeAxis();
  x.min = range.from;
  x.max = range.to;
  return {
    responsive: true,
    maintainAspectRatio: false,
    interaction: { mode: 'nearest', axis: 'x', intersect: false },
    plugins: {
      legend: {
        align: 'end',
        labels: {
          color: color('--text-secondary'), boxWidth: 12, boxHeight: 2,
          filter: (item, data) => data.datasets[item.datasetIndex]._avg,
        },
      },
      tooltip: {
        filter: (item) => item.dataset._avg,
        callbacks: {
          title: (items) => new Date(items[0].parsed.x).toLocaleString(i18n.intlTag()),
          label: (ctx) => {
            const ds = ctx.chart.data.datasets, i = ctx.datasetIndex, j = ctx.dataIndex;
            const unit = ctx.dataset._unit;
            return `${ctx.dataset.label}: ${fmtNum(ctx.parsed.y)} ${unit} (${fmtNum(ds[i - 2].data[j]?.y)}–${fmtNum(ds[i - 1].data[j]?.y)})`;
          },
        },
      },
      ...(zoomable ? {
        zoom: {
          limits: { x: { min: 'original', max: 'original', minRange: MIN_ZOOM_RANGE_MS } },
          pan: { enabled: true, mode: 'x', onPanComplete: ({ chart }) => syncCostRange(chart) },
          zoom: { wheel: { enabled: true }, pinch: { enabled: true }, mode: 'x', onZoomComplete: ({ chart }) => syncCostRange(chart) },
        },
      } : {}),
    },
    scales: {
      x,
      y: valueAxis({ title: { display: true, text: 'W', color: color('--text-muted') } }),
      y1: valueAxis({ position: 'right', grid: { drawOnChartArea: false }, title: { display: true, text: '%', color: color('--text-muted') } }),
    },
  };
}

async function loadCharts() {
  const { from, to } = rangeToUnix();
  const [points, cost] = await Promise.all([
    api(`/api/v1/power/history?from=${from}&to=${to}`).catch(() => []),
    api(`/api/v1/power/cost?from=${from}&to=${to}`).catch(() => null),
  ]);

  const trio = (min, max, avg) => {
    const s = (f) => points.map((p) => ({ x: p.bucket_start * 1000, y: p[f] }));
    return { min: s(min), max: s(max), avg: s(avg) };
  };
  historyChart?.destroy();
  historyChart = new Chart(document.getElementById('historyChart'), {
    type: 'line',
    data: { datasets: powerCpuDatasets(trio('watts_min', 'watts_max', 'watts_avg'), trio('cpu_min_pct', 'cpu_max_pct', 'cpu_avg_pct')) },
    options: powerCpuOptions({ from: from * 1000, to: to * 1000 }, true),
  });

  renderCost(cost, from * 1000, to * 1000);
}

// Cost per hour (ranges up to a week) or per day, as bars spanning their
// interval, on the same x range as the power/CPU chart. The title carries
// the period total, flagged like the KPI tiles when estimated/incomplete.
function renderCost(cost, fromMs, toMs) {
  const totalEl = document.getElementById('costTotal');
  if (!cost) {
    totalEl.textContent = `— ${i18n.t('price.na')}`;
  } else {
    totalEl.textContent = `— ${fmtEur(cost.total.cost_eur)} · ${fmtKwh(cost.total.kwh)}`;
    if (!cost.total.complete) totalEl.innerHTML += ` <span class="badge-stale">${i18n.t('badge.incomplete')}</span>`;
    else if (cost.total.provisional) totalEl.innerHTML += ` <span class="badge-stale" style="background:var(--cat-user)">${i18n.t('badge.estimate')}</span>`;
  }

  const daily = cost?.bucket === 'day';
  const bucketMs = (daily ? 24 : 1) * 3600 * 1000;
  const x = timeAxis();
  x.min = fromMs;
  x.max = toMs;

  costChart?.destroy();
  costChart = new Chart(document.getElementById('costChart'), {
    type: 'bar',
    data: {
      datasets: [{
        data: (cost?.buckets || []).map((b) => ({ x: b.bucket_start * 1000 + bucketMs / 2, y: b.cost_eur, start: b.bucket_start * 1000, kwh: b.kwh })),
        backgroundColor: color('--cost-bar'),
        borderRadius: { topLeft: 4, topRight: 4 },
        borderSkipped: 'bottom',
        barPercentage: 0.85,
        categoryPercentage: 1,
      }],
    },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      interaction: { mode: 'nearest', axis: 'x', intersect: false },
      plugins: {
        legend: { display: false },
        tooltip: {
          callbacks: {
            title: (items) => {
              const d = new Date(items[0].raw.start);
              return daily
                ? d.toLocaleDateString(i18n.intlTag(), { weekday: 'short', day: 'numeric', month: 'short' })
                : d.toLocaleString(i18n.intlTag(), { day: 'numeric', month: 'short', hour: 'numeric', minute: '2-digit' });
            },
            label: (ctx) => `${fmtEur(ctx.parsed.y)} · ${fmtKwh(ctx.raw.kwh)}`,
          },
        },
      },
      scales: {
        x,
        // Hourly costs are fractions of a cent apart: 2 decimals would
        // repeat the same tick label.
        y: valueAxis({ ticks: { color: color('--text-muted'), callback: (v) => new Intl.NumberFormat(i18n.intlTag(), { style: 'currency', currency: 'EUR', maximumFractionDigits: 3 }).format(v) } }),
      },
    },
  });
}

// --- Live (raw, ~2s) charts ----------------------------------------------

// Creates a chart on first call; later calls update its datasets' data in
// place instead of destroy()/recreate, which used to read as a "blink"
// every poll.
function upsertChart(existing, canvasId, datasets, options) {
  if (existing) {
    datasets.forEach((ds, i) => { existing.data.datasets[i].data = ds.data; });
    existing.update('none');
    return existing;
  }
  return new Chart(document.getElementById(canvasId), { type: 'line', data: { datasets }, options });
}

const LIVE_WINDOW_MS = 15 * 60 * 1000;
const LIVE_BUCKET_MS = 15 * 1000; // groups raw ~2-10s samples for a min/max/avg band, same idea as the hourly charts

function pruneOlderThanWindow(points, nowMs) {
  while (points.length && nowMs - points[0].x > LIVE_WINDOW_MS) points.shift();
}

// Buckets raw {x,y} points into fixed-width time buckets and returns the
// {min, max, avg} series seriesTrio expects.
function bucketize(points, bucketMs) {
  const buckets = new Map();
  for (const p of points) {
    const key = Math.floor(p.x / bucketMs) * bucketMs;
    let b = buckets.get(key);
    if (!b) { b = { sum: 0, count: 0, min: p.y, max: p.y }; buckets.set(key, b); }
    b.sum += p.y;
    b.count += 1;
    b.min = Math.min(b.min, p.y);
    b.max = Math.max(b.max, p.y);
  }
  const keys = [...buckets.keys()].sort((a, b) => a - b);
  return {
    min: keys.map((k) => ({ x: k, y: buckets.get(k).min })),
    max: keys.map((k) => ({ x: k, y: buckets.get(k).max })),
    avg: keys.map((k) => ({ x: k, y: buckets.get(k).sum / buckets.get(k).count })),
  };
}

// Accumulated raw points for the live window; loadLive only fetches
// what's new since the last poll (see power_since/cpu_since) and appends
// here, instead of re-fetching the whole 15-minute window every 2s.
let livePowerRaw = [], liveCpuRaw = [];
let livePowerSince = null, liveCpuSince = null;

async function loadLive() {
  let data;
  try {
    const params = livePowerSince ? `?power_since=${livePowerSince}&cpu_since=${liveCpuSince}` : '';
    data = await api(`/api/v1/power/live${params}`);
  } catch (e) {
    return; // keep whatever was last rendered rather than clearing it
  }

  const newPower = (data.power || []).map((p) => ({ x: p.ts * 1000, y: p.value }));
  const newCpu = (data.cpu || []).map((p) => ({ x: p.ts * 1000, y: p.value }));
  livePowerRaw.push(...newPower);
  liveCpuRaw.push(...newCpu);

  const nowMs = Date.now();
  pruneOlderThanWindow(livePowerRaw, nowMs);
  pruneOlderThanWindow(liveCpuRaw, nowMs);

  const latestPower = livePowerRaw[livePowerRaw.length - 1];
  if (latestPower) {
    livePowerSince = Math.floor(latestPower.x / 1000);
    updateCurrentPowerKpi(livePowerSince, latestPower.y);
  } else if (!livePowerSince) {
    document.getElementById('kpiPower').textContent = i18n.t('price.na');
  }
  const latestCpu = liveCpuRaw[liveCpuRaw.length - 1];
  if (latestCpu) liveCpuSince = Math.floor(latestCpu.x / 1000);

  // The axis always spans the full window ending now, even while the
  // data covers less of it (just after load, or after a gap).
  const liveRange = { from: nowMs - LIVE_WINDOW_MS, to: nowMs };
  liveChart = upsertChart(liveChart, 'liveChart',
    powerCpuDatasets(bucketize(livePowerRaw, LIVE_BUCKET_MS), bucketize(liveCpuRaw, LIVE_BUCKET_MS)),
    powerCpuOptions(liveRange, false));
  liveChart.options.scales.x.min = liveRange.from;
  liveChart.options.scales.x.max = liveRange.to;
  liveChart.update('none');
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
  customRange = null;
  rangePicker?.clear();
  loadCharts();
  loadBreakdown();
});

// One calendar for the custom range: first click picks the "from" day,
// second the "to" day. The footer holds the two times and a "Now"
// shortcut (to = this minute). Times use flatpickr's own inline time
// widgets rather than <input type="time">, whose 12/24h display follows
// the browser UI language: these follow use24Hour(), like the charts.
let rangeTimes = { from: '00:00', to: '23:59' };
let rangeTimePickers = {};

function fmtRangeEnd(d) {
  return d.toLocaleString(i18n.intlTag(), { day: 'numeric', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit' });
}

function withTime(day, hhmm) {
  const [h, m] = hhmm.split(':').map(Number);
  return new Date(day.getFullYear(), day.getMonth(), day.getDate(), h || 0, m || 0);
}

function showRangeText() {
  if (customRange) rangePicker.input.value = `${fmtRangeEnd(customRange.from)} → ${fmtRangeEnd(customRange.to)}`;
}

function applyCustomRange() {
  const days = rangePicker.selectedDates;
  if (days.length !== 2) return;
  let from = withTime(days[0], rangeTimes.from), to = withTime(days[1], rangeTimes.to);
  if (to <= from) [from, to] = [to, from];
  customRange = { from, to };
  showRangeText();
  document.querySelectorAll('#rangePicker button[data-range]').forEach((b) => b.classList.remove('active'));
  loadCharts();
  loadBreakdown();
}

function rangeFooter() {
  const footer = document.createElement('div');
  footer.className = 'range-footer';
  footer.innerHTML = `
    <div class="range-time"><span>${i18n.t('range.from_time')}</span><input data-end="from" /></div>
    <div class="range-time"><span>${i18n.t('range.to_time')}</span><input data-end="to" /></div>
    <button type="button" class="range-now">${i18n.t('range.now')}</button>`;
  footer.querySelectorAll('input[data-end]').forEach((el) => {
    const end = el.dataset.end;
    rangeTimePickers[end] = flatpickr(el, {
      enableTime: true,
      noCalendar: true,
      inline: true,
      time_24hr: use24Hour(),
      dateFormat: 'H:i',
      defaultDate: rangeTimes[end],
      onChange: (_, hhmm) => {
        if (!hhmm) return;
        rangeTimes[end] = hhmm;
        applyCustomRange();
      },
    });
  });
  footer.querySelector('.range-now').addEventListener('click', () => {
    const now = new Date();
    rangeTimes.to = `${String(now.getHours()).padStart(2, '0')}:${String(now.getMinutes()).padStart(2, '0')}`;
    rangeTimePickers.to.setDate(rangeTimes.to, false);
    // Day only: flatpickr drops a date past maxDate ('today' = 00:00),
    // the time lives in rangeTimes.
    const today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
    rangePicker.setDate([rangePicker.selectedDates[0] || today, today], false);
    applyCustomRange();
    rangePicker.close();
  });
  return footer;
}

// Week start from the regional format too (Monday in most of Europe),
// where the browser exposes it.
function firstDayOfWeek() {
  try {
    const loc = new Intl.Locale(i18n.intlTag());
    const info = loc.getWeekInfo ? loc.getWeekInfo() : loc.weekInfo;
    return info.firstDay % 7;
  } catch (e) {
    return 1;
  }
}

function initRangePicker() {
  Object.values(rangeTimePickers).forEach((fp) => fp.destroy());
  rangeTimePickers = {};
  rangePicker?.destroy();
  const base = i18n.locale === 'it' ? flatpickr.l10ns.it : flatpickr.l10ns.default;
  rangePicker = flatpickr('#customRange', {
    mode: 'range',
    maxDate: 'today',
    dateFormat: 'j M Y',
    locale: { ...base, firstDayOfWeek: firstDayOfWeek() },
    defaultDate: customRange ? [customRange.from, customRange.to] : undefined,
    onReady: (_, __, fp) => fp.calendarContainer.appendChild(rangeFooter()),
    onChange: (dates) => {
      if (dates.length === 2) applyCustomRange();
    },
    // flatpickr rewrites the field with dates only (e.g. on close): put
    // the times back.
    onClose: () => setTimeout(showRangeText),
  });
  showRangeText();
}

document.getElementById('resetZoomBtn').addEventListener('click', () => {
  if (!historyChart) return;
  historyChart.resetZoom();
  syncCostRange(historyChart);
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
  // Live charts are updated in place, not recreated, so they need an
  // explicit rebuild to pick up the new tick format.
  liveChart?.destroy();
  liveChart = null;
  initRangePicker();
  loadAll();
});

// --- startup -----------------------------------------------------------------

async function main() {
  await i18n.init();
  document.getElementById('localeSwitcher').value = i18n.locale;
  initRangePicker();

  loadAll();
  setInterval(loadKpis, 30000); // cost/price summary
  setInterval(loadLive, 2000); // matches the wattmeter's ~2s publish interval; also drives "Current power" + its pulse
}
main();
