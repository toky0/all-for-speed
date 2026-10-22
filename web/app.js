// SPDX-License-Identifier: GPL-3.0-or-later
'use strict';

// ---------------------------------------------------------------- parameters

const PHASE_MS = 10000;          // duration of the download and of the upload
const GRACE_MS = 1500;           // ignored at the start of each transfer (ramp-up)
const STREAMS = 6;               // parallel requests (6 TCP connections on HTTP/1.1, 6 streams on HTTP/2 and HTTP/3)
const PINGS = 20;
const PING_TIMEOUT = 2000;
const H3_WAIT_MS = 2500;         // how long to wait for the browser to move to HTTP/3
const MAX_UPLOAD_CHUNK = 64 << 20;
const SCALE = [0, 10, 50, 100, 250, 500, 1000, 2500, 5000, 10000]; // Mbps, evenly spaced on the dial

const fams = {
  v4: { key: 'v4', label: 'IPv4', base: '', ok: false },
  v6: { key: 'v6', label: 'IPv6', base: '', ok: false },
};
const PROTOS = {                 // page value -> what the server reports in X-Proto
  h1: { name: 'HTTP/1.1', x: 'HTTP/1.1' },
  h2: { name: 'HTTP/2', x: 'HTTP/2.0' },
  h3: { name: 'HTTP/3', x: 'HTTP/3.0' },
};
const cfg = { endpoints: {}, protos: [], def: 'h1' };
const avail = {};                // avail[proto][fam] = /api/info answer, or null
const info = {};                 // info[fam] = first /api/info answer for the lane
let proto = 'h1';                // selected HTTP version
let runCtl = null;               // AbortController for the running test (Stop)
let series = [];                 // [{fam, dir, pts: [[ms, bps], ...]}] drawn on the trace
let running = false;
const reduceMotion = matchMedia('(prefers-reduced-motion: reduce)').matches;

const $ = (id) => document.getElementById(id);
const lane = (k) => document.querySelector(`.lane[data-fam="${k}"]`);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const rnd = () => Math.random().toString(36).slice(2, 10);
const randHex = (n) => Array.from(crypto.getRandomValues(new Uint8Array(n)), (b) => b.toString(16).padStart(2, '0')).join('');

// ---------------------------------------------------------------- formatting

function fmtRate(bps) {
  const m = bps / 1e6;
  if (m >= 1000) return [(m / 1000).toFixed(m >= 10000 ? 1 : 2), 'Gbps'];
  if (m >= 100) return [m.toFixed(0), 'Mbps'];
  if (m >= 10) return [m.toFixed(1), 'Mbps'];
  return [m.toFixed(2), 'Mbps'];
}
const fmtMs = (ms) => (ms >= 100 ? ms.toFixed(0) : ms.toFixed(1));
const protoName = (p) => ({ 'HTTP/1.1': 'HTTP/1.1', 'HTTP/2.0': 'HTTP/2', 'HTTP/3.0': 'HTTP/3' }[p] || p);

function flagEmoji(cc) {
  if (!cc || cc.length !== 2) return '';
  return String.fromCodePoint(...[...cc.toUpperCase()].map((c) => 0x1f1a5 + c.charCodeAt(0)));
}

function setMetric(k, m, value, unit, note) {
  const dd = lane(k).querySelector(`[data-m="${m}"]`);
  dd.textContent = value;
  if (unit) {
    const u = document.createElement('small');
    u.textContent = unit;
    dd.append(u);
  }
  if (note) {
    const s = document.createElement('span');
    s.className = 'note';
    s.textContent = note;
    dd.append(s);
  }
}
const setPhase = (t) => { $('phase').textContent = t; };

// ---------------------------------------------------------------- the dial

const SVGNS = 'http://www.w3.org/2000/svg';
const DIAL = { cx: 200, cy: 196, r: 150, a0: -125, a1: 125 };
const dial = {
  target: 0, shown: 0,           // fraction of the scale, 0..1
  peak: 0, progress: 0,
  els: {}, raf: 0, last: 0,
};

