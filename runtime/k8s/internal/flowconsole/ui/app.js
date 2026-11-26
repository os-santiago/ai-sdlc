// AI-SDLC flow console: vanilla JS, no dependencies. It consumes
// ai-sdlc.flow-event/v1 events from /events (Server-Sent Events) and draws one
// swimlane per issue with ordered stage bars. All event text is inserted with
// textContent or SVG text nodes, never as HTML.
(function () {
  'use strict';

  const STAGES = ['intake', 'admission', 'planning', 'execution', 'verification', 'review', 'publication', 'merge'];
  const STAGE_LABEL = { intake: 'intake', admission: 'admit', planning: 'plan', execution: 'generate', verification: 'verify', review: 'review', publication: 'publish', merge: 'merge' };
  const INSTANT = { intake: true, planning: true };
  const TERMINAL = { completed: true, failed: true, 'stopped-safely': true };
  const OUTCOME_TEXT = {
    completed: 'completed', failed: 'failed (verdict)', 'stopped-safely': 'stopped safely', running: 'running',
    waiting: 'waiting', blocked: 'blocked', quarantined: 'quarantined',
  };
  const LANE_H = 40, BAR_H = 16, WAIT_H = 4;
  const NS = 'http://www.w3.org/2000/svg';

  const state = {
    events: new Map(),
    lanes: new Map(),
    dirty: true,
    status: null,
    connected: false,
    view: null,            // [t0, t1] in ms
    follow: true,
    filter: { status: 'all', component: '', issue: '' },
    selected: null,
  };

  const $ = (id) => document.getElementById(id);
  // Shareable view: ?range=<hours>&status=<status>&issue=<text>&open=<lane key>
  const params = new URLSearchParams(location.search);
  const chart = $('chart'), axis = $('axis'), labels = $('labels'), lanesBox = $('lanes'), tooltip = $('tooltip');

  function svg(tag, attrs, parent) {
    const node = document.createElementNS(NS, tag);
    for (const k in attrs) node.setAttribute(k, attrs[k]);
    if (parent) parent.appendChild(node);
    return node;
  }
  function html(tag, cls, text, parent) {
    const node = document.createElement(tag);
    if (cls) node.className = cls;
    if (text !== undefined && text !== null) node.textContent = text;
    if (parent) parent.appendChild(node);
    return node;
  }

  // ---------- formatting ----------
  function fmtDuration(ms) {
    if (ms === undefined || ms === null || !isFinite(ms)) return '–';
    if (ms < 1000) return Math.round(ms) + ' ms';
    const s = ms / 1000;
    if (s < 60) return (s < 10 ? s.toFixed(1) : Math.round(s)) + ' s';
    const m = Math.floor(s / 60), rs = Math.round(s % 60);
    if (m < 60) return m + ' min' + (rs ? ' ' + rs + ' s' : '');
    const h = Math.floor(m / 60);
    return h + ' h ' + (m % 60) + ' min';
  }
  const pad = (n) => String(n).padStart(2, '0');
  function fmtClock(t, seconds) {
    const d = new Date(t);
    return pad(d.getHours()) + ':' + pad(d.getMinutes()) + (seconds ? ':' + pad(d.getSeconds()) : '');
  }
  function fmtDate(t) {
    const d = new Date(t);
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate());
  }
  function median(values) {
    if (!values.length) return null;
    const v = values.slice().sort((a, b) => a - b), m = v.length >> 1;
    return v.length % 2 ? v[m] : (v[m - 1] + v[m]) / 2;
  }
  function quantile(values, q) {
    if (!values.length) return null;
    const v = values.slice().sort((a, b) => a - b);
    return v[Math.min(v.length - 1, Math.floor(q * v.length))];
  }

  // ---------- model ----------
  function laneKey(e) {
    if (e.issue) return (e.repository || '') + '#' + e.issue;
    return 'work-item/' + e.workItem;
  }

  function addEvent(e) {
    if (state.events.has(e.id)) return;
    e.t = Date.parse(e.time);
    state.events.set(e.id, e);
    const key = laneKey(e);
    let lane = state.lanes.get(key);
    if (!lane) {
      lane = { key, repository: e.repository || '', issue: e.issue || 0, workItem: e.workItem || '', events: [] };
      state.lanes.set(key, lane);
    }
    if (!lane.repository && e.repository) lane.repository = e.repository;
    lane.events.push(e);
    lane.dirty = true;
    state.dirty = true;
  }

  // Build stage segments of one lane: one per (attempt, stage), in order.
  function buildLane(lane, now) {
    lane.events.sort((a, b) => a.t - b.t || STAGES.indexOf(a.stage) - STAGES.indexOf(b.stage));
    const segments = new Map(), markers = [];
    let attempts = 0;
    for (const e of lane.events) {
      const attempt = e.attempt || 0;
      attempts = Math.max(attempts, attempt);
      const key = (STAGES.indexOf(e.stage) < 3 ? 'pre' : (e.workItem + '/' + attempt)) + '/' + e.stage + (e.stage === 'admission' || e.stage === 'intake' ? '/' + e.id : '');
      let s = segments.get(key);
      if (!s) {
        s = { stage: e.stage, attempt, workItem: e.workItem, first: e.t, queued: null, start: null, end: null, outcome: null, diagnostic: '',
          events: [], calls: [], effects: 0, stale: null, blocked: null, quarantined: null, degraded: [], lastAt: e.t };
        segments.set(key, s);
      }
      s.events.push(e);
      s.lastAt = Math.max(s.lastAt, e.t);
      switch (e.kind) {
        case 'waiting':
          if (e.diagnostic === 'stale-wait') { s.stale = e; markers.push({ type: 'stale', e }); } else if (s.queued === null) s.queued = e.t;
          break;
        case 'started': if (s.start === null) s.start = e.t; break;
        case 'progressed':
          if (e.step === 'model-call') s.calls.push(e); else s.effects++;
          if (s.start === null) s.start = e.t - (e.durationMs || e.latencyMs || 0);
          break;
        case 'completed': case 'failed': case 'stopped-safely':
          s.end = e.t; s.outcome = e.kind; s.diagnostic = e.diagnostic || ''; s.terminal = e;
          if (s.start === null) s.start = e.t - (e.durationMs || 0);
          if (e.kind !== 'completed') markers.push({ type: e.kind, e });
          break;
        case 'blocked': s.blocked = e; markers.push({ type: 'blocked', e }); break;
        case 'quarantined': s.quarantined = e; markers.push({ type: 'quarantined', e }); break;
        case 'degraded': s.degraded.push(e); markers.push({ type: 'degraded', e }); break;
        case 'retried': markers.push({ type: 'retried', e }); break;
      }
    }
    const list = [];
    for (const s of segments.values()) {
      if (s.queued === null) s.queued = s.start !== null ? Math.min(s.start, s.first) : s.first;
      if (s.start === null) s.start = s.end !== null ? s.end : (s.blocked ? s.queued : null);
      if (s.end !== null && s.start > s.end) s.start = s.end;
      s.open = s.end === null && !INSTANT[s.stage];
      if (s.quarantined) s.outcome = 'quarantined';
      else if (s.end === null) {
        if (s.blocked) s.outcome = 'blocked';
        else if (s.start !== null) s.outcome = 'running';
        else s.outcome = 'waiting';
        if (INSTANT[s.stage]) { s.outcome = 'completed'; s.end = s.first; s.start = s.first; s.open = false; }
      }
      if (s.stage === 'admission' && s.blocked) { s.end = s.blocked.t; s.open = false; s.outcome = 'blocked'; }
      s.drawEnd = s.end !== null ? s.end : now;
      list.push(s);
    }
    list.sort((a, b) => a.queued - b.queued || STAGES.indexOf(a.stage) - STAGES.indexOf(b.stage));
    lane.segments = list;
    lane.markers = markers;
    lane.attempts = attempts;
    lane.components = new Set(lane.events.map((e) => e.component));
    // Lane status, from the newest work.
    const merged = list.find((s) => s.stage === 'merge' && s.outcome === 'completed');
    const open = list.filter((s) => s.open && s.outcome !== 'quarantined');
    const lastTerminal = list.filter((s) => s.end !== null && !INSTANT[s.stage]).sort((a, b) => a.end - b.end).pop();
    if (merged) lane.status = 'delivered';
    else if (list.some((s) => s.outcome === 'quarantined' || s.outcome === 'blocked' && (s.open || s.stage === 'admission'))) lane.status = 'blocked';
    else if (open.length) lane.status = 'inflight';
    else if (lastTerminal && lastTerminal.outcome !== 'completed') lane.status = 'failed';
    else lane.status = 'inflight';
    lane.first = list.length ? Math.min.apply(null, list.map((s) => s.queued)) : now;
    lane.last = list.length ? Math.max.apply(null, list.map((s) => s.drawEnd)) : now;
    lane.delivery = merged ? merged.end - lane.first : null;
    lane.stopped = list.filter((s) => s.outcome === 'failed' || s.outcome === 'stopped-safely').length;
    lane.dirty = false;
  }

  function visibleLanes() {
    const f = state.filter, q = f.issue.trim().toLowerCase().replace(/^#/, '');
    return Array.from(state.lanes.values()).filter((l) => {
      if (f.status !== 'all' && l.status !== f.status) return false;
      if (f.component && !l.components.has(f.component)) return false;
      if (q) {
        const text = (l.repository + '#' + l.issue + ' ' + l.workItem).toLowerCase();
        if (String(l.issue) !== q && text.indexOf(q) < 0) return false;
      }
      return true;
    }).sort((a, b) => b.first - a.first);
  }

  // ---------- summary ----------
  function renderTiles(all) {
    const count = (s) => all.filter((l) => l.status === s).length;
    $('t-inflight').textContent = count('inflight');
    const running = all.reduce((n, l) => n + l.segments.filter((s) => s.outcome === 'running').length, 0);
    const waiting = all.reduce((n, l) => n + l.segments.filter((s) => s.outcome === 'waiting').length, 0);
    $('t-inflight-note').textContent = running + ' running · ' + waiting + ' waiting';
    const delivered = all.filter((l) => l.status === 'delivered');
    $('t-delivered').textContent = delivered.length;
    const retriedDelivered = delivered.filter((l) => l.attempts > 1).length;
    $('t-delivered-note').textContent = all.length ? (delivered.length - retriedDelivered) + ' first attempt · ' + retriedDelivered + ' after retry' : '';
    $('t-failed').textContent = count('failed');
    const stops = all.reduce((n, l) => n + l.stopped, 0);
    $('t-failed-note').textContent = stops + ' stopped or refused stages';
    $('t-blocked').textContent = count('blocked');
    const stale = all.reduce((n, l) => n + l.markers.filter((m) => m.type === 'stale').length, 0);
    const quarantined = all.reduce((n, l) => n + l.markers.filter((m) => m.type === 'quarantined').length, 0);
    $('t-blocked-note').textContent = quarantined + ' quarantined · ' + stale + ' stale waits';
    const ttd = delivered.map((l) => l.delivery).filter((v) => v !== null);
    $('t-ttd').textContent = ttd.length ? fmtDuration(median(ttd)) : '–';
    $('t-ttd-note').textContent = ttd.length ? 'from intake to merge · ' + ttd.length + ' delivered' : 'no delivery yet';
    const lat = [];
    let degraded = 0;
    for (const l of all) for (const s of l.segments) { for (const c of s.calls) if (c.latencyMs) lat.push(c.latencyMs); degraded += s.degraded.length; }
    $('t-latency').textContent = lat.length ? fmtDuration(median(lat)) : '–';
    $('t-latency-note').textContent = lat.length ? 'p90 ' + fmtDuration(quantile(lat, 0.9)) + ' · ' + lat.length + ' calls · ' + degraded + ' degraded' : 'no model calls';
  }

  // ---------- scales ----------
  function chartWidth() { return Math.max(200, chart.clientWidth || (lanesBox.clientWidth - 232)); }
  function x(t) { const [t0, t1] = state.view, w = chartWidth(); return (t - t0) / (t1 - t0) * w; }
  function tAt(px) { const [t0, t1] = state.view; return t0 + px / chartWidth() * (t1 - t0); }

  function dataExtent(lanes, now) {
    let lo = Infinity, hi = -Infinity;
    for (const l of lanes) { lo = Math.min(lo, l.first); hi = Math.max(hi, l.last); }
    if (!isFinite(lo)) { lo = now - 3600e3; hi = now; }
    hi = Math.max(hi, now);
    const padding = Math.max(60e3, (hi - lo) * 0.03);
    return [lo - padding, hi + padding];
  }

  const STEPS = [60e3, 120e3, 300e3, 600e3, 900e3, 1800e3, 3600e3, 7200e3, 10800e3, 21600e3, 43200e3, 86400e3];
  function renderAxis(height) {
    axis.textContent = '';
    const w = chartWidth();
    axis.setAttribute('viewBox', '0 0 ' + w + ' 34');
    axis.setAttribute('width', w);
    const [t0, t1] = state.view;
    const target = (t1 - t0) / Math.max(2, w / 110);
    const step = STEPS.find((s) => s >= target) || 86400e3;
    const offset = new Date(t0).getTimezoneOffset() * 60e3;
    let t = Math.ceil((t0 - offset) / step) * step + offset;
    const ticks = [];
    let lastDay = '';
    for (; t <= t1; t += step) {
      const day = fmtDate(t), px = x(t);
      const major = day !== lastDay && lastDay !== '';
      lastDay = day;
      ticks.push({ t, px, major });
      const g = svg('g', { class: 'tick' + (major ? ' major' : '') }, axis);
      svg('line', { x1: px, x2: px, y1: 24, y2: 34 }, g);
      const label = svg('text', { x: px + 4, y: 20 }, g);
      label.textContent = major || ticks.length === 1 ? day.slice(5) + ' ' + fmtClock(t) : fmtClock(t);
    }
    return ticks;
  }

  // ---------- chart ----------
  function defs(root) {
    const d = svg('defs', {}, root);
    const hatch = (id, color) => {
      const p = svg('pattern', { id, width: 6, height: 6, patternUnits: 'userSpaceOnUse', patternTransform: 'rotate(45)' }, d);
      svg('rect', { width: 6, height: 6, fill: color, opacity: 0.35 }, p);
      svg('line', { x1: 0, y1: 0, x2: 0, y2: 6, stroke: color, 'stroke-width': 3 }, p);
    };
    hatch('hatch-blocked', '#c98500');
    hatch('hatch-quarantined', '#9085e9');
    const run = svg('pattern', { id: 'stripes-running', width: 10, height: 10, patternUnits: 'userSpaceOnUse', patternTransform: 'rotate(45)' }, d);
    svg('rect', { width: 10, height: 10, fill: '#3987e5' }, run);
    svg('rect', { width: 4, height: 10, fill: '#5598e7', class: 'running-stripes' }, run);
  }

  function fillFor(outcome) {
    switch (outcome) {
      case 'completed': return 'var(--completed)';
      case 'failed': return 'var(--failed)';
      case 'stopped-safely': return 'var(--stopped)';
      case 'running': return 'url(#stripes-running)';
      case 'blocked': return 'url(#hatch-blocked)';
      case 'quarantined': return 'url(#hatch-quarantined)';
      default: return 'var(--waiting)';
    }
  }

  // End glyph: secondary encoding so outcome is never colour alone.
  function endGlyph(g, outcome, cx, cy) {
    const stroke = { stroke: '#0e0e0d', 'stroke-width': 2, 'stroke-linecap': 'round', fill: 'none' };
    if (outcome === 'completed') svg('path', Object.assign({ d: `M${cx - 4},${cy} l3,3 l5,-6` }, stroke), g);
    else if (outcome === 'failed') svg('path', Object.assign({ d: `M${cx - 3.5},${cy - 3.5} l7,7 M${cx + 3.5},${cy - 3.5} l-7,7` }, stroke), g);
    else if (outcome === 'stopped-safely') svg('rect', { x: cx - 3.5, y: cy - 3.5, width: 7, height: 7, fill: '#0e0e0d', rx: 1 }, g);
  }

  function marker(g, type, px, cy) {
    switch (type) {
      case 'degraded':
        svg('path', { d: `M${px},${cy - 6} l6,10 h-12 z`, fill: 'var(--degraded)', stroke: 'var(--surface-1)', 'stroke-width': 1.5 }, g); break;
      case 'retried': {
        svg('circle', { cx: px, cy, r: 7, fill: 'var(--text-primary)', stroke: 'var(--surface-1)', 'stroke-width': 1.5 }, g);
        // Circular arrow: three quarters of a circle with a head at its end.
        svg('path', { d: `M${px + 3.6},${cy + 1.2} A3.8,3.8 0 1 1 ${px + 2.6},${cy - 2.8}`, stroke: '#0e0e0d', 'stroke-width': 1.6, fill: 'none', 'stroke-linecap': 'round' }, g);
        svg('path', { d: `M${px + 3.9},${cy - 5} L${px + 3.4},${cy - 2.1} L${px + 0.6},${cy - 2.7}`, stroke: '#0e0e0d', 'stroke-width': 1.4, fill: 'none', 'stroke-linejoin': 'round', 'stroke-linecap': 'round' }, g);
        break;
      }
      case 'stale':
        svg('circle', { cx: px, cy, r: 6, fill: 'var(--surface-1)', stroke: 'var(--blocked)', 'stroke-width': 1.6 }, g);
        svg('path', { d: `M${px},${cy - 3.5} v3.5 l2.5,1.5`, stroke: 'var(--blocked)', 'stroke-width': 1.4, fill: 'none', 'stroke-linecap': 'round' }, g);
        break;
      case 'quarantined':
        svg('path', { d: `M${px - 2.9},${cy - 7} h5.8 l4.1,4.1 v5.8 l-4.1,4.1 h-5.8 l-4.1,-4.1 v-5.8 z`, fill: 'var(--quarantined)', stroke: 'var(--surface-1)', 'stroke-width': 1.2 }, g);
        svg('path', { d: `M${px - 3.5},${cy} h7`, stroke: '#0e0e0d', 'stroke-width': 2 }, g);
        break;
      case 'blocked':
        svg('rect', { x: px - 6, y: cy - 6, width: 12, height: 12, rx: 2, fill: 'var(--blocked)', stroke: 'var(--surface-1)', 'stroke-width': 1.2 }, g);
        svg('path', { d: `M${px - 2},${cy - 3} v6 M${px + 2},${cy - 3} v6`, stroke: '#0e0e0d', 'stroke-width': 1.8 }, g);
        break;
    }
  }

  let ticksCache = [];
  function render() {
    const now = Date.now();
    for (const lane of state.lanes.values()) if (lane.dirty || lane.segments === undefined || lane.hasOpen) { buildLane(lane, now); lane.hasOpen = lane.segments.some((s) => s.open); }
    const all = Array.from(state.lanes.values());
    renderTiles(all);
    const matching = visibleLanes();
    const extent = dataExtent(matching.length ? matching : all, now);
    if (!state.view) {
      const hours = Number(params.get('range'));
      state.view = hours > 0 && hours <= 192 ? [now - hours * 3600e3 * 0.95, now + hours * 3600e3 * 0.05] : extent;
    }
    if (state.follow) {
      const span = state.view[1] - state.view[0];
      if (now > state.view[1] - span * 0.02) state.view = [now + span * 0.04 - span, now + span * 0.04];
    }
    // Only lanes with activity inside the visible window: newest first.
    const lanes = matching.filter((l) => l.last >= state.view[0] && l.first <= state.view[1]);
    $('lane-count').textContent = '(' + lanes.length + (lanes.length !== all.length ? ' of ' + all.length : '') + ')';
    $('empty').hidden = lanes.length > 0 || all.length === 0;
    $('empty').textContent = matching.length ? 'No matching work in this time window. Use Fit to see all of it.' : 'No work matches the filters.';
    const w = chartWidth(), height = Math.max(lanes.length * LANE_H, 1);
    chart.textContent = '';
    chart.setAttribute('viewBox', '0 0 ' + w + ' ' + height);
    chart.setAttribute('width', w);
    chart.setAttribute('height', height);
    chart.style.height = height + 'px';
    defs(chart);
    ticksCache = renderAxis();
    const grid = svg('g', {}, chart);
    lanes.forEach((_, i) => { if (i % 2) svg('rect', { class: 'lane-bg alt', x: 0, y: i * LANE_H, width: w, height: LANE_H }, grid); });
    for (const tk of ticksCache) svg('line', { x1: tk.px, x2: tk.px, y1: 0, y2: height, stroke: tk.major ? '#3a3a37' : 'var(--grid)', 'stroke-width': 1 }, grid);
    lanes.forEach((_, i) => svg('line', { class: 'lane-sep', x1: 0, x2: w, y1: (i + 1) * LANE_H - 0.5, y2: (i + 1) * LANE_H - 0.5 }, grid));

    // Labels column.
    labels.textContent = '';
    lanes.forEach((lane) => {
      const row = html('div', 'label' + (state.selected === lane.key ? ' active' : ''), null, labels);
      row.setAttribute('role', 'listitem');
      row.tabIndex = 0;
      row.dataset.key = lane.key;
      html('span', 'issue', lane.issue ? '#' + lane.issue : lane.workItem.slice(0, 8), row);
      const repo = html('span', 'repo', (lane.repository || '').split('/').pop() + (lane.attempts > 1 ? ' · ×' + lane.attempts : ''), row);
      repo.title = lane.repository;
      const chipText = { inflight: 'in flight', delivered: 'delivered', failed: 'failed', blocked: 'blocked' }[lane.status];
      html('span', 'chip ' + lane.status, chipText, row);
      row.setAttribute('aria-label', (lane.issue ? 'Issue ' + lane.issue : 'Work item') + ', ' + chipText + ', ' + lane.segments.length + ' stages');
    });

    const bars = svg('g', {}, chart);
    lanes.forEach((lane, i) => {
      const y0 = i * LANE_H, cy = y0 + 25;
      const starts = lane.segments.filter((o) => !INSTANT[o.stage]).map((o) => x(o.queued)).sort((a, b) => a - b);
      const dim = state.filter.component;
      for (const s of lane.segments) {
        const g = svg('g', { opacity: dim && s.events[0].component !== dim ? 0.28 : 1 }, bars);
        const xq = x(s.queued), xs = s.start !== null ? x(s.start) : x(s.drawEnd), xe = x(s.drawEnd);
        if (xe < -20 || xq > w + 20) continue;
        // Queue wait: a thin neutral lead-in before the claim.
        if (xs - xq > 1) svg('rect', { x: xq, y: cy - WAIT_H / 2, width: xs - xq, height: WAIT_H, rx: 2, fill: s.outcome === 'waiting' ? 'var(--waiting)' : '#4a4945' }, g);
        if (s.outcome === 'waiting') {
          svg('rect', { x: xq, y: cy - WAIT_H / 2, width: Math.max(xe - xq, 2), height: WAIT_H, rx: 2, fill: 'var(--waiting)' }, g);
        } else if (INSTANT[s.stage]) {
          svg('circle', { cx: xs, cy, r: 4, fill: 'var(--completed)', stroke: 'var(--surface-1)', 'stroke-width': 1.5 }, g);
        } else {
          const width = Math.max(xe - xs, 4);
          svg('rect', { x: xs, y: cy - BAR_H / 2, width, height: BAR_H, rx: 4, fill: fillFor(s.outcome), stroke: 'var(--surface-1)', 'stroke-width': 1 }, g);
          if (s.outcome === 'blocked' || s.outcome === 'quarantined') svg('rect', { x: xs + 0.5, y: cy - BAR_H / 2 + 0.5, width: width - 1, height: BAR_H - 1, rx: 4, fill: 'none', stroke: s.outcome === 'blocked' ? 'var(--blocked)' : 'var(--quarantined)', 'stroke-width': 1 }, g);
          // Model calls: ticks along the bottom edge.
          if (width > 30) for (const c of s.calls) {
            const px = x(c.t);
            if (px > xs + 2 && px < xs + width - 2) svg('line', { x1: px, x2: px, y1: cy + BAR_H / 2 - 4, y2: cy + BAR_H / 2 - 1, stroke: 'rgba(14,14,13,.55)', 'stroke-width': 1 }, g);
          }
          const glyph = width > 18 && (s.outcome === 'completed' || s.outcome === 'failed' || s.outcome === 'stopped-safely');
          if (glyph) endGlyph(g, s.outcome, xs + width - 9, cy);
          const base = STAGE_LABEL[s.stage] + (s.attempt > 1 && s.stage === 'execution' ? ' #' + s.attempt : '');
          const stopped = (s.outcome === 'failed' || s.outcome === 'stopped-safely') && s.diagnostic;
          const room = (starts.find((v) => v > xe + 1) || Infinity) - xe;
          const outside = stopped && room > s.diagnostic.length * 6.4 + 12;
          // Without room after the bar, the diagnostic goes inside it when it fits.
          const fits = (text) => width > text.length * 6.2 + 10 + (glyph ? 16 : 0);
          const inside = stopped && !outside && fits(base + ' · ' + s.diagnostic) ? base + ' · ' + s.diagnostic : base;
          if (fits(inside)) {
            const t = svg('text', { class: 'bar-label' + (s.outcome === 'blocked' || s.outcome === 'quarantined' ? ' light' : ''), x: xs + 6, y: cy + 4 }, g);
            t.textContent = inside;
          }
          if (outside) {
            const d = svg('text', { class: 'diag', x: xs + width + 6, y: cy + 4 }, g);
            d.textContent = s.diagnostic;
          }
        }
        const hit = svg('rect', { class: 'hit', x: Math.min(xq, xs) - 3, y: y0 + 4, width: Math.max(xe - Math.min(xq, xs), 6) + 6, height: LANE_H - 8 }, g);
        hit.addEventListener('mousemove', (ev) => showTooltip(ev, segmentTooltip(lane, s)));
        hit.addEventListener('mouseleave', hideTooltip);
        hit.addEventListener('click', () => openPanel(lane.key));
      }
      const mk = svg('g', {}, bars);
      for (const m of lane.markers) {
        if (m.type === 'failed' || m.type === 'stopped-safely') continue;
        const px = x(m.e.t);
        if (px < -10 || px > w + 10) continue;
        const my = y0 + 9;
        const g = svg('g', {}, mk);
        marker(g, m.type, px, my);
        const hit = svg('circle', { class: 'hit', cx: px, cy: my, r: 9 }, g);
        hit.addEventListener('mousemove', (ev) => showTooltip(ev, markerTooltip(lane, m)));
        hit.addEventListener('mouseleave', hideTooltip);
        hit.addEventListener('click', () => openPanel(lane.key));
      }
    });

    // Now line.
    const nx = x(now);
    if (nx >= 0 && nx <= w) {
      svg('line', { class: 'now-line', x1: nx, x2: nx, y1: 0, y2: height }, chart);
      const g = svg('g', { class: 'tick' }, axis);
      svg('rect', { class: 'now-pill', x: nx - 15, y: 22, width: 30, height: 12, rx: 3 }, g);
      const t = svg('text', { class: 'now-label', x: nx, y: 31, 'text-anchor': 'middle' }, g);
      t.textContent = 'NOW';
    }
    if (state.selected) renderPanel();
    state.dirty = false;
  }

  // ---------- tooltip ----------
  function segmentTooltip(lane, s) {
    const rows = [];
    rows.push(['Outcome', OUTCOME_TEXT[s.outcome] || s.outcome]);
    if (s.diagnostic) rows.push(['Diagnostic', s.diagnostic]);
    if (s.attempt) rows.push(['Attempt', String(s.attempt)]);
    rows.push(['Component', s.events[0].component]);
    if (s.start !== null && s.queued < s.start) rows.push(['Waited', fmtDuration(s.start - s.queued)]);
    if (s.start !== null) rows.push(['Started', fmtClock(s.start, true)]);
    if (s.end !== null) rows.push(['Ended', fmtClock(s.end, true)]);
    if (s.start !== null && !INSTANT[s.stage]) rows.push(['Duration', fmtDuration((s.end !== null ? s.end : Date.now()) - s.start) + (s.end === null ? ' (so far)' : '')]);
    if (s.calls.length) {
      const lat = s.calls.map((c) => c.latencyMs).filter(Boolean);
      rows.push(['Model calls', s.calls.length + (lat.length ? ' · p50 ' + fmtDuration(median(lat)) + ' · max ' + fmtDuration(Math.max.apply(null, lat)) : '')]);
    }
    if (s.effects) rows.push(['Effects', String(s.effects)]);
    if (s.degraded.length) rows.push(['Degraded', s.degraded.map((d) => d.diagnostic).join(', ')]);
    if (s.stale) rows.push(['Stale wait', 'queued over ' + fmtDuration(Number(s.stale.attributes && s.stale.attributes.waitedMs) || 0)]);
    const attrs = s.terminal && s.terminal.attributes;
    if (attrs) for (const k of ['phase', 'exitCode', 'changes', 'reason']) if (attrs[k]) rows.push([k, attrs[k]]);
    const title = (lane.issue ? '#' + lane.issue + ' · ' : '') + s.stage;
    return { title, rows };
  }
  function markerTooltip(lane, m) {
    const e = m.e, rows = [['Time', fmtClock(e.t, true)], ['Stage', e.stage]];
    if (e.diagnostic) rows.push(['Diagnostic', e.diagnostic]);
    if (e.attempt) rows.push(['Attempt', String(e.attempt)]);
    if (e.latencyMs) rows.push(['Latency', fmtDuration(e.latencyMs)]);
    for (const k in e.attributes || {}) { const f = fmtAttribute(k, e.attributes[k]), at = f.indexOf(' '); rows.push([f.slice(0, at), f.slice(at + 1)]); }
    const names = { degraded: 'Degraded', retried: 'Retried', stale: 'Stale wait', quarantined: 'Quarantined', blocked: 'Blocked' };
    return { title: (lane.issue ? '#' + lane.issue + ' · ' : '') + names[m.type], rows };
  }
  function showTooltip(ev, content) {
    tooltip.textContent = '';
    html('div', 'tt-title', content.title, tooltip);
    const dl = html('dl', null, null, tooltip);
    for (const [k, v] of content.rows) { html('dt', null, k, dl); html('dd', null, v, dl); }
    tooltip.hidden = false;
    const pad = 14, r = tooltip.getBoundingClientRect();
    let left = ev.clientX + pad, top = ev.clientY + pad;
    if (left + r.width > window.innerWidth - 8) left = ev.clientX - r.width - pad;
    if (top + r.height > window.innerHeight - 8) top = ev.clientY - r.height - pad;
    tooltip.style.left = left + 'px';
    tooltip.style.top = top + 'px';
  }
  function hideTooltip() { tooltip.hidden = true; }

  // ---------- detail panel ----------
  const KIND_COLOR = { completed: 'var(--completed)', failed: 'var(--failed)', 'stopped-safely': 'var(--stopped)', started: 'var(--running)', progressed: '#4a6f99',
    waiting: 'var(--waiting)', retried: 'var(--text-primary)', quarantined: 'var(--quarantined)', degraded: 'var(--degraded)', blocked: 'var(--blocked)' };
  function openPanel(key) { state.selected = key; $('panel').hidden = false; document.body.classList.add('panel-open'); renderPanel(); render(); $('panel-close').focus(); }
  function closePanel() { state.selected = null; $('panel').hidden = true; document.body.classList.remove('panel-open'); render(); }
  function renderPanel() {
    const lane = state.lanes.get(state.selected);
    if (!lane) return;
    $('panel-title').textContent = lane.issue ? 'Issue #' + lane.issue : 'Work item';
    const meta = $('panel-meta');
    meta.textContent = '';
    const add = (k, v) => { html('dt', null, k, meta); html('dd', null, v, meta); };
    if (lane.repository) add('Repository', lane.repository);
    add('Status', { inflight: 'in flight', delivered: 'delivered', failed: 'failed', blocked: 'blocked or quarantined' }[lane.status]);
    add('Attempts', String(Math.max(lane.attempts, 1)));
    if (lane.delivery !== null) add('Time to delivery', fmtDuration(lane.delivery));
    add('Events', String(lane.events.length));
    const list = $('panel-events');
    list.textContent = '';
    let group = '';
    const events = lane.events;
    for (let i = 0; i < events.length; i++) {
      const e = events[i];
      const g = e.attempt ? 'Attempt ' + e.attempt : 'Intake and planning';
      if (g !== group) { group = g; html('li', 'group', g, list); }
      // Consecutive successful model calls of one stage collapse into one row.
      let j = i;
      const isCall = (o) => o && o.kind === 'progressed' && o.step === 'model-call' && o.stage === e.stage && o.attempt === e.attempt;
      while (isCall(e) && isCall(events[j + 1])) j++;
      const li = html('li', null, null, list);
      html('span', 'time', fmtClock(e.t, true), li);
      const sw = html('span', 'glyph', null, li);
      sw.style.background = KIND_COLOR[e.kind] || 'var(--waiting)';
      sw.setAttribute('aria-hidden', 'true');
      const details = [];
      if (j > i) {
        const calls = events.slice(i, j + 1), lat = calls.map((c) => c.latencyMs).filter(Boolean);
        html('span', 'what', e.stage + ' · ' + calls.length + ' model calls — progressed', li);
        details.push('until ' + fmtClock(events[j].t, true));
        if (lat.length) details.push('latency p50 ' + fmtDuration(median(lat)) + ' · max ' + fmtDuration(Math.max.apply(null, lat)));
        i = j;
      } else {
        html('span', 'what', e.stage + (e.step ? ' · ' + e.step : '') + ' — ' + e.kind + (e.diagnostic ? ' (' + e.diagnostic + ')' : ''), li);
        if (e.durationMs) details.push('duration ' + fmtDuration(e.durationMs));
        if (e.latencyMs) details.push('latency ' + fmtDuration(e.latencyMs));
        for (const k in e.attributes || {}) details.push(fmtAttribute(k, e.attributes[k]));
      }
      details.push(e.component);
      html('span', 'detail', details.join(' · '), li);
    }
  }
  function fmtAttribute(k, v) {
    if (/Ms$/.test(k) && /^\d+$/.test(v)) return k.slice(0, -2) + ' ' + fmtDuration(Number(v));
    if (/Bytes$/.test(k) && /^\d+$/.test(v)) return k.slice(0, -5) + ' ' + (Number(v) >= 1024 ? (Number(v) / 1024).toFixed(1) + ' KiB' : v + ' B');
    return k + ' ' + v;
  }

  // ---------- legend ----------
  function legend() {
    const ul = $('legend');
    const item = (draw, text) => {
      const li = html('li', null, null, ul);
      const s = svg('svg', { width: 34, height: 18, viewBox: '0 0 34 18', 'aria-hidden': 'true' }, li);
      draw(s);
      html('span', null, text, li);
    };
    const bar = (fill, outcome, border) => (s) => {
      defs(s);
      svg('rect', { x: 1, y: 1, width: 32, height: 16, rx: 4, fill, stroke: border || 'none' }, s);
      if (outcome) endGlyph(s, outcome, 24, 9);
    };
    item(bar('var(--completed)', 'completed'), 'Completed ✓');
    item(bar('url(#stripes-running)'), 'Running (open end)');
    item((s) => svg('rect', { x: 1, y: 7, width: 32, height: 4, rx: 2, fill: 'var(--waiting)' }, s), 'Waiting in queue');
    item(bar('var(--failed)', 'failed'), 'Failed: verdict against the work ✕');
    item(bar('var(--stopped)', 'stopped-safely'), 'Stopped safely, with diagnostic ■');
    item(bar('url(#hatch-blocked)', null, 'var(--blocked)'), 'Blocked (lease lapsed or needs a person)');
    item(bar('url(#hatch-quarantined)', null, 'var(--quarantined)'), 'Quarantined');
    item((s) => marker(s, 'degraded', 17, 10), 'Degraded (model call, cleanup retry)');
    item((s) => marker(s, 'retried', 17, 9), 'Retried attempt');
    item((s) => marker(s, 'stale', 17, 9), 'Stale wait');
    item((s) => marker(s, 'quarantined', 17, 9), 'Quarantine marker');
    item((s) => { svg('rect', { x: 1, y: 1, width: 32, height: 16, rx: 4, fill: '#3a6ea8' }, s); for (const px of [8, 14, 18, 26]) svg('line', { x1: px, x2: px, y1: 13, y2: 16, stroke: 'rgba(14,14,13,.7)' }, s); }, 'Model calls (ticks)');
    item((s) => svg('circle', { cx: 17, cy: 9, r: 4, fill: 'var(--completed)' }, s), 'Intake observed / work item planned');
  }

  // ---------- interaction ----------
  function setView(t0, t1, follow) {
    const span = Math.min(Math.max(t1 - t0, 5 * 60e3), 8 * 86400e3);
    const mid = (t0 + t1) / 2;
    state.view = [mid - span / 2, mid + span / 2];
    if (follow !== undefined) setFollow(follow);
    schedule();
  }
  function setFollow(on) { state.follow = on; $('follow').setAttribute('aria-pressed', String(on)); }

  document.querySelectorAll('[data-range]').forEach((b) => b.addEventListener('click', () => {
    const now = Date.now(), r = b.dataset.range;
    if (r === 'fit') {
      const lanes = visibleLanes();
      const [lo, hi] = dataExtent(lanes.length ? lanes : Array.from(state.lanes.values()), now);
      setView(lo, hi, false);
    } else {
      const span = Number(r) * 3600e3;
      setView(now - span * 0.95, now + span * 0.05, true);
    }
  }));
  $('follow').addEventListener('click', () => { setFollow(!state.follow); schedule(); });
  $('status-filter').addEventListener('click', (ev) => {
    const b = ev.target.closest('button');
    if (!b) return;
    state.filter.status = b.dataset.status;
    document.querySelectorAll('#status-filter button').forEach((o) => o.setAttribute('aria-pressed', String(o === b)));
    schedule();
  });
  $('component-filter').addEventListener('change', (ev) => { state.filter.component = ev.target.value; schedule(); });
  $('issue-filter').addEventListener('input', (ev) => { state.filter.issue = ev.target.value; schedule(); });
  $('panel-close').addEventListener('click', closePanel);
  document.addEventListener('keydown', (ev) => { if (ev.key === 'Escape' && state.selected) closePanel(); });
  labels.addEventListener('click', (ev) => { const row = ev.target.closest('.label'); if (row) openPanel(row.dataset.key); });
  labels.addEventListener('keydown', (ev) => {
    const row = ev.target.closest('.label');
    if (!row) return;
    if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); openPanel(row.dataset.key); }
    if (ev.key === 'ArrowDown' && row.nextElementSibling) { ev.preventDefault(); row.nextElementSibling.focus(); }
    if (ev.key === 'ArrowUp' && row.previousElementSibling) { ev.preventDefault(); row.previousElementSibling.focus(); }
  });

  chart.addEventListener('wheel', (ev) => {
    if (!(ev.ctrlKey || ev.metaKey) && Math.abs(ev.deltaX) <= Math.abs(ev.deltaY)) return;
    ev.preventDefault();
    const rect = chart.getBoundingClientRect(), [t0, t1] = state.view;
    if (ev.ctrlKey || ev.metaKey) {
      const at = tAt(ev.clientX - rect.left), k = Math.exp(ev.deltaY * 0.0015);
      const span = Math.min(Math.max((t1 - t0) * k, 5 * 60e3), 8 * 86400e3);
      const ratio = (at - t0) / (t1 - t0);
      state.view = [at - span * ratio, at - span * ratio + span];
    } else {
      const shift = ev.deltaX / chartWidth() * (t1 - t0);
      state.view = [t0 + shift, t1 + shift];
    }
    setFollow(false);
    schedule();
  }, { passive: false });
  let drag = null;
  chart.addEventListener('pointerdown', (ev) => {
    if (ev.button !== 0) return;
    drag = { x: ev.clientX, view: state.view.slice(), moved: false };
  });
  window.addEventListener('pointermove', (ev) => {
    if (!drag) return;
    const dx = ev.clientX - drag.x;
    if (Math.abs(dx) > 3) { drag.moved = true; lanesBox.classList.add('dragging'); hideTooltip(); }
    if (!drag.moved) return;
    const shift = -dx / chartWidth() * (drag.view[1] - drag.view[0]);
    state.view = [drag.view[0] + shift, drag.view[1] + shift];
    setFollow(false);
    schedule();
  });
  window.addEventListener('pointerup', () => { if (drag) { lanesBox.classList.remove('dragging'); drag = null; } });
  window.addEventListener('resize', schedule);

  let pending = false;
  function schedule() {
    if (pending) return;
    pending = true;
    requestAnimationFrame(() => { pending = false; render(); });
  }

  // ---------- stream ----------
  function setSource(text, cls) {
    $('source-text').textContent = text;
    $('source-dot').className = 'dot' + (cls ? ' ' + cls : '');
  }
  function describeStatus() {
    const s = state.status;
    if (!state.connected) return setSource('reconnecting…', 'down');
    if (!s) return setSource('connected', 'live');
    const source = s.source === 'fixture' ? 'recorded fixture' : 'journal';
    if (!s.healthy) return setSource(source + ' unavailable — showing last known events', 'down');
    const when = s.observedAt ? ' · read ' + fmtClock(Date.parse(s.observedAt), true) : '';
    setSource('live · ' + source + (s.problem === 'window-truncated' ? ' · window truncated' : '') + when, 'live');
  }
  function connect() {
    const stream = new EventSource('events');
    stream.addEventListener('open', () => { state.connected = true; describeStatus(); });
    stream.addEventListener('flow', (m) => { try { addEvent(JSON.parse(m.data)); schedule(); } catch (_) { /* ignore malformed */ } });
    stream.addEventListener('status', (m) => { try { state.status = JSON.parse(m.data); describeStatus(); } catch (_) { /* ignore */ } });
    stream.addEventListener('ready', () => {
      state.connected = true; describeStatus(); schedule();
      const open = params.get('open');
      if (open && state.lanes.has(open) && !state.selected) openPanel(open);
    });
    stream.addEventListener('gap', () => { setSource('events were missed while disconnected; reload for full history', 'down'); });
    stream.addEventListener('error', () => { state.connected = false; describeStatus(); });
  }

  legend();
  if (params.get('issue')) { state.filter.issue = params.get('issue').slice(0, 100); $('issue-filter').value = state.filter.issue; }
  const initialStatus = params.get('status');
  if (['inflight', 'delivered', 'failed', 'blocked'].indexOf(initialStatus) >= 0) {
    state.filter.status = initialStatus;
    document.querySelectorAll('#status-filter button').forEach((o) => o.setAttribute('aria-pressed', String(o.dataset.status === initialStatus)));
  }
  connect();
  // Advance the now line and open bars.
  setInterval(schedule, 5000);
})();
