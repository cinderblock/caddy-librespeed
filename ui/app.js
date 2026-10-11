import * as speedtest from './speedtest.js';

const $ = (id) => document.getElementById(id);
const SVG = 'http://www.w3.org/2000/svg';

const ui = {
  server: $('server'),
  start: $('start'),
  status: $('status'),
  error: $('error'),
  ip: $('ip'),
  chart: $('chart'),
  samples: $('samples').tBodies[0],
  tiles: {
    download: $('tile-download'),
    upload: $('tile-upload'),
    ping: $('tile-ping'),
    jitter: $('tile-jitter'),
  },
};

const series = [
  { key: 'download', name: 'Download', cls: 's1' },
  { key: 'upload', name: 'Upload', cls: 's2' },
];

// Both tests share one time axis and one warm-up, so their curves compare.
const { duration, grace } = speedtest.defaults.download;

let servers = [];
let running = null; // AbortController of the test in progress
const data = { download: [], upload: [] };

// --- formatting ---------------------------------------------------------

/** Splits a rate into a 3-significant-figure number and its unit. */
function rate(bps) {
  if (bps >= 1e9) return [(bps / 1e9).toPrecision(3), 'Gbps'];
  if (bps >= 1e6 || bps === 0) return [(bps / 1e6).toPrecision(3), 'Mbps'];
  return [(bps / 1e3).toPrecision(3), 'kbps'];
}

const ms = (v) => (v < 10 ? v.toFixed(1) : Math.round(v).toString());
const mbps = (bps) => (bps / 1e6).toLocaleString(undefined, { maximumFractionDigits: 1 });

function setTile(name, num, unit, active = false) {
  const tile = ui.tiles[name];
  tile.querySelector('.num').textContent = num;
  if (unit) tile.querySelector('.unit').textContent = unit;
  tile.classList.toggle('active', active);
}

function resetTiles() {
  setTile('download', '–', 'Mbps');
  setTile('upload', '–', 'Mbps');
  setTile('ping', '–', 'ms');
  setTile('jitter', '–', 'ms');
}

// --- servers ------------------------------------------------------------

function showError(err) {
  ui.error.textContent = err ? String(err.message ?? err) : '';
  ui.error.hidden = !err;
}

function selected() {
  return servers[ui.server.selectedIndex];
}

async function showIP() {
  const s = selected();
  ui.ip.textContent = ' ';
  try {
    const ip = await speedtest.getIP(s.url);
    if (selected() === s) ui.ip.textContent = `Your IP, as ${s.name} sees it: ${ip}`;
  } catch (err) {
    if (selected() === s) ui.ip.textContent = `Couldn't get your IP from ${s.name}: ${err.message}`;
  }
}

async function loadServers() {
  const res = await fetch('config.json', { cache: 'no-store' });
  if (!res.ok) throw new Error(`config.json: HTTP ${res.status}`);
  const config = await res.json();
  const here = new URL('./', location.href).href;
  servers = config.servers.length ? config.servers : [{ name: location.host, url: here }];

  ui.server.replaceChildren(
    ...servers.map((s) => {
      const o = document.createElement('option');
      o.textContent = s.name;
      return o;
    }),
  );
  const local = servers.findIndex((s) => new URL(s.url, location.href).href === here);
  ui.server.selectedIndex = Math.max(0, local);
  ui.server.disabled = servers.length < 2;
  ui.start.disabled = false;
  showIP();

  // Ping every server so the menu shows which is closest.
  if (servers.length > 1) {
    servers.forEach(async (s, i) => {
      const o = ui.server.options[i];
      try {
        const { ping } = await speedtest.ping(s.url, { count: 3 });
        o.textContent = `${s.name} · ${ms(ping)} ms`;
      } catch {
        o.textContent = `${s.name} · unreachable`;
      }
    });
  }
}

// --- chart --------------------------------------------------------------

/**
 * Chart geometry for the container's current width. The SVG is drawn at its
 * real size in CSS pixels (not scaled from a fixed viewBox), so its text stays
 * readable on a phone.
 */