function polar(r, deg) {
  const a = (deg * Math.PI) / 180;
  return [DIAL.cx + r * Math.sin(a), DIAL.cy - r * Math.cos(a)];
}
function arcPath(r, from, to) {
  const [x0, y0] = polar(r, from);
  const [x1, y1] = polar(r, to);
  const large = to - from > 180 ? 1 : 0;
  return `M${x0.toFixed(2)} ${y0.toFixed(2)} A${r} ${r} 0 ${large} 1 ${x1.toFixed(2)} ${y1.toFixed(2)}`;
}
function svgEl(tag, attrs, parent) {
  const e = document.createElementNS(SVGNS, tag);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
  parent.append(e);
  return e;
}
const fracToDeg = (f) => DIAL.a0 + f * (DIAL.a1 - DIAL.a0);

function rateToFrac(bps) {
  const m = bps / 1e6;
  if (m <= 0) return 0;
  const n = SCALE.length - 1;
  for (let i = 0; i < n; i++) {
    if (m < SCALE[i + 1]) return (i + (m - SCALE[i]) / (SCALE[i + 1] - SCALE[i])) / n;
  }
  return 1;
}

function buildDial() {
  const svg = $('dial');
  const { r, a0, a1 } = DIAL;
  const full = arcPath(r, a0, a1);
  svgEl('path', { d: full, class: 'track', fill: 'none', 'stroke-width': 16, 'stroke-linecap': 'round' }, svg);

  // phase progress: a thin ring just outside the scale
  svgEl('path', { d: arcPath(r + 22, a0, a1), class: 'prog-track idle', fill: 'none', 'stroke-width': 2, 'stroke-linecap': 'round' }, svg);
  dial.els.prog = svgEl('path', { d: arcPath(r + 22, a0, a1), class: 'prog idle', fill: 'none', 'stroke-width': 2, 'stroke-linecap': 'round', pathLength: 1000, 'stroke-dasharray': '0 1000' }, svg);

  // ticks and labels
  const n = SCALE.length - 1;
  dial.els.labels = [];
  for (let i = 0; i <= n; i++) {
    const deg = fracToDeg(i / n);
    const [x0, y0] = polar(r - 15, deg);
    const [x1, y1] = polar(r - 25, deg);
    svgEl('line', { x1: x0, y1: y0, x2: x1, y2: y1, class: 'tick', 'stroke-width': 2, 'stroke-linecap': 'round' }, svg);
    const [lx, ly] = polar(r - 40, deg);
    const v = SCALE[i];
    const t = svgEl('text', { x: lx.toFixed(1), y: ly.toFixed(1), class: 'lbl' }, svg);
    t.textContent = v >= 1000 ? `${v / 1000}G` : String(v);
    dial.els.labels.push(t);
    if (i < n) {
      for (let k = 1; k < 4; k++) {
        const d = fracToDeg((i + k / 4) / n);
        const [mx0, my0] = polar(r - 15, d);
        const [mx1, my1] = polar(r - 20, d);
        svgEl('line', { x1: mx0, y1: my0, x2: mx1, y2: my1, class: 'tick minor', 'stroke-width': 1.5, 'stroke-linecap': 'round' }, svg);
      }
    }
  }

  // value arc: a soft halo under a solid stroke, drawn with dash offsets
  const dash = { fill: 'none', 'stroke-linecap': 'round', pathLength: 1000, 'stroke-dasharray': '0 1000' };
  dial.els.glow = svgEl('path', { d: full, class: 'glow', 'stroke-width': 30, ...dash }, svg);
  dial.els.fill = svgEl('path', { d: full, class: 'fill', 'stroke-width': 16, ...dash }, svg);
  dial.els.head = svgEl('circle', { r: 9, class: 'head', 'stroke-width': 4, cx: polar(r, a0)[0], cy: polar(r, a0)[1], opacity: 0 }, svg);
  dial.els.peak = svgEl('line', { class: 'peakmark', 'stroke-width': 3, 'stroke-linecap': 'round', opacity: 0 }, svg);
}

function renderDial() {
  const f = Math.max(0, Math.min(1, dial.shown));
  // A zero-length dash with round caps still paints a dot, and a gap of exactly
  // the path length repeats the dash at the far end: hide the arc at zero and
  // use a gap longer than the path.
  const len = (f * 1000).toFixed(1);
  const vis = f > 0.002 ? 'visible' : 'hidden';
  for (const el of [dial.els.fill, dial.els.glow]) {
    el.setAttribute('stroke-dasharray', `${len} 2000`);
    el.setAttribute('visibility', vis);
  }
  const [hx, hy] = polar(DIAL.r, fracToDeg(f));
  dial.els.head.setAttribute('cx', hx.toFixed(2));
  dial.els.head.setAttribute('cy', hy.toFixed(2));
  dial.els.head.setAttribute('opacity', f > 0.002 ? 1 : 0);

  if (dial.peak > 0.002) {
    const d = fracToDeg(dial.peak);
    const [x0, y0] = polar(DIAL.r + 13, d);
    const [x1, y1] = polar(DIAL.r + 30, d);
    Object.entries({ x1: x0, y1: y0, x2: x1, y2: y1, opacity: 1 }).forEach(([k, v]) => dial.els.peak.setAttribute(k, v));
  } else {
    dial.els.peak.setAttribute('opacity', 0);
  }
  dial.els.prog.setAttribute('stroke-dasharray', `${(dial.progress * 1000).toFixed(1)} 2000`);
  dial.els.prog.setAttribute('visibility', dial.progress > 0.002 ? 'visible' : 'hidden');

  const n = SCALE.length - 1;
  dial.els.labels.forEach((t, i) => t.classList.toggle('lit', i / n <= f + 1e-6 && f > 0));
}

// Ease the arc toward the latest reading so the 10 Hz samples move smoothly.
function animateDial(now) {
  const dt = dial.last ? Math.min(100, now - dial.last) : 16;
  dial.last = now;
  const k = reduceMotion ? 1 : 1 - Math.exp(-dt / 140);
  dial.shown += (dial.target - dial.shown) * k;
  renderDial();
  if (Math.abs(dial.target - dial.shown) > 0.0005) {
    dial.raf = requestAnimationFrame(animateDial);
  } else {
    dial.shown = dial.target;
    renderDial();
    dial.raf = 0;
    dial.last = 0;
  }
}
function kickDial() { if (!dial.raf) dial.raf = requestAnimationFrame(animateDial); }

function gaugeMode(text, fam) {
  $('mode').textContent = text;
  if (fam) $('gauge').dataset.fam = fam;
}
function gaugeRate(bps) {
  const [v, u] = fmtRate(bps);
  $('value').textContent = v;
  $('unit').textContent = u;
  dial.target = rateToFrac(bps);
  kickDial();
}
function gaugeMs(ms) {
  $('value').textContent = fmtMs(ms);
  $('unit').textContent = 'ms';
  dial.target = 0;
  kickDial();
}
function gaugePeak(bps) {
  const f = rateToFrac(bps);
  if (f > dial.peak) {
    dial.peak = f;
    const [v, u] = fmtRate(bps);
    $('peak').textContent = `Peak ${v} ${u}`;
  }
}
function gaugeReset() {
  dial.peak = 0;
  dial.progress = 0;
  $('peak').textContent = ' ';
  kickDial();
}
function gaugeProgress(p) { dial.progress = p; kickDial(); }
function gaugeLive(on) { $('gauge').classList.toggle('live', on); }

// ---------------------------------------------------------------- protocol lights

const lights = {};               // 'HTTP/3.0' -> {el, at, used}
let phaseProtos = new Set();

function initLights() {
  document.querySelectorAll('#protos li').forEach((el) => { lights[el.dataset.p] = { el, at: -Infinity, used: false }; });
  setInterval(() => {
    const now = performance.now();
    for (const l of Object.values(lights)) l.el.classList.toggle('on', now - l.at < 1200);
  }, 150);
}
function noteProto(p) {
  if (!p) return;
  const l = lights[p];
  phaseProtos.add(p);
  if (!l) return;
  l.at = performance.now();
  if (!l.used) { l.used = true; l.el.classList.add('used'); }
  l.el.classList.add('on');
}
function resetLights() {
  for (const l of Object.values(lights)) { l.used = false; l.at = -Infinity; l.el.classList.remove('used', 'on'); }
}
const protoOf = (r) => r.headers.get('X-Proto');