function layout() {
  const W = Math.max(240, Math.round(ui.chart.clientWidth || 720));
  const narrow = W < 480;
  const H = Math.round(Math.min(280, Math.max(200, W * 0.42)));
  // Narrow charts drop the direct end labels (the legend, tiles and readout
  // carry the values), which frees the right margin for the plot.
  const M = { top: 12, right: narrow ? 12 : 84, bottom: 26, left: narrow ? 44 : 52 };
  return { W, H, M, narrow, plotW: W - M.left - M.right, plotH: H - M.top - M.bottom };
}

function el(name, attrs = {}, parent) {
  const e = document.createElementNS(SVG, name);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
  parent?.appendChild(e);
  return e;
}

/** A round axis maximum, in Mbps, with a tick step that divides it into 4. */
function niceScale(maxMbps) {
  if (!(maxMbps > 0)) return { max: 100, step: 25 };
  const raw = maxMbps / 4;
  const mag = 10 ** Math.floor(Math.log10(raw));
  const step = [1, 2, 2.5, 5, 10].map((m) => m * mag).find((s) => s * 4 >= maxMbps);
  return { max: step * 4, step };
}

let chart = null;

function drawChart() {
  const L = layout();
  const { W, H, M, plotW, plotH, narrow } = L;
  const maxMbps = Math.max(0, ...series.flatMap((s) => data[s.key].map((d) => d.bps / 1e6)));
  const { max, step } = niceScale(maxMbps);
  const x = (t) => M.left + (Math.min(t, duration) / duration) * plotW;
  const y = (bps) => M.top + plotH - (bps / 1e6 / max) * plotH;

  const svg = el('svg', {
    width: W,
    height: H,
    viewBox: `0 0 ${W} ${H}`,
    role: 'img',
    tabindex: '0',
    'aria-label': 'Download and upload throughput over time. Use the arrow keys, or drag along the chart, to read values.',
  });

  el('rect', { class: 'warmup', x: M.left, y: M.top, width: x(grace) - M.left, height: plotH }, svg);
  el('text', { class: 'warmup-label', x: M.left + 6, y: M.top + 14 }, svg).textContent = 'warm-up';

  for (let v = step; v <= max + 1e-9; v += step) {
    el('line', { class: 'grid', x1: M.left, x2: M.left + plotW, y1: y(v * 1e6), y2: y(v * 1e6) }, svg);
  }
  for (let v = 0; v <= max + 1e-9; v += step) {
    const t = el('text', { class: 'tick', x: M.left - 8, y: y(v * 1e6) + 4, 'text-anchor': 'end' }, svg);
    t.textContent = v.toLocaleString();
  }
  el('line', { class: 'baseline', x1: M.left, x2: M.left + plotW, y1: M.top + plotH, y2: M.top + plotH }, svg);
  const every = narrow ? 4 : 2;
  for (let s = 0; s <= duration / 1000; s += every) {
    const t = el('text', { class: 'tick', x: x(s * 1000), y: H - 6, 'text-anchor': 'middle' }, svg);
    t.textContent = s + every > duration / 1000 ? `${s} s` : `${s}`;
  }

  const ends = [];
  for (const s of series) {
    const pts = data[s.key];
    if (!pts.length) continue;
    const d = pts.map((p, i) => `${i ? 'L' : 'M'}${x(p.t).toFixed(1)},${y(p.bps).toFixed(1)}`).join('');
    el('path', { class: `series ${s.cls}`, d }, svg);
    const last = pts[pts.length - 1];
    ends.push({ s, x: x(last.t), y: y(last.bps), bps: last.bps });
  }
  // Direct end labels, unless two would collide; the legend and tooltip cover that case.
  const collide = ends.length === 2 && Math.abs(ends[0].y - ends[1].y) < 16 && Math.abs(ends[0].x - ends[1].x) < 90;
  for (const e of ends) {
    el('circle', { class: `dot ${e.s.cls}`, cx: e.x, cy: e.y, r: 4 }, svg);
    if (!collide && !narrow) {
      const [n, u] = rate(e.bps);
      el('text', { class: 'end-label', x: e.x + 8, y: e.y + 4 }, svg).textContent = `${n} ${u}`;
    }
  }

  const hover = el('g', { visibility: 'hidden' }, svg);
  const cross = el('line', { class: 'crosshair', y1: M.top, y2: M.top + plotH }, hover);
  const hoverDots = series.map((s) => el('circle', { class: `dot ${s.cls}`, r: 4 }, hover));

  const tip = document.createElement('div');
  tip.className = 'tooltip';
  tip.hidden = true;

  ui.chart.replaceChildren(svg, tip);
  chart = { svg, x, y, L, hover, cross, hoverDots, tip, index: null };
  bindHover();
}