// ---------------------------------------------------------------- network helpers

async function fetchTimeout(url, ms, opts = {}, quiet = false) {
  const c = new AbortController();
  const t = setTimeout(() => c.abort(), ms);
  runCtl?.signal.addEventListener('abort', () => c.abort(), { once: true });
  try {
    const r = await fetch(url, { ...opts, cache: 'no-store', signal: c.signal });
    if (!quiet) noteProto(protoOf(r)); // background checks don't light the protocol lamps
    return r;
  } finally {
    clearTimeout(t);
  }
}

// ---------------------------------------------------------------- stopping

class Stopped extends Error {}
const stopped = () => !!runCtl?.signal.aborted;
function checkStop() { if (stopped()) throw new Stopped(); }
// sleep that ends early when the visitor presses Stop
function waitOrStop(ms) {
  return new Promise((resolve) => {
    const t = setTimeout(resolve, ms);
    runCtl?.signal.addEventListener('abort', () => { clearTimeout(t); resolve(); }, { once: true });
  });
}

// ---------------------------------------------------------------- engine button

function setEngine(state, fam) {
  const b = $('start');
  b.dataset.state = state;
  if (fam) b.dataset.fam = fam;
  b.disabled = state === 'off';
  b.setAttribute('aria-label', state === 'running' ? 'Stop test' : 'Start test');
}

// ---------------------------------------------------------------- detection

// Probe every protocol's endpoints for both families at once. A protocol whose
// hostnames don't resolve yet (DNS not added) is simply greyed out.
async function probe(p, k) {
  try {
    const r = await fetchTimeout(cfg.endpoints[p][k] + '/api/info', 5000, {}, true);
    if (!r.ok) throw new Error(r.status);
    const j = await r.json();
    if (j.family !== fams[k].label) throw new Error('wrong family');
    avail[p][k] = j;
    info[k] = info[k] || j;
  } catch {
    avail[p][k] = null;
  }
}

function showLane(k) {
  const el = lane(k);
  const j = info[k];
  if (!j) {
    el.classList.add('off');
    el.querySelector('[data-k="ip"]').textContent = `No ${fams[k].label} connectivity from this device`;
    return;
  }
  el.classList.remove('off');
  el.querySelector('[data-k="ip"]').textContent = j.ip;
  el.querySelector('[data-k="net"]').textContent =
    j.asn ? `AS${j.asn}${j.org ? ' ' + j.org : ''}` : 'Network not in database';
  const place = [j.city, j.region !== j.city ? j.region : '', j.country].filter(Boolean).join(', ');
  el.querySelector('[data-k="loc"]').textContent = place ? `${flagEmoji(j.country_code)} ${place}`.trim() : '';
}

// Apply the selected protocol: endpoints, family checkboxes, lane notes.
function selectProto(p, remember) {
  proto = p;
  document.querySelectorAll('input[name="proto"]').forEach((i) => { i.checked = i.value === p; });
  for (const k of ['v4', 'v6']) {
    const f = fams[k];
    const box = $('use-' + k);
    f.base = cfg.endpoints[p]?.[k] || '';
    const was = f.ok;
    f.ok = !!avail[p]?.[k];
    box.disabled = !f.ok || running;
    if (!f.ok) box.checked = false;
    else if (!was) box.checked = true;
    if (info[k]) {
      lane(k).querySelector('[data-k="via"]').textContent =
        f.ok ? '' : `Not reachable over ${PROTOS[p].name} from here`;
    }
  }
  if (remember) { try { localStorage.setItem('proto', p); } catch { /* private mode */ } }
}

function readyText() {
  const ok = ['v4', 'v6'].filter((k) => fams[k].ok);
  if (!ok.length) return `Neither IPv4 nor IPv6 answered over ${PROTOS[proto].name}. Pick another HTTP version.`;
  const fam = ok.length === 2 ? 'IPv4, then IPv6' : fams[ok[0]].label;
  return `Ready. Press Start to test ${fam} over ${PROTOS[proto].name}. It takes about ${ok.length === 2 ? 'a minute' : '30 seconds'}.`;
}

// ---------------------------------------------------------------- HTTP/3 warm-up

// The first contact with a host goes over TCP; its Alt-Svc header tells the browser
// HTTP/3 is available, and the browser moves new requests over once QUIC is up.
// Send a few light requests until that happens, or give up and use what we have.
async function warmUp(f) {
  const url = f.base + '/api/ping';
  const end = performance.now() + (proto === 'h3' ? H3_WAIT_MS : 0);
  let got = '';
  do {
    checkStop();
    try { got = protoOf(await fetchTimeout(`${url}?w=${rnd()}`, PING_TIMEOUT)); } catch { /* retry */ }
    if (got === PROTOS[proto].x) break;
    await waitOrStop(120);
  } while (performance.now() < end);
  return got;
}

// ---------------------------------------------------------------- latency

async function runPing(f) {
  gaugeMode(`${f.label} ping`, f.key);
  performance.clearResourceTimings();
  const url = f.base + '/api/ping';
  const rtts = [];
  let lost = 0;
  for (let i = 0; i < PINGS; i++) {
    checkStop();
    const u = `${url}?n=${i}-${rnd()}`;
    const t0 = performance.now();
    try {
      await fetchTimeout(u, PING_TIMEOUT);
      let rtt = performance.now() - t0;
      const e = performance.getEntriesByName(u).pop();
      if (e && e.requestStart > 0 && e.responseStart > e.requestStart) rtt = e.responseStart - e.requestStart;
      rtts.push(rtt);
      gaugeMs(rtt);
    } catch {
      lost++;
    }
    gaugeProgress((i + 1) / PINGS);
    await waitOrStop(40);
  }
  performance.clearResourceTimings();
  if (!rtts.length) return { ping: NaN, jitter: NaN, lost };
  const sorted = [...rtts].sort((a, b) => a - b);
  const ping = sorted[Math.floor(sorted.length / 2)];
  let jitter = 0;
  for (let i = 1; i < rtts.length; i++) jitter += Math.abs(rtts[i] - rtts[i - 1]);
  jitter = rtts.length > 1 ? jitter / (rtts.length - 1) : 0;
  gaugeMs(ping);
  return { ping, jitter, lost };
}

// ---------------------------------------------------------------- throughput meter

function meter(getBytes, fam, dir) {
  const t0 = performance.now();
  const samples = [[0, 0]];
  const s = { fam, dir, pts: [] };
  series.push(s);
  const timer = setInterval(() => {
    const t = performance.now() - t0;
    const b = getBytes();
    samples.push([t, b]);
    let j = samples.length - 1; // rate over the last second
    while (j > 0 && t - samples[j - 1][0] <= 1000) j--;
    const [tj, bj] = samples[j];
    const bps = t > tj ? ((b - bj) * 8 * 1000) / (t - tj) : 0;
    s.pts.push([t, bps]);
    gaugeRate(bps);
    if (t > GRACE_MS) gaugePeak(bps);
    gaugeProgress(Math.min(1, t / PHASE_MS));
    draw();
  }, 100);
  return {
    stop() {
      clearInterval(timer);
      samples.push([performance.now() - t0, getBytes()]);
      draw();
    },
    result() {
      const end = samples[samples.length - 1];
      const start = samples.find(([t]) => t >= GRACE_MS) || samples[0];
      if (end[0] <= start[0]) return 0;
      return ((end[1] - start[1]) * 8 * 1000) / (end[0] - start[0]);
    },
  };
}

// ---------------------------------------------------------------- download