/** Every distinct sample time, so the crosshair can snap to real data. */
function sampleTimes() {
  const longest = series.map((s) => data[s.key]).reduce((a, b) => (b.length > a.length ? b : a));
  return longest.map((p) => p.t);
}

function nearest(pts, t) {
  let best = null;
  for (const p of pts) if (!best || Math.abs(p.t - t) < Math.abs(best.t - t)) best = p;
  return best && Math.abs(best.t - t) <= speedtest.defaults.sampleInterval ? best : null;
}

function showHover(index) {
  const times = sampleTimes();
  if (!times.length) return;
  index = Math.max(0, Math.min(times.length - 1, index));
  chart.index = index;
  const t = times[index];
  const cx = chart.x(t);
  chart.cross.setAttribute('x1', cx);
  chart.cross.setAttribute('x2', cx);
  chart.hover.setAttribute('visibility', 'visible');

  const head = document.createElement('div');
  head.className = 't';
  head.textContent = `${(t / 1000).toFixed(2)} s${t < grace ? ' (warm-up)' : ''}`;
  const rows = [head];
  series.forEach((s, i) => {
    const p = nearest(data[s.key], t);
    const dot = chart.hoverDots[i];
    dot.setAttribute('visibility', p ? 'visible' : 'hidden');
    if (!p) return;
    dot.setAttribute('cx', chart.x(p.t));
    dot.setAttribute('cy', chart.y(p.bps));
    const row = document.createElement('div');
    row.className = 'row';
    const key = document.createElement('span');
    key.className = `line-key key-${i + 1}`;
    const value = document.createElement('strong');
    value.textContent = rate(p.bps).join(' ');
    const name = document.createElement('span');
    name.textContent = s.name;
    row.append(key, value, name);
    rows.push(row);
  });
  chart.tip.replaceChildren(...rows);
  chart.tip.hidden = false;

  // In CSS pixels. The SVG is drawn at its real size, but it can still be
  // squeezed between a resize and the redraw that follows it.
  const scale = chart.svg.getBoundingClientRect().width / chart.L.W;
  const tipW = chart.tip.offsetWidth;
  let left = cx * scale + 12;
  if (left + tipW > ui.chart.clientWidth) left = cx * scale - 12 - tipW;
  chart.tip.style.left = `${Math.max(0, left)}px`;
  chart.tip.style.top = `${chart.L.M.top * scale}px`;
}

function hideHover() {
  chart.hover.setAttribute('visibility', 'hidden');
  chart.tip.hidden = true;
  chart.index = null;
}