async function runDownload(f) {
  gaugeMode(`${f.label} download`, f.key);
  gaugeReset();
  const id = randHex(16);
  const ctrl = new AbortController();
  const end = performance.now() + PHASE_MS;
  let total = 0;

  const stream = async (s) => {
    while (performance.now() < end && !ctrl.signal.aborted) {
      try {
        const r = await fetch(`${f.base}/api/download?id=${id}&ms=${PHASE_MS + 3000}&s=${s}&r=${rnd()}`,
          { cache: 'no-store', signal: ctrl.signal });
        noteProto(protoOf(r));
        if (!r.ok) { await sleep(250); continue; }
        const reader = r.body.getReader();
        const p = protoOf(r);
        let n = 0;
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          total += value.byteLength;
          if ((++n & 63) === 0) noteProto(p); // keep the light on while data flows
        }
      } catch {
        if (ctrl.signal.aborted) return;
        await sleep(150);
      }
    }
  };

  const m = meter(() => total, f.key, 'dl');
  const tasks = Array.from({ length: STREAMS }, (_, i) => stream(i));
  await waitOrStop(PHASE_MS);
  ctrl.abort();
  m.stop();
  await Promise.allSettled(tasks);
  checkStop();
  await sleep(300); // let the server record transport stats for the aborted streams

  let extra = {};
  try {
    const r = await fetchTimeout(`${f.base}/api/result?id=${id}`, 3000);
    if (r.ok) extra = await r.json();
  } catch { /* stats are optional */ }
  return { bps: m.result(), ...extra };
}

// ---------------------------------------------------------------- upload

let uploadMaster = null;
function uploadBlob(size) {
  if (!uploadMaster) {
    const mb = new Uint8Array(1 << 20);
    for (let o = 0; o < mb.length; o += 65536) crypto.getRandomValues(mb.subarray(o, o + 65536));
    uploadMaster = new Blob(Array(MAX_UPLOAD_CHUNK >> 20).fill(mb));
  }
  return uploadMaster.slice(0, size);
}

async function runUpload(f) {
  gaugeMode(`${f.label} upload`, f.key);
  gaugeReset();
  const end = performance.now() + PHASE_MS;
  const live = new Set();
  let total = 0;
  let done = false;
  let lastProto = '';

  const stream = (s) => new Promise((resolve) => {
    let size = 1 << 20;
    const next = () => {
      if (done || performance.now() >= end) return resolve();
      const x = new XMLHttpRequest();
      const started = performance.now();
      let sent = 0;
      const count = (n) => { if (n > sent) { total += n - sent; sent = n; } };
      x.upload.onprogress = (e) => { count(e.loaded); noteProto(lastProto); };
      x.onload = () => {
        live.delete(x);
        lastProto = x.getResponseHeader('X-Proto') || lastProto;
        noteProto(lastProto);
        if (x.status === 429) return setTimeout(next, 250);
        count(size);
        // grow the request until each one takes at least half a second
        if (performance.now() - started < 500 && size < MAX_UPLOAD_CHUNK) size *= 2;
        next();
      };
      x.onerror = () => { live.delete(x); setTimeout(next, 150); };
      x.onabort = () => { live.delete(x); resolve(); };
      x.open('POST', `${f.base}/api/upload?s=${s}&r=${rnd()}`);
      live.add(x);
      x.send(uploadBlob(size));
    };
    next();
  });

  const m = meter(() => total, f.key, 'ul');
  const tasks = Array.from({ length: STREAMS }, (_, i) => stream(i));
  await waitOrStop(PHASE_MS);
  done = true;
  m.stop();
  live.forEach((x) => x.abort());
  await Promise.allSettled(tasks);
  checkStop();
  return { bps: m.result() };
}

// ---------------------------------------------------------------- trace chart

function cssVar(n) { return getComputedStyle(document.documentElement).getPropertyValue(n).trim(); }

function draw() {
  const c = $('trace');
  const dpr = window.devicePixelRatio || 1;
  const w = c.clientWidth, h = c.clientHeight;
  if (c.width !== Math.round(w * dpr) || c.height !== Math.round(h * dpr)) {
    c.width = Math.round(w * dpr);
    c.height = Math.round(h * dpr);
  }
  const g = c.getContext('2d');
  g.setTransform(dpr, 0, 0, dpr, 0, 0);
  g.clearRect(0, 0, w, h);

  let max = 0;
  for (const s of series) for (const [, v] of s.pts) max = Math.max(max, v);
  if (!max) return; // nothing measured yet: leave the trace empty
  max = niceMax(max);
  const pad = 6;
  const x = (t) => (t / PHASE_MS) * w;
  const y = (v) => h - pad - (v / max) * (h - 2 * pad);

  g.font = `12px ${cssVar('--sans')}`;
  g.fillStyle = cssVar('--muted');
  g.strokeStyle = cssVar('--line');
  g.lineWidth = 1;
  g.setLineDash([]);
  for (const f of [0.5, 1]) {
    const yy = Math.round(y(max * f)) + 0.5;
    g.beginPath(); g.moveTo(0, yy); g.lineTo(w, yy); g.stroke();
    const [v, u] = fmtRate(max * f);
    g.fillText(`${v} ${u}`, 4, yy + 14);
  }

  for (const s of series) {
    if (s.pts.length < 2) continue;
    g.strokeStyle = cssVar(s.fam === 'v4' ? '--v4' : '--v6');
    g.lineWidth = s.dir === 'dl' ? 2.5 : 2;
    g.setLineDash(s.dir === 'dl' ? [] : [7, 5]);
    g.lineJoin = 'round';
    g.beginPath();
    s.pts.forEach(([t, v], i) => (i ? g.lineTo(x(t), y(v)) : g.moveTo(x(t), y(v))));
    g.stroke();
  }
}

function niceMax(v) {
  const p = 10 ** Math.floor(Math.log10(v));
  for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= v * 1.05) return m * p;
  return 10 * p;
}

// ---------------------------------------------------------------- run

function clearLane(k) {
  for (const m of ['down', 'up', 'ping', 'jitter', 'loaded', 'loss']) setMetric(k, m, '–');
  lane(k).querySelector('[data-k="via"]').textContent = '';
}

function protoSummary(set) {
  const names = [...set].filter((p) => lights[p]).sort().map(protoName);
  return names.length ? `Tested over ${names.join(' and ')}` : '';
}

function onEngine() {
  if (running) { runCtl?.abort(); setPhase('Stopping…'); return; }
  run();
}

async function run() {
  const chosen = ['v4', 'v6'].filter((k) => fams[k].ok && $('use-' + k).checked);
  if (!chosen.length) { setPhase('Select IPv4, IPv6 or both, then press Start.'); return; }
  running = true;
  runCtl = new AbortController();
  setEngine('running', chosen[0]);
  document.querySelectorAll('.options input').forEach((i) => { i.dataset.was = i.disabled; i.disabled = true; });
  series = [];
  const results = {};
  chosen.forEach(clearLane);
  resetLights();
  gaugeLive(true);
  draw();
  const want = PROTOS[proto];

  try {
    for (const k of chosen) {
      const f = fams[k];
      lane(k).classList.add('active');
      setEngine('running', k);

      setPhase(`Connecting to the ${f.label} test server over ${want.name}…`);
      gaugeMode(`${f.label} connecting`, f.key);
      gaugeReset();
      const got = await warmUp(f);
      checkStop();
      let viaNote = '';
      if (got && got !== want.x) {
        viaNote = proto === 'h3'
          ? ` HTTP/3 didn't come up (UDP may be blocked on your network), so this uses ${protoName(got)}.`
          : ` Your browser used ${protoName(got)} instead of ${want.name}.`;
      }

      setPhase(`Measuring ${f.label} latency.${viaNote}`);
      const p = await runPing(f);
      checkStop();
      setMetric(k, 'ping', Number.isFinite(p.ping) ? fmtMs(p.ping) : '–', 'ms');
      setMetric(k, 'jitter', Number.isFinite(p.jitter) ? fmtMs(p.jitter) : '–', 'ms');

      phaseProtos = new Set();
      setPhase(`Measuring ${f.label} download.${viaNote}`);
      const d = await runDownload(f);
      results[k] = Object.assign(fmtRate(d.bps), { bps: d.bps });
      setMetric(k, 'down', ...fmtRate(d.bps));
      if (Number.isFinite(d.loaded_rtt_ms)) setMetric(k, 'loaded', fmtMs(d.loaded_rtt_ms), 'ms');
      const probeNote = p.lost ? `${p.lost} of ${PINGS} pings timed out` : '';
      if (Number.isFinite(d.loss_pct)) {
        const src = d.loss_source === 'quic' ? 'QUIC packets lost' : 'TCP retransmits';
        const v = d.loss_pct > 0 && d.loss_pct < 0.01 ? '<0.01' : d.loss_pct.toFixed(2);
        setMetric(k, 'loss', v, '%', [src, probeNote].filter(Boolean).join('. '));
      } else if (p.lost) {
        setMetric(k, 'loss', ((100 * p.lost) / PINGS).toFixed(0), '%', 'from pings only');
      }

      setPhase(`Measuring ${f.label} upload.${viaNote}`);
      const u = await runUpload(f);
      setMetric(k, 'up', ...fmtRate(u.bps));
      lane(k).querySelector('[data-k="via"]').textContent = protoSummary(phaseProtos);
      lane(k).classList.remove('active');
    }

    const parts = chosen.map((k) => `${fams[k].label} ${results[k].join(' ')}`);
    setPhase(`Done. Download over ${want.name}: ${parts.join(', ')}. Full results for each protocol are below.`);
    const best = chosen.reduce((a, b) => (results[b].bps > results[a].bps ? b : a));
    gaugeMode(`${fams[best].label} download`, best);
    gaugeRate(results[best].bps);
  } catch (e) {
    if (e instanceof Stopped) {
      setPhase('Test stopped. Results measured before you pressed Stop are below.');
      gaugeMode('Stopped');
      gaugeRate(0);
      gaugeProgress(0);
    } else {
      setPhase(`The test stopped: ${e.message || e}. Press Start to try again.`);
    }
  } finally {
    document.querySelectorAll('.lane.active').forEach((l) => l.classList.remove('active'));
    gaugeLive(false);
    running = false;
    runCtl = null;
    document.querySelectorAll('.options input').forEach((i) => { i.disabled = i.dataset.was === 'true'; });
    setEngine('ready', 'v4');
  }
}