function bindHover() {
  const { svg, L } = chart;
  const track = (e) => {
    const r = svg.getBoundingClientRect();
    const t = (((e.clientX - r.left) * (L.W / r.width) - L.M.left) / L.plotW) * duration;
    const times = sampleTimes();
    if (!times.length) return;
    let best = 0;
    times.forEach((tt, i) => {
      if (Math.abs(tt - t) < Math.abs(times[best] - t)) best = i;
    });
    showHover(best);
  };
  // A tap shows the readout and a horizontal drag scrubs it (CSS touch-action
  // leaves vertical drags to page scrolling).
  svg.addEventListener('pointerdown', track);
  svg.addEventListener('pointermove', track);
  // A lifted finger leaves the readout up until the next tap elsewhere.
  svg.addEventListener('pointerleave', (e) => e.pointerType === 'mouse' && hideHover());
  svg.addEventListener('blur', hideHover);
  svg.addEventListener('keydown', (e) => {
    const n = sampleTimes().length;
    if (!n) return;
    const moves = { ArrowLeft: -1, ArrowRight: 1, Home: -Infinity, End: Infinity };
    if (!(e.key in moves)) return;
    e.preventDefault();
    const i = chart.index ?? (moves[e.key] > 0 ? -1 : n);
    showHover(Number.isFinite(moves[e.key]) ? i + moves[e.key] : moves[e.key] < 0 ? 0 : n - 1);
  });
}

function drawTable() {
  const n = Math.max(data.download.length, data.upload.length);
  const rows = [];
  for (let i = 0; i < n; i++) {
    const d = data.download[i];
    const u = data.upload[i];
    const tr = document.createElement('tr');
    for (const text of [((d ?? u).t / 1000).toFixed(2), d ? mbps(d.bps) : '', u ? mbps(u.bps) : '']) {
      const td = document.createElement('td');
      td.textContent = text;
      tr.append(td);
    }
    rows.push(tr);
  }
  ui.samples.replaceChildren(...rows);
}

// --- running a test -----------------------------------------------------

function onProgress(key) {
  return ({ t, bps, average, counting }) => {
    data[key].push({ t, bps });
    const [n, u] = rate(counting && average !== null ? average : bps);
    setTile(key, n, u, !counting);
    drawChart();
  };
}

async function run() {
  const s = selected();
  running = new AbortController();
  const { signal } = running;
  ui.start.textContent = 'Stop';
  ui.start.classList.add('stop');
  ui.server.disabled = true;
  showError(null);
  resetTiles();
  data.download = [];
  data.upload = [];
  drawChart();
  drawTable();

  try {
    ui.status.textContent = `Measuring ping to ${s.name}…`;
    const p = await speedtest.ping(s.url, { signal, onSample: (v) => setTile('ping', ms(v), 'ms', true) });
    setTile('ping', ms(p.ping), 'ms');
    setTile('jitter', ms(p.jitter), 'ms');

    ui.status.textContent = `Measuring download from ${s.name}…`;
    const down = await speedtest.download(s.url, { signal, onProgress: onProgress('download') });
    setTile('download', ...rate(down.bps));

    ui.status.textContent = `Measuring upload to ${s.name}…`;
    const up = await speedtest.upload(s.url, { signal, duration, grace, onProgress: onProgress('upload') });
    setTile('upload', ...rate(up.bps));

    ui.status.textContent = `Done: tested against ${s.name} at ${new Date().toLocaleTimeString()}.`;
  } catch (err) {
    if (signal.aborted) {
      ui.status.textContent = 'Stopped.';
    } else {
      ui.status.textContent = 'The test failed.';
      showError(err);
    }
    for (const name of Object.keys(ui.tiles)) ui.tiles[name].classList.remove('active');
  } finally {
    running = null;
    ui.start.textContent = 'Start';
    ui.start.classList.remove('stop');
    ui.server.disabled = servers.length < 2;
    drawChart();
    drawTable();
  }
}

ui.start.addEventListener('click', () => (running ? running.abort() : run()));
document.addEventListener('pointerdown', (e) => chart && !chart.svg.contains(e.target) && hideHover());

// Redraw at the new size when the chart's width changes (rotation, resizing).
let drawnWidth = 0;
new ResizeObserver(() => {
  const w = Math.round(ui.chart.clientWidth);
  if (w !== drawnWidth) {
    drawnWidth = w;
    drawChart();
  }
}).observe(ui.chart);
ui.server.addEventListener('change', showIP);

drawChart();
loadServers().catch((err) => {
  ui.status.textContent = 'Could not load the server list.';
  showError(err);
});