// ---------------------------------------------------------------- boot

async function boot() {
  buildDial();
  renderDial();
  initLights();
  $('start').addEventListener('click', onEngine);
  document.querySelectorAll('input[name="proto"]').forEach((i) =>
    i.addEventListener('change', () => { selectProto(i.value, true); setPhase(readyText()); }));
  window.addEventListener('resize', draw);
  matchMedia('(prefers-color-scheme: dark)').addEventListener?.('change', draw);
  draw();
  try {
    const r = await fetchTimeout('/api/config', 5000, {}, true);
    const c = await r.json();
    cfg.endpoints = c.endpoints || {};
    cfg.protos = c.protos || [];
    cfg.def = c.default || cfg.protos[0];
    if (c.location) $('server-location').textContent = c.location;
    else document.querySelector('.server').hidden = true; // no location configured
  } catch {
    setPhase('The test server did not answer. Reload the page to try again.');
    return;
  }

  const jobs = [];
  for (const p of cfg.protos) {
    avail[p] = {};
    for (const k of ['v4', 'v6']) jobs.push(probe(p, k));
  }
  await Promise.all(jobs);
  showLane('v4');
  showLane('v6');

  const usable = cfg.protos.filter((p) => avail[p].v4 || avail[p].v6);
  document.querySelectorAll('input[name="proto"]').forEach((i) => {
    i.disabled = !usable.includes(i.value);
    i.closest('label').title = usable.includes(i.value) ? '' : 'Not available from this device';
  });
  if (!usable.length) {
    setPhase('Neither IPv4 nor IPv6 reached the test server. Check your connection and reload.');
    return;
  }
  // ?proto=h3 in the URL, then the visitor's last choice, then the server default
  let want = new URLSearchParams(location.search).get('proto');
  if (!usable.includes(want)) { try { want = localStorage.getItem('proto'); } catch { want = null; } }
  if (!usable.includes(want)) want = usable.includes(cfg.def) ? cfg.def : usable[0];
  selectProto(want, false);
  setPhase(readyText());
  setEngine('ready', 'v4');
}

boot();
