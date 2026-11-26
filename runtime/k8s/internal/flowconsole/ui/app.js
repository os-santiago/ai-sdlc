// AI-SDLC flow console: vanilla JS, no dependencies. It consumes
// ai-sdlc.flow-event/v1 events from /events (Server-Sent Events) and draws one
// swimlane per issue with ordered stage bars. All event text is inserted with
// textContent or SVG text nodes, never as HTML.
//
// Encoding: the stage is the bar's hue (one fixed hue per stage, in flow
// order) plus its label or initial; the outcome is separate: a solid bar is
// completed, stripes are running, an outline is waiting, a hatch is blocked
// or quarantined, and failed, stopped, blocked and quarantined bars carry an
// end badge with their own glyph. Markers sit on their own row above the bars
// and cluster into a counted badge when they would overlap.
(function () {
  'use strict';

  const STAGES = ['intake', 'admission', 'planning', 'execution', 'verification', 'review', 'publication', 'merge'];
  const STAGE_INITIAL = { intake: 'I', admission: 'A', planning: 'P', execution: 'G', verification: 'V', review: 'R', publication: 'P', merge: 'M' };
  // Categorical hues validated for the dark surface, in the palette's
  // CVD-safe adjacent order and assigned in flow order.
  // Light steps are the same hues stepped for the light surface.
  const STAGE_COLORS = {
    dark: { intake: '#8f8e86', admission: '#8f8e86', planning: '#8f8e86', execution: '#3987e5', verification: '#d95926', review: '#199e70', publication: '#c98500', merge: '#d55181' },
    light: { intake: '#8f8e86', admission: '#8f8e86', planning: '#8f8e86', execution: '#3987e5', verification: '#eb6834', review: '#1baf7a', publication: '#eda100', merge: '#e87ba4' },
  };
  const STAGE_COLOR = Object.assign({}, STAGE_COLORS.dark);

  // ---------- language ----------
  const STRINGS = window.FLOW_I18N || { en: {} };
  let lang = 'en';
  function t(key, vars) {
    const table = STRINGS[lang] || STRINGS.en;
    let value = table[key] !== undefined ? table[key] : (STRINGS.en[key] !== undefined ? STRINGS.en[key] : key);
    if (vars) value = value.replace(/\{(\w+)\}/g, (m, k) => (vars[k] !== undefined ? String(vars[k]) : m));
    return value;
  }
  const stageName = (stage) => t('stage.' + stage);
  const stageVerb = (stage) => (STRINGS.en['verb.' + stage] ? t('verb.' + stage) : stageName(stage));
  const kindLabel = (kind) => t('kind.' + kind);
  const outcomeText = (outcome) => t('outcome.' + outcome);
  const markerName = (type) => t('marker.' + type);
  // Issue links: built only from the validated repository pattern and the
  // integer issue number, never from other event data.
  const REPOSITORY = /^[a-z0-9][a-z0-9._-]{0,99}\/[a-z0-9][a-z0-9._-]{0,99}$/;
  function issueLink(parent, repository, issue, textValue, cls) {
    if (!REPOSITORY.test(repository || '') || !Number.isInteger(issue) || issue <= 0) return html('span', cls, textValue, parent);
    const a = html('a', cls, textValue, parent);
    a.href = 'https://github.com/' + repository + '/issues/' + issue;
    a.target = '_blank';
    a.rel = 'noopener noreferrer';
    a.title = t('issue.link', { n: issue });
    return a;
  }
  const INSTANT = { intake: true, planning: true };
  const MARKER_PRIORITY = { quarantined: 5, blocked: 4, degraded: 3, stale: 2, retried: 1 };
  const NS = 'http://www.w3.org/2000/svg';

  const state = {
    events: new Map(),
    lanes: new Map(),
    status: null,
    connected: false,
    gap: false,
    view: null,            // [t0, t1] in ms
    follow: true,
    sort: 'recent',
    filter: { status: 'all', component: '', issue: '', current: '' },
    feedOpen: false,
    replaying: true,       // receiving the initial replay: render once at "ready"
    focusIndex: -1,
    selected: null,
  };

  const $ = (id) => document.getElementById(id);
  // Shareable view: ?range=<hours>&status=<status>&issue=<text>&open=<lane key>&sort=problems
  const params = new URLSearchParams(location.search);
  const chart = $('chart'), axis = $('axis'), labels = $('labels'), lanesBox = $('lanes'), tooltip = $('tooltip');
  const css = getComputedStyle(document.documentElement);
  const laneHeight = () => parseFloat(css.getPropertyValue('--lane-h')) || 44;
  const BAR_H = 16;

  function svg(tag, attrs, parent) {
    const node = document.createElementNS(NS, tag);
    for (const k in attrs) node.setAttribute(k, attrs[k]);
    if (parent) parent.appendChild(node);
    return node;
  }
  function text(parent, attrs, value) {
    const node = svg('text', attrs, parent);
    node.textContent = value;
    return node;
  }
  function html(tag, cls, value, parent) {
    const node = document.createElement(tag);
    if (cls) node.className = cls;
    if (value !== undefined && value !== null) node.textContent = value;
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
  function fmtUTC(t) { return new Date(t).toISOString().replace('T', ' ').replace(/\.\d+Z$/, ' UTC'); }
  const tzOffset = (() => {
    const m = -new Date().getTimezoneOffset(), sign = m >= 0 ? '+' : '−', a = Math.abs(m);
    return 'UTC' + sign + pad(Math.floor(a / 60)) + ':' + pad(a % 60);
  })();
  const tzName = (() => { try { return Intl.DateTimeFormat().resolvedOptions().timeZone || ''; } catch (_) { return ''; } })();
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
  function fmtAttribute(k, v) {
    if (/Ms$/.test(k) && /^\d+$/.test(v)) return k.slice(0, -2) + ' ' + fmtDuration(Number(v));
    if (/Bytes$/.test(k) && /^\d+$/.test(v)) return k.slice(0, -5) + ' ' + (Number(v) >= 1024 ? (Number(v) / 1024).toFixed(1) + ' KiB' : v + ' B');
    return k + ' ' + v;
  }

  // ---------- model ----------
  function laneKey(e) {
    if (e.issue) return (e.repository || '') + '#' + e.issue;
    return 'work-item/' + e.workItem;
  }

  let arrivals = 0;
  function addEvent(e) {
    if (state.events.has(e.id)) return;
    arrivals++;
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
  }

  // After a merge, intake sees the closed issue, admission records the
  // server-labelled issue-closed step, and sometimes refuses the closed
  // issue's next revisions (step after-issue-closed). From each merge up to
  // the first accepted admission or new work item, those segments and the
  // intake revisions leading to them fold into one neutral "closed" flag per
  // merge; they keep their event kinds in the detail panel and count as
  // neither failures nor stops. Any other refusal or block after the merge
  // (a reopened issue) stays visible as a problem.
  const CLOSURE_STEPS = { 'issue-closed': true, 'after-issue-closed': true };
  function markPostDelivery(lane, list, merges) {
    lane.closed = [];
    for (const s of list) s.postDelivery = false;
    const ordered = list.slice().sort((a, b) => a.first - b.first);
    const step = (s) => (s.terminal || s.events[0]).step;
    const closure = (s) => s.stage === 'admission' && s.outcome !== 'blocked' && CLOSURE_STEPS[step(s)];
    merges.forEach((m, i) => {
      const next = merges[i + 1] ? merges[i + 1].end : Infinity;
      let stop = next;
      for (const s of ordered) {
        if (s.first < m.end || s.first >= stop) continue;
        if (s.stage === 'planning' || (s.stage === 'admission' && s.outcome === 'completed' && !closure(s))) stop = s.first;
      }
      let group = null;
      ordered.forEach((s, k) => {
        if (s.first < m.end || s.first >= stop || STAGES.indexOf(s.stage) >= 2) return;
        let fold = closure(s);
        if (s.stage === 'intake') {
          const adm = ordered.slice(k + 1).find((x) => x.stage === 'admission');
          fold = adm ? adm.first < stop && closure(adm) : true;
        }
        if (!fold) return;
        s.postDelivery = true;
        if (!group) lane.closed.push(group = { at: s.first, segments: [] });
        group.segments.push(s);
      });
    });
    for (const s of list) for (const e of s.events) e.postDelivery = s.postDelivery;
  }

  // Build stage segments of one lane: one per (attempt, stage), in order.
  function buildLane(lane, now) {
    lane.events.sort((a, b) => a.t - b.t || STAGES.indexOf(a.stage) - STAGES.indexOf(b.stage));
    const segments = new Map(), markers = [];
    let attempts = 0;
    for (const e of lane.events) {
      const attempt = e.attempt || 0;
      attempts = Math.max(attempts, attempt);
      const key = (STAGES.indexOf(e.stage) < 3 ? 'pre' : (e.workItem + '/' + attempt)) + '/' + e.stage + (e.stage === 'admission' || e.stage === 'intake' ? '/' + e.id : e.stage === 'planning' ? '/' + (e.workItem || '') : '');
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
      if (s.quarantined) { s.outcome = 'quarantined'; s.diagnostic = s.diagnostic || s.quarantined.diagnostic || ''; }
      else if (s.end === null) {
        if (s.blocked) { s.outcome = 'blocked'; s.diagnostic = s.blocked.diagnostic || ''; }
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
    lane.markers = markers.sort((a, b) => a.e.t - b.e.t);
    lane.attempts = attempts;
    lane.components = new Set(lane.events.map((e) => e.component));
    // Lane status and where its work is now, from the newest work.
    const merges = list.filter((s) => s.stage === 'merge' && s.outcome === 'completed').sort((a, b) => a.end - b.end);
    const merged = merges[0] || null, lastMerge = merges[merges.length - 1] || null;
    markPostDelivery(lane, list, merges);
    // Status comes from the work after the most recent merge: a reopened
    // issue's refusal, block or new work shows; before that merge, delivered.
    const after = lastMerge ? list.filter((s) => !s.postDelivery && s.first >= lastMerge.end) : list;
    const open = after.filter((s) => s.open && s.outcome !== 'quarantined');
    const problem = after.filter((s) => s.outcome === 'quarantined' || (s.outcome === 'blocked' && (s.open || s.stage === 'admission'))).pop();
    const lastTerminal = after.filter((s) => s.end !== null && !INSTANT[s.stage]).sort((a, b) => a.end - b.end).pop();
    lane.final = null;
    if (problem) { lane.status = 'blocked'; lane.final = problem; }
    else if (open.length) lane.status = 'inflight';
    else if (lastTerminal && lastTerminal.outcome !== 'completed') { lane.status = 'failed'; lane.final = lastTerminal; }
    else if (merged) lane.status = 'delivered';
    else lane.status = 'inflight';
    const current = open.length ? open[open.length - 1] : null;
    lane.current = lane.status === 'blocked' ? 'blocked' : current ? (current.outcome === 'waiting' ? 'queued' : current.stage) : null;
    lane.currentSegment = current;
    lane.first = list.length ? Math.min.apply(null, list.map((s) => s.queued)) : now;
    lane.last = list.length ? Math.max.apply(null, list.map((s) => s.drawEnd)) : now;
    // Sorting uses the newest event, not the end of an open bar (which is now).
    lane.lastActivity = lane.events.length ? lane.events[lane.events.length - 1].t : lane.first;
    lane.delivery = merged ? merged.end - lane.first : null;
    lane.stopped = list.filter((s) => !s.postDelivery && (s.outcome === 'failed' || s.outcome === 'stopped-safely')).length;
    lane.hasOpen = list.some((s) => s.open);
    lane.dirty = false;
  }

  // One short line under the issue number: where the work is or why it ended.
  function laneSummary(lane, now) {
    if (lane.status === 'delivered') return { text: t('sum.merged', { d: fmtDuration(lane.delivery) }), cls: '' };
    if (lane.final) return { text: stageName(lane.final.stage) + ' · ' + (lane.final.diagnostic || outcomeText(lane.final.outcome)), cls: 'problem' };
    const s = lane.currentSegment;
    if (s && s.outcome === 'waiting') return { text: t('queued.for', { stage: stageName(s.stage) }) + ' · ' + fmtDuration(now - s.queued), cls: '' };
    if (s) return { text: stageVerb(s.stage) + ' · ' + fmtDuration(now - (s.start || s.queued)), cls: '' };
    return { text: t('sum.planned'), cls: '' };
  }

  const STATUS_RANK = { blocked: 0, failed: 1, inflight: 2, delivered: 3 };
  function visibleLanes() {
    const f = state.filter, q = f.issue.trim().toLowerCase().replace(/^#/, '');
    const out = Array.from(state.lanes.values()).filter((l) => {
      if (f.status !== 'all' && l.status !== f.status) return false;
      if (f.component && !l.components.has(f.component)) return false;
      if (f.current && l.current !== f.current) return false;
      if (q) {
        const value = (l.repository + '#' + l.issue + ' ' + l.workItem).toLowerCase();
        if (String(l.issue) !== q && value.indexOf(q) < 0) return false;
      }
      return true;
    });
    if (state.sort === 'problems') out.sort((a, b) => STATUS_RANK[a.status] - STATUS_RANK[b.status] || b.lastActivity - a.lastActivity);
    else out.sort((a, b) => b.lastActivity - a.lastActivity || b.first - a.first);
    return out;
  }

  // ---------- summary ----------
  function renderTiles(all) {
    const count = (s) => all.filter((l) => l.status === s).length;
    $('t-inflight').textContent = count('inflight');
    const running = all.reduce((n, l) => n + l.segments.filter((s) => s.outcome === 'running').length, 0);
    const waiting = all.reduce((n, l) => n + l.segments.filter((s) => s.outcome === 'waiting').length, 0);
    $('t-inflight-note').textContent = t('note.inflight', { running, waiting });
    const delivered = all.filter((l) => l.status === 'delivered');
    $('t-delivered').textContent = delivered.length;
    const retriedDelivered = delivered.filter((l) => l.attempts > 1).length;
    $('t-delivered-note').textContent = all.length ? t('note.delivered', { first: delivered.length - retriedDelivered, retried: retriedDelivered }) : '';
    $('t-failed').textContent = count('failed');
    const stops = all.reduce((n, l) => n + l.stopped, 0);
    $('t-failed-note').textContent = t('note.failed', { n: stops });
    $('t-blocked').textContent = count('blocked');
    const stale = all.reduce((n, l) => n + l.markers.filter((m) => m.type === 'stale').length, 0);
    const quarantined = all.reduce((n, l) => n + l.markers.filter((m) => m.type === 'quarantined').length, 0);
    const reasons = blockedReasons(all);
    const note = reasons.length ? reasons.join(' · ') : t('note.blocked', { q: quarantined, s: stale });
    $('t-blocked-note').textContent = note;
    $('t-blocked-note').title = note;
    const ttd = delivered.map((l) => l.delivery).filter((v) => v !== null);
    $('t-ttd').textContent = ttd.length ? fmtDuration(median(ttd)) : '–';
    $('t-ttd-note').textContent = ttd.length ? t('note.ttd', { n: ttd.length }) : t('note.ttd.none');
    const lat = [];
    let degraded = 0;
    for (const l of all) for (const s of l.segments) { for (const c of s.calls) if (c.latencyMs) lat.push(c.latencyMs); degraded += s.degraded.length; }
    $('t-latency').textContent = lat.length ? fmtDuration(median(lat)) : '–';
    $('t-latency-note').textContent = lat.length ? t('note.latency', { p90: fmtDuration(quantile(lat, 0.9)), n: lat.length, d: degraded }) : t('note.latency.none');
  }

  // What blocks: "1 publication quarantined · 1 needs clarification · 1 lease lapsed".
  function blockReason(lane) {
    const s = lane.final;
    if (!s) return t('blocked');
    if (s.outcome === 'quarantined') return t('quarantined.what', { noun: t('noun.' + s.stage) });
    return s.diagnostic ? s.diagnostic.replace(/-/g, ' ') : t('blocked');
  }
  function blockedReasons(all) {
    const counts = new Map();
    for (const l of all) if (l.status === 'blocked') { const r = blockReason(l); counts.set(r, (counts.get(r) || 0) + 1); }
    return Array.from(counts).sort((a, b) => b[1] - a[1]).map(([r, n]) => n + ' ' + r);
  }

  // ---------- what is wrong now ----------
  // Blocked or quarantined lanes, lanes in a stage longer than that stage's
  // p90 over the visible window, and degradation bursts; newest first.
  function problems(all, now) {
    const out = [];
    for (const l of all) if (l.status === 'blocked' && l.final) {
      const at = (l.final.quarantined || l.final.blocked || l.final.terminal || { t: l.final.drawEnd }).t;
      out.push({ at, key: l.key, kind: 'blocked', text: (l.issue ? '#' + l.issue + ' ' : '') + blockReason(l) + (l.final.diagnostic && l.final.outcome === 'quarantined' ? ' · ' + l.final.diagnostic : ''), note: t('problems.ago', { d: fmtDuration(now - at) }) });
    }
    // Typical durations: completed stage runs and waits inside the visible window.
    const runs = {}, waits = {};
    for (const l of all) for (const s of l.segments) {
      if (INSTANT[s.stage] || s.end === null || s.end < state.view[0] || s.start === null) continue;
      (runs[s.stage] = runs[s.stage] || []).push(s.end - s.start);
      if (s.start > s.queued) (waits[s.stage] = waits[s.stage] || []).push(s.start - s.queued);
    }
    for (const l of all) {
      const s = l.currentSegment;
      if (!s || l.status === 'blocked') continue;
      const waiting = s.outcome === 'waiting', sample = (waiting ? waits : runs)[s.stage] || [];
      if (sample.length < 3) continue;
      const p90 = quantile(sample, 0.9), age = now - (waiting ? s.queued : s.start);
      if (age > p90 && age > 60e3) {
        out.push({ at: now - age + p90, key: l.key, kind: 'slow', text: (l.issue ? '#' + l.issue + ' ' : '') + (waiting ? t('queued.for', { stage: stageName(s.stage) }) : stageVerb(s.stage)) + ' ' + fmtDuration(age), note: 'p90 ' + fmtDuration(p90) });
      }
    }
    for (const b of bands(all)) out.push({ at: b.b, band: b, kind: 'burst', text: t('problems.burst', { n: b.count }), note: fmtClock(b.a) + '–' + fmtClock(b.b) });
    return out.sort((a, b) => b.at - a.at);
  }
  function renderProblems(all, now) {
    const box = $('problems'), list = $('problem-list');
    const items = problems(all, now);
    box.hidden = items.length === 0;
    $('problem-count').textContent = String(items.length);
    list.textContent = '';
    const limit = state.problemsExpanded ? items.length : 4;
    for (const p of items.slice(0, limit)) {
      const li = html('li', 'problem-' + p.kind, null, list);
      const b = html('button', null, null, li);
      b.type = 'button';
      if (p.key) b.dataset.key = p.key;
      if (p.band) { b.dataset.a = String(p.band.a); b.dataset.b = String(p.band.b); }
      html('span', 'problem-icon', { blocked: '⏸', slow: '⏱', burst: '▲' }[p.kind], b).setAttribute('aria-hidden', 'true');
      html('span', 'problem-text', p.text, b);
      html('span', 'problem-note', p.note, b);
      b.title = p.text + ' · ' + p.note;
    }
    const more = $('problem-more');
    more.hidden = items.length <= 4;
    more.textContent = state.problemsExpanded ? t('problems.fewer') : t('problems.more', { n: items.length });
  }

  // ---------- trends over the visible window ----------
  const BUCKETS = 24;
  function trends(all) {
    const [t0, t1] = state.view, width = (t1 - t0) / BUCKETS;
    const lat = Array.from({ length: BUCKETS }, () => []), stops = new Array(BUCKETS).fill(0), delivered = new Array(BUCKETS).fill(0);
    const at = (t) => Math.floor((t - t0) / width);
    for (const l of all) for (const e of l.events) {
      if (e.t < t0 || e.t >= t1 || e.postDelivery) continue;
      const i = at(e.t);
      if (e.step === 'model-call' && e.kind === 'progressed' && e.latencyMs) lat[i].push(e.latencyMs);
      else if (e.kind === 'failed' || e.kind === 'stopped-safely') stops[i]++;
      else if (e.kind === 'completed' && e.stage === 'merge') delivered[i]++;
    }
    const hours = width / 3600e3;
    sparkLines($('s-latency'), lat.map((v) => quantile(v, 0.5)), lat.map((v) => quantile(v, 0.9)), width, (v) => fmtDuration(v), t('spark.latency', { w: fmtDuration(width) }));
    sparkBars($('s-stops'), stops.map((v) => v / hours), width, (v) => t('unit.stops', { v: v.toFixed(1) }), 'var(--stopped)', t('spark.stops', { w: fmtDuration(width) }));
    sparkBars($('s-delivered'), delivered.map((v) => v / hours), width, (v) => t('unit.merges', { v: v.toFixed(1) }), 'var(--completed)', t('spark.delivered', { w: fmtDuration(width) }));
    trendTable(width, lat, stops, delivered, hours);
  }
  // The same buckets as a table, for keyboard and screen-reader users.
  function trendTable(width, lat, stops, delivered, hours) {
    const details = $('trend-table');
    if (!details.open) return;
    const body = details.querySelector('tbody');
    body.textContent = '';
    for (let i = 0; i < lat.length; i++) {
      const start = state.view[0] + i * width, tr = html('tr', null, null, body);
      html('th', null, fmtClock(start) + '–' + fmtClock(start + width), tr).scope = 'row';
      html('td', null, lat[i].length ? fmtDuration(quantile(lat[i], 0.5)) + ' / ' + fmtDuration(quantile(lat[i], 0.9)) : '–', tr);
      html('td', null, (stops[i] / hours).toFixed(1), tr);
      html('td', null, (delivered[i] / hours).toFixed(1), tr);
    }
  }
  function sparkFrame(el, label) {
    el.textContent = '';
    const w = el.clientWidth || 92, h = 30;
    el.setAttribute('viewBox', '0 0 ' + w + ' ' + h);
    el.setAttribute('aria-label', label);
    svg('title', {}, el).textContent = label;
    return { w, h };
  }
  function sparkLines(el, p50, p90, width, fmt, label) {
    const { w, h } = sparkFrame(el, label);
    const max = Math.max(1, ...p90.filter((v) => v !== null));
    const path = (values) => values.map((v, i) => (v === null ? null : [(i + 0.5) / values.length * w, h - 2 - v / max * (h - 4)]))
      .reduce((d, p, i, a) => (p ? d + (i && a[i - 1] ? 'L' : 'M') + p[0].toFixed(1) + ',' + p[1].toFixed(1) : d), '');
    svg('path', { d: path(p90), class: 'p90', fill: 'none', 'stroke-width': 1.2 }, el);
    svg('path', { d: path(p50), fill: 'none', stroke: 'var(--running)', 'stroke-width': 2, 'stroke-linejoin': 'round' }, el);
    hoverBuckets(el, w, h, p50.length, (i) => p50[i] === null ? t('spark.nocalls') : 'p50 ' + fmt(p50[i]) + ' · p90 ' + fmt(p90[i]), width);
  }
  function sparkBars(el, values, width, fmt, color, label) {
    const { w, h } = sparkFrame(el, label);
    const max = Math.max(1e-9, ...values), bw = w / values.length;
    svg('line', { x1: 0, x2: w, y1: h - 0.5, y2: h - 0.5, stroke: 'var(--line)' }, el);
    values.forEach((v, i) => { if (v > 0) svg('rect', { x: i * bw + 0.5, y: h - 1 - v / max * (h - 3), width: Math.max(bw - 1, 1), height: v / max * (h - 3), fill: color, rx: 1 }, el); });
    hoverBuckets(el, w, h, values.length, (i) => fmt(values[i]), width);
  }
  function hoverBuckets(el, w, h, n, describe, width) {
    const bw = w / n;
    for (let i = 0; i < n; i++) {
      const r = svg('rect', { x: i * bw, y: 0, width: bw, height: h, fill: 'transparent' }, el);
      const start = state.view[0] + i * width;
      r.addEventListener('mousemove', (ev) => showTooltip(ev, { title: fmtClock(start) + '–' + fmtClock(start + width), rows: [['', describe(i)]] }));
      r.addEventListener('mouseleave', hideTooltip);
    }
  }

  // Degradation periods: windows with a burst of stops, degradations,
  // retries and quarantines, shaded across the lanes.
  function bands(all) {
    const [t0, t1] = state.view, n = 60, width = (t1 - t0) / n;
    const counts = new Array(n).fill(0);
    for (const l of all) for (const e of l.events) {
      if (e.t < t0 || e.t >= t1) continue;
      if (e.postDelivery) continue;
      if (e.kind === 'failed' || e.kind === 'stopped-safely' || e.kind === 'degraded' || e.kind === 'retried' || e.kind === 'quarantined' || e.kind === 'blocked') counts[Math.floor((e.t - t0) / width)]++;
    }
    // A burst window is ~15 minutes (at least one bucket).
    const span = Math.max(1, Math.round(15 * 60e3 / width));
    // Mark the problem buckets of every dense window, then join marked
    // buckets closer than the window: a band spans first to last problem.
    // The threshold is relative to the window's own background rate, so a
    // busy installation does not shade everything: at least three problems
    // and at least twice the average for a window of that length.
    const total = counts.reduce((a, b) => a + b, 0);
    const threshold = Math.max(3, 2 * total * span / n);
    const marked = new Array(n).fill(false);
    for (let i = 0; i < n; i++) {
      let sum = 0;
      for (let j = i; j < Math.min(n, i + span); j++) sum += counts[j];
      if (sum >= threshold) for (let j = i; j < Math.min(n, i + span); j++) if (counts[j]) marked[j] = true;
    }
    const out = [];
    for (let i = 0; i < n; i++) {
      if (!marked[i]) continue;
      const last = out[out.length - 1], a = t0 + i * width, b = a + width;
      if (last && i - last.end <= span) { last.b = b; last.end = i; last.count += counts[i]; } else out.push({ a, b, end: i, count: counts[i] });
    }
    return out;
  }

  // ---------- funnel ----------
  const FUNNEL = [
    { key: 'queued', name: 'funnel.queued', note: 'funnel.queued.note', swatch: 'outline' },
    { key: 'execution', stage: 'execution' },
    { key: 'verification', stage: 'verification' },
    { key: 'review', stage: 'review' },
    { key: 'publication', stage: 'publication' },
    { key: 'merge', stage: 'merge' },
    { key: 'blocked', name: 'funnel.blocked', note: 'funnel.blocked.note', swatch: 'hatch' },
  ];
  function renderFunnel(all) {
    const box = $('funnel');
    if (!box.childElementCount) {
      html('div', 'funnel-title', t('funnel.title'), box);
      for (const f of FUNNEL) {
        const b = html('button', null, null, box);
        b.type = 'button';
        b.dataset.current = f.key;
        b.setAttribute('aria-pressed', 'false');
        const sw = html('span', 'stage-swatch' + (f.swatch ? ' ' + f.swatch : ''), f.stage ? STAGE_INITIAL[f.stage] : '', b);
        if (f.stage) sw.style.background = STAGE_COLOR[f.stage];
        sw.setAttribute('aria-hidden', 'true');
        html('span', 'count', '0', b);
        const label = f.name ? t(f.name) : stageVerb(f.stage);
        const name = html('span', 'name', label.charAt(0).toUpperCase() + label.slice(1), b);
        if (f.note) html('small', null, t(f.note), name);
      }
    }
    for (const b of box.querySelectorAll('button')) {
      const n = all.filter((l) => l.current === b.dataset.current).length;
      b.querySelector('.count').textContent = String(n);
      b.classList.toggle('zero', n === 0);
      b.setAttribute('aria-pressed', String(state.filter.current === b.dataset.current));
      b.setAttribute('aria-label', t('funnel.label', { name: b.querySelector('.name').firstChild.textContent, n }) + (state.filter.current === b.dataset.current ? t('funnel.filtering') : ''));
    }
  }

  // ---------- live feed ----------
  const PROBLEM_KINDS = { failed: true, 'stopped-safely': true, degraded: true, quarantined: true, blocked: true, retried: true };
  let feedShown = 0;
  function feedMatches(e, q, kind) {
    if (kind === 'problems' && (e.postDelivery || (!PROBLEM_KINDS[e.kind] && e.diagnostic !== 'stale-wait'))) return false;
    if (kind === 'lifecycle' && e.kind === 'progressed') return false;
    if (!q) return true;
    const hay = ('#' + (e.issue || '') + ' ' + (e.repository || '') + ' ' + e.component + ' ' + stageName(e.stage) + ' ' + e.stage + ' ' + e.kind + ' ' + (e.step || '') + ' ' + (e.diagnostic || '')).toLowerCase();
    return hay.indexOf(q) >= 0;
  }
  function renderFeed(force) {
    if (!state.feedOpen) return;
    // Auto-refresh only while following now; otherwise offer the new events.
    // Arrivals are counted, so pruning old events never hides the offer.
    if (!force && !state.follow && feedShown) {
      const pending = arrivals - feedShown;
      const nb = $('feed-new');
      nb.hidden = pending <= 0;
      nb.textContent = t('feed.new', { n: pending });
      return;
    }
    feedShown = arrivals;
    $('feed-new').hidden = true;
    const q = $('feed-filter').value.trim().toLowerCase(), kind = $('feed-kind').value;
    const list = $('feed-list');
    const sorted = Array.from(state.events.values()).filter((e) => feedMatches(e, q, kind)).sort((a, b) => b.t - a.t);
    list.textContent = '';
    if (!$('feed-every').checked) return renderFeedGrouped(list, sorted);
    for (const e of sorted.slice(0, 300)) {
      const li = html('li', !e.postDelivery && (PROBLEM_KINDS[e.kind] || e.diagnostic) ? 'problem' : '', null, list);
      li.tabIndex = 0;
      li.dataset.key = laneKey(e);
      li.dataset.id = e.id;
      li.style.borderLeftColor = STAGE_COLOR[e.stage];
      const time = html('span', 'time', fmtClock(e.t, true), li);
      time.title = fmtUTC(e.t);
      html('span', 'what', (e.issue ? '#' + e.issue + ' ' : '') + stageName(e.stage) + (e.step ? ' · ' + e.step : '') + ' — ' + kindLabel(e.kind) + (e.diagnostic ? ' (' + e.diagnostic + ')' : ''), li);
      html('span', 'detail', [e.component, e.attempt ? t('attempt', { n: e.attempt }) : '', e.durationMs ? fmtDuration(e.durationMs) : '', e.latencyMs ? t('panel.lat', { d: fmtDuration(e.latencyMs) }) : ''].filter(Boolean).join(' · '), li);
    }
    if (state.follow) list.scrollTop = 0;
  }
  // Default feed: model calls and effect steps of one stage of one attempt
  // fold into one row at their newest time; transitions and problems keep
  // their own rows, problems with more weight.
  function renderFeedGrouped(list, sorted) {
    const rows = [], groups = new Map();
    for (const e of sorted) {
      if (e.kind === 'progressed') {
        const key = laneKey(e) + '|' + (e.attempt || 0) + '|' + e.stage;
        let g = groups.get(key);
        if (!g) { g = { group: true, e, calls: [], steps: 0 }; groups.set(key, g); rows.push(g); }
        if (e.step === 'model-call') g.calls.push(e); else g.steps++;
      } else rows.push({ e });
      if (rows.length >= 300) break;
    }
    for (const r of rows) {
      const e = r.e, problem = !e.postDelivery && (PROBLEM_KINDS[e.kind] || e.diagnostic === 'stale-wait');
      const li = html('li', r.group ? 'progress' : problem ? 'problem' : 'transition', null, list);
      li.tabIndex = 0;
      li.dataset.key = laneKey(e);
      li.dataset.id = e.id;
      li.style.borderLeftColor = problem ? (e.kind === 'failed' ? 'var(--failed)' : 'var(--degraded)') : STAGE_COLOR[e.stage];
      html('span', 'time', fmtClock(e.t, true), li).title = fmtUTC(e.t);
      const who = (e.issue ? '#' + e.issue + ' ' : '') + stageName(e.stage);
      if (r.group) {
        const lat = r.calls.map((c) => c.latencyMs).filter(Boolean);
        const parts = [who];
        if (r.calls.length) parts.push((r.calls.length > 1 ? t('feed.calls', { n: r.calls.length }) : t('feed.call')) + (lat.length ? ' · p50 ' + fmtDuration(median(lat)) : ''));
        if (r.steps) parts.push(r.steps > 1 ? t('feed.steps', { n: r.steps }) : t('feed.step'));
        html('span', 'what', parts.join(' · '), li);
        html('span', 'detail', [t('feed.last', { t: fmtClock(e.t, true) }), e.component, e.attempt ? t('attempt', { n: e.attempt }) : ''].filter(Boolean).join(' · '), li);
      } else {
        html('span', 'what', who + (e.step ? ' · ' + e.step : '') + ' — ' + kindLabel(e.kind) + (e.diagnostic ? ' (' + e.diagnostic + ')' : ''), li);
        html('span', 'detail', [e.component, e.attempt ? t('attempt', { n: e.attempt }) : '', e.durationMs ? fmtDuration(e.durationMs) : ''].filter(Boolean).join(' · '), li);
      }
    }
    if (state.follow) list.scrollTop = 0;
  }
  function setFeed(open) {
    state.feedOpen = open;
    $('feed').hidden = !open;
    document.body.classList.toggle('feed-open', open);
    $('feed-toggle').setAttribute('aria-pressed', String(open));
    feedShown = 0;
    renderFeed(true);
    schedule();
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
  function renderAxis() {
    axis.textContent = '';
    const w = chartWidth();
    axis.setAttribute('viewBox', '0 0 ' + w + ' 34');
    axis.setAttribute('width', w);
    // Same pixel width as the chart, which loses the lane list's scrollbar.
    axis.style.flex = 'none';
    axis.style.width = w + 'px';
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
      svg('title', {}, g).textContent = fmtUTC(t);
      svg('line', { x1: px, x2: px, y1: 24, y2: 34 }, g);
      text(g, { x: px + 4, y: 20 }, major || ticks.length === 1 ? day.slice(5) + ' ' + fmtClock(t) : fmtClock(t));
    }
    return ticks;
  }

  // ---------- chart ----------
  function defs(root) {
    const d = svg('defs', {}, root);
    for (const stage of STAGES) {
      const p = svg('pattern', { id: 'hatch-' + stage, width: 6, height: 6, patternUnits: 'userSpaceOnUse', patternTransform: 'rotate(45)' }, d);
      svg('rect', { width: 6, height: 6, fill: STAGE_COLOR[stage], opacity: 0.28 }, p);
      svg('line', { x1: 0, y1: 0, x2: 0, y2: 6, stroke: STAGE_COLOR[stage], 'stroke-width': 3 }, p);
    }
    const run = svg('pattern', { id: 'stripes', width: 10, height: 10, patternUnits: 'userSpaceOnUse', patternTransform: 'rotate(45)' }, d);
    svg('rect', { width: 4, height: 10, fill: 'rgba(255,255,255,.28)', class: 'running-stripes' }, run);
  }

  // Outcome badge at a bar's end: a light disc with the outcome's own glyph.
  function badge(g, outcome, cx, cy) {
    const ink = { stroke: '#0e0e0d', 'stroke-width': 1.8, 'stroke-linecap': 'round', fill: 'none' };
    switch (outcome) {
      case 'failed':
        svg('circle', { cx, cy, r: 7, fill: '#f0efec', stroke: '#d03b3b', 'stroke-width': 2 }, g);
        svg('path', Object.assign({ d: `M${cx - 3},${cy - 3} l6,6 M${cx + 3},${cy - 3} l-6,6` }, ink), g);
        break;
      case 'stopped-safely':
        svg('circle', { cx, cy, r: 7, fill: '#f0efec', stroke: '#1a1a19', 'stroke-width': 1.5 }, g);
        svg('rect', { x: cx - 3, y: cy - 3, width: 6, height: 6, fill: '#0e0e0d', rx: 1 }, g);
        break;
      case 'blocked':
        svg('circle', { cx, cy, r: 7, fill: '#f0efec', stroke: '#1a1a19', 'stroke-width': 1.5 }, g);
        svg('path', Object.assign({ d: `M${cx - 1.8},${cy - 3} v6 M${cx + 1.8},${cy - 3} v6` }, ink), g);
        break;
      case 'quarantined':
        svg('path', { d: octagon(cx, cy, 7.5), fill: '#f0efec', stroke: '#1a1a19', 'stroke-width': 1.5 }, g);
        svg('path', Object.assign({ d: `M${cx - 3.2},${cy} h6.4` }, ink, { 'stroke-width': 2.2 }), g);
        break;
    }
  }
  function octagon(cx, cy, r) {
    const a = r * 0.414;
    return `M${cx - a},${cy - r} h${2 * a} l${r - a},${r - a} v${2 * a} l${a - r},${r - a} h${-2 * a} l${a - r},${a - r} v${-2 * a} z`;
  }
  function check(g, cx, cy) {
    svg('path', { d: `M${cx - 3.5},${cy} l2.5,2.8 l4.5,-5.5`, stroke: '#0e0e0d', 'stroke-width': 2, 'stroke-linecap': 'round', 'stroke-linejoin': 'round', fill: 'none' }, g);
  }

  // The issue closed after its merge: a neutral flag on the bar row.
  function closedTick(g, px, cy) {
    svg('line', { x1: px, x2: px, y1: cy - 8, y2: cy + 8, stroke: 'var(--text-muted)', 'stroke-width': 1.5 }, g);
    svg('path', { d: `M${px},${cy - 8} l7,2.5 l-7,2.5 z`, fill: 'var(--text-muted)' }, g);
  }
  function closedTooltip(lane, group) {
    const rows = [];
    for (const s of group.segments) for (const e of s.events) {
      rows.push([fmtClock(e.t, true), stageName(e.stage) + (e.step ? ' · ' + e.step : '') + ' — ' + kindLabel(e.kind) + (e.diagnostic ? ' (' + e.diagnostic + ')' : '')]);
    }
    rows.push(['', t('closed.note')]);
    return { title: (lane.issue ? '#' + lane.issue + ' · ' : '') + t('closed.title'), rows };
  }
  function marker(g, type, px, cy) {
    switch (type) {
      case 'degraded':
        svg('path', { d: `M${px},${cy - 6} l6.5,11 h-13 z`, fill: 'var(--degraded)', stroke: 'var(--surface-1)', 'stroke-width': 1.2 }, g);
        svg('path', { d: `M${px},${cy - 2.5} v3.2 M${px},${cy + 2.3} v0.6`, stroke: '#0e0e0d', 'stroke-width': 1.5, 'stroke-linecap': 'round' }, g);
        break;
      case 'retried':
        svg('circle', { cx: px, cy, r: 6.5, fill: '#f0efec', stroke: 'var(--text-secondary)', 'stroke-width': 1.2 }, g);
        svg('path', { d: `M${px + 3.3},${cy + 1.1} A3.5,3.5 0 1 1 ${px + 2.4},${cy - 2.6}`, stroke: '#0e0e0d', 'stroke-width': 1.5, fill: 'none', 'stroke-linecap': 'round' }, g);
        svg('path', { d: `M${px + 3.6},${cy - 4.6} L${px + 3.1},${cy - 1.9} L${px + 0.5},${cy - 2.5}`, stroke: '#0e0e0d', 'stroke-width': 1.3, fill: 'none', 'stroke-linejoin': 'round', 'stroke-linecap': 'round' }, g);
        break;
      case 'stale':
        svg('circle', { cx: px, cy, r: 6.2, fill: 'var(--surface-1)', stroke: 'var(--degraded)', 'stroke-width': 1.6 }, g);
        svg('path', { d: `M${px},${cy - 3.5} v3.5 l2.5,1.5`, stroke: 'var(--degraded)', 'stroke-width': 1.4, fill: 'none', 'stroke-linecap': 'round' }, g);
        break;
      case 'quarantined':
        svg('path', { d: octagon(px, cy, 6.8), fill: 'var(--quarantined)', stroke: 'var(--surface-1)', 'stroke-width': 1.2 }, g);
        svg('path', { d: `M${px - 3},${cy} h6`, stroke: '#0e0e0d', 'stroke-width': 2 }, g);
        break;
      case 'blocked':
        svg('rect', { x: px - 6, y: cy - 6, width: 12, height: 12, rx: 2.5, fill: 'var(--blocked)', stroke: 'var(--surface-1)', 'stroke-width': 1.2 }, g);
        svg('path', { d: `M${px - 2},${cy - 3} v6 M${px + 2},${cy - 3} v6`, stroke: '#0e0e0d', 'stroke-width': 1.8 }, g);
        break;
    }
  }

  // Markers closer than this many pixels merge into one counted badge.
  const CLUSTER_PX = 15;
  function clusters(markers) {
    const out = [];
    for (const m of markers) {
      const px = x(m.e.t), last = out[out.length - 1];
      if (last && px - last.px1 < CLUSTER_PX) { last.items.push(m); last.px1 = px; } else out.push({ px0: px, px1: px, items: [m] });
    }
    return out;
  }

  let chipEnd = -Infinity;
  function drawSegment(parent, lane, s, y0, w, dim, overlay) {
    const LANE_H = laneHeight(), cy = y0 + LANE_H - 13;
    const xq = x(s.queued), xs = s.start !== null ? x(s.start) : x(s.drawEnd), xe = x(s.drawEnd);
    if (xe < -20 || xq > w + 20) return null;
    const g = svg('g', { opacity: dim ? 0.28 : 1 }, parent);
    const color = STAGE_COLOR[s.stage];
    // Queue wait before the claim: a dashed lead-in in the stage's hue.
    if (xs - xq > 1 && s.outcome !== 'waiting') svg('line', { x1: xq, x2: xs, y1: cy, y2: cy, stroke: color, 'stroke-width': 2, 'stroke-dasharray': '3 3', opacity: 0.75 }, g);
    if (INSTANT[s.stage]) {
      svg('circle', { cx: xs, cy, r: 3.6, fill: color, stroke: 'var(--surface-1)', 'stroke-width': 1.2 }, g);
      return { g, x0: xs - 4, x1: xs + 4 };
    }
    // Very short runs keep their true width (at least 2 px) and get a chip
    // with the stage initial above them, so the axis is not stretched.
    const short = s.outcome !== 'waiting' && xe - xs < 7;
    const chip = short && STAGES.indexOf(s.stage) >= 3;
    let width = short ? Math.max(xe - xs, 2) : Math.max(xe - xs, 4), x0 = xs;
    if (s.outcome === 'waiting') {
      // Waiting: an outline only, from the queue time.
      x0 = xq; width = Math.max(xe - xq, 6);
      svg('rect', { x: x0 + 0.75, y: cy - BAR_H / 2 + 0.75, width: width - 1.5, height: BAR_H - 1.5, rx: 4, fill: 'none', stroke: color, 'stroke-width': 1.5, 'stroke-dasharray': '4 2' }, g);
    } else {
      const fill = s.outcome === 'blocked' || s.outcome === 'quarantined' ? 'url(#hatch-' + s.stage + ')' : color;
      svg('rect', { x: x0, y: cy - BAR_H / 2, width, height: BAR_H, rx: 4, fill, stroke: 'var(--surface-1)', 'stroke-width': 1 }, g);
      if (s.outcome === 'running') {
        svg('rect', { x: x0, y: cy - BAR_H / 2, width, height: BAR_H, rx: 4, fill: 'url(#stripes)' }, g);
        svg('path', { d: `M${x0 + width - 1},${cy - BAR_H / 2} l6,${BAR_H / 2} l-6,${BAR_H / 2} z`, fill: color }, g);
      }
      if (s.outcome === 'failed' || s.outcome === 'stopped-safely') svg('rect', { x: x0 + 0.5, y: cy - BAR_H / 2 + 0.5, width: width - 1, height: BAR_H - 1, rx: 4, fill: 'none', stroke: '#f0efec', 'stroke-width': 1, opacity: 0.7 }, g);
      // Model calls: ticks along the bottom edge.
      if (width > 30) for (const c of s.calls) {
        const px = x(c.t);
        if (px > x0 + 2 && px < x0 + width - 2) svg('line', { x1: px, x2: px, y1: cy + BAR_H / 2 - 4, y2: cy + BAR_H / 2 - 1, stroke: 'rgba(14,14,13,.6)', 'stroke-width': 1 }, g);
      }
    }
    if (chip) {
      // Neighbouring chips line up to the right; a hairline ties each to its run.
      const tx = x0 + width / 2, cx = Math.max(tx, chipEnd + 11);
      svg('rect', { x: cx - 5, y: cy - BAR_H / 2 - 12, width: 10, height: 10, rx: 2.5, fill: color }, g);
      text(g, { class: 'chip-label', x: cx, y: cy - BAR_H / 2 - 4, 'text-anchor': 'middle' }, STAGE_INITIAL[s.stage]);
      if (cx !== tx) svg('line', { x1: cx - 5, x2: tx, y1: cy - BAR_H / 2 - 2, y2: cy - BAR_H / 2, stroke: color, 'stroke-width': 1 }, g);
      chipEnd = cx;
    }
    const badged = s.outcome === 'failed' || s.outcome === 'stopped-safely' || s.outcome === 'blocked' || s.outcome === 'quarantined';
    const room = width - (badged ? 10 : s.outcome === 'completed' ? 14 : 4) - 6;
    const name = stageName(s.stage) + (s.attempt > 1 && s.stage === 'execution' ? ' #' + s.attempt : '');
    const label = room >= name.length * 6.3 ? name : room >= 9 ? STAGE_INITIAL[s.stage] : '';
    if (label) text(g, { class: 'bar-label' + (s.outcome === 'waiting' || s.outcome === 'blocked' || s.outcome === 'quarantined' ? ' light' : ''), x: x0 + 5, y: cy + 4 }, label);
    if (s.outcome === 'completed' && width > 22) check(g, x0 + width - 8, cy);
    // Badges go on a layer above every bar, so a retry that starts right
    // after a stop cannot cover the stop's glyph.
    if (badged) badge(svg('g', { opacity: dim ? 0.28 : 1 }, overlay || g), s.outcome, x0 + width, cy);
    return { g, x0: Math.min(xq, x0), x1: x0 + width + (badged ? 8 : 6) };
  }

  // Re-rendering recreates rows; remember which lane, panel event or feed
  // event had keyboard focus and put it back on the new node.
  function focusedKey() {
    const a = document.activeElement;
    if (!a || !a.closest) return null;
    if (a.closest('#labels') && a.dataset.key) return { box: '#labels', attr: 'key', value: a.dataset.key };
    if (a.closest('#panel-events') && a.dataset.id) return { box: '#panel-events', attr: 'id', value: a.dataset.id };
    if (a.closest('#feed-list') && a.dataset.id) return { box: '#feed-list', attr: 'id', value: a.dataset.id };
    return null;
  }
  function restoreFocus(f) {
    if (!f || (document.activeElement && document.activeElement !== document.body && focusedKey())) return;
    const box = document.querySelector(f.box);
    const node = box && Array.from(box.querySelectorAll('[data-' + f.attr + ']')).find((n) => n.dataset[f.attr] === f.value);
    if (node) { node.tabIndex = 0; node.focus({ preventScroll: true }); }
  }

  let ticksCache = [];
  function render() {
    const keep = focusedKey();
    try { draw(); } finally { restoreFocus(keep); }
  }
  function draw() {
    const t0perf = performance.now();
    const now = Date.now();
    for (const lane of state.lanes.values()) if (lane.dirty || lane.segments === undefined || lane.hasOpen) buildLane(lane, now);
    const all = Array.from(state.lanes.values());
    renderTiles(all);
    renderFunnel(all);
    const matching = visibleLanes();
    // One repository for every lane: name it once in the header instead.
    const repos = new Set(all.map((l) => l.repository));
    state.singleRepo = repos.size === 1 ? Array.from(repos)[0] : '';
    const extent = dataExtent(matching.length ? matching : all, now);
    // The initial window is chosen once the first events exist.
    if (!state.view || (!state.viewChosen && all.length)) {
      const hours = Number(params.get('range'));
      // The same 5-minute minimum as the zoom; anything else uses the extent.
      state.view = hours >= 5 / 60 && hours <= 192 ? [now - hours * 3600e3 * 0.95, now + hours * 3600e3 * 0.05] : extent;
      state.viewChosen = all.length > 0;
    }
    if (state.follow) {
      const span = state.view[1] - state.view[0];
      if (now > state.view[1] - span * 0.02) state.view = [now + span * 0.04 - span, now + span * 0.04];
    }
    // Only lanes with activity inside the visible window.
    const lanes = matching.filter((l) => l.last >= state.view[0] && l.first <= state.view[1]);
    $('lane-count').textContent = '(' + lanes.length + (lanes.length !== all.length ? ' ' + t('of', { n: all.length }) : '') + ')';
    $('empty').hidden = lanes.length > 0 || all.length === 0;
    $('empty').textContent = matching.length ? t('empty.window') : t('empty.filters');
    const LANE_H = laneHeight();
    const w = chartWidth(), height = Math.max(lanes.length * LANE_H, 1);
    chart.textContent = '';
    chart.setAttribute('viewBox', '0 0 ' + w + ' ' + height);
    chart.setAttribute('width', w);
    chart.setAttribute('height', height);
    chart.style.height = height + 'px';
    defs(chart);
    ticksCache = renderAxis();
    const grid = svg('g', {}, chart);
    // Only the lanes inside the scrolled viewport (plus a margin) are drawn,
    // so hundreds of work items cost no more than one screenful.
    const first = state.printing ? 0 : Math.max(0, Math.floor(lanesBox.scrollTop / LANE_H) - 6);
    const last = state.printing ? lanes.length : Math.min(lanes.length, Math.ceil((lanesBox.scrollTop + (lanesBox.clientHeight || 800)) / LANE_H) + 6);
    state.visible = lanes;
    for (let i = first; i < last; i++) if (i % 2) svg('rect', { class: 'lane-bg alt', x: 0, y: i * LANE_H, width: w, height: LANE_H }, grid);
    for (const tk of ticksCache) svg('line', { x1: tk.px, x2: tk.px, y1: 0, y2: height, stroke: tk.major ? 'var(--grid-major)' : 'var(--grid)', 'stroke-width': 1 }, grid);
    for (let i = first; i < last; i++) svg('line', { class: 'lane-sep', x1: 0, x2: w, y1: (i + 1) * LANE_H - 0.5, y2: (i + 1) * LANE_H - 0.5 }, grid);
    for (const b of bands(all)) {
      const xa = Math.max(0, x(b.a)), xb = Math.min(w, x(b.b));
      if (xb <= xa) continue;
      const r = svg('rect', { class: 'band', x: xa, y: 0, width: xb - xa, height }, grid);
      svg('title', {}, r).textContent = t('band.title', { from: fmtClock(b.a), to: fmtClock(b.b), n: b.count });
      if (xb - xa > 60) text(axis, { class: 'band-label', x: xa + 3, y: 9 }, t('band.label', { n: b.count }));
    }
    trends(all);
    renderProblems(all, now);
    $('header-repo').textContent = state.singleRepo;
    $('header-repo').hidden = !state.singleRepo;

    // Labels column: issue, repository, status, and a summary line.
    labels.textContent = '';
    labels.style.height = height + 'px';
    labels.style.paddingTop = first * LANE_H + 'px';
    labels.setAttribute('aria-rowcount', String(lanes.length));
    const focused = state.focusIndex >= 0 ? Math.min(state.focusIndex, lanes.length - 1) : -1;
    lanes.slice(first, last).forEach((lane, k) => {
      const index = first + k;
      const row = html('div', 'label' + (state.selected === lane.key ? ' active' : ''), null, labels);
      row.setAttribute('role', 'listitem');
      row.setAttribute('aria-posinset', String(index + 1));
      row.setAttribute('aria-setsize', String(lanes.length));
      // Roving focus: one lane is in the tab order; arrows move between lanes.
      row.tabIndex = index === (focused >= first && focused < last ? focused : first) ? 0 : -1;
      row.dataset.key = lane.key;
      row.dataset.index = String(index);
      const top = html('div', 'label-top', null, row);
      // The issue number links to the forge; the row itself opens the details.
      const num = lane.issue ? issueLink(top, lane.repository, lane.issue, '#' + lane.issue, 'issue') : html('span', 'issue', lane.workItem.slice(0, 8), top);
      num.tabIndex = -1;
      // The repository is shown per lane only when lanes span several.
      const repo = html('span', 'repo', state.singleRepo ? '' : (lane.repository || '').split('/').pop(), top);
      repo.title = lane.repository;
      if (lane.attempts > 1) html('span', 'attempts', '×' + lane.attempts, top).title = t('attempts.title', { n: lane.attempts });
      const chipText = t('chip.' + lane.status);
      html('span', 'chip ' + lane.status, chipText, top);
      const summary = laneSummary(lane, now);
      const line = html('div', 'label-sub ' + summary.cls, summary.text, row);
      line.title = summary.text;
      row.setAttribute('aria-label', t('lane.aria', { who: lane.issue ? t('lane.issue', { n: lane.issue }) : t('lane.workitem'), chip: chipText, summary: summary.text }));
    });

    const bars = svg('g', {}, chart);
    const overlay = svg('g', {}, chart);
    lanes.slice(first, last).forEach((lane, k) => {
      const i = first + k, y0 = i * LANE_H;
      const dim = state.filter.component;
      let lastEnd = -Infinity, finalBox = null;
      chipEnd = -Infinity;
      // Attempt dividers: a dashed rule where attempt 2, 3, ... begins.
      for (let a = 2; a <= lane.attempts; a++) {
        const starts = lane.segments.filter((s) => s.attempt === a).map((s) => s.queued);
        if (!starts.length) continue;
        const px = x(Math.min.apply(null, starts)) - 4;
        if (px < 0 || px > w) continue;
        svg('line', { class: 'attempt-divider', x1: px, x2: px, y1: y0 + 17, y2: y0 + LANE_H - 2 }, bars);
        text(bars, { class: 'attempt-label', x: px - 3, y: y0 + 24, 'text-anchor': 'end' }, '#' + a);
      }
      for (const group of lane.closed) {
        const px = x(group.at), cy = y0 + LANE_H - 13;
        if (px > -10 && px < w + 10) {
          const g = svg('g', { class: 'closed-tick' }, bars);
          closedTick(g, px, cy);
          const hit = svg('rect', { class: 'hit', x: px - 8, y: y0 + 16, width: 16, height: LANE_H - 18 }, g);
          hit.addEventListener('mousemove', (ev) => showTooltip(ev, closedTooltip(lane, group)));
          hit.addEventListener('mouseleave', hideTooltip);
          hit.addEventListener('click', () => openPanel(lane.key));
        }
      }
      for (const s of lane.segments) {
        if (s.postDelivery) continue;
        const box = drawSegment(bars, lane, s, y0, w, dim && s.events[0].component !== dim, overlay);
        if (!box) continue;
        lastEnd = Math.max(lastEnd, box.x1);
        if (s === lane.final) finalBox = box;
        const hit = svg('rect', { class: 'hit', x: box.x0 - 3, y: y0 + 16, width: Math.max(box.x1 - box.x0, 6) + 6, height: LANE_H - 18 }, box.g);
        hit.addEventListener('mousemove', (ev) => showTooltip(ev, segmentTooltip(lane, s)));
        hit.addEventListener('mouseleave', hideTooltip);
        hit.addEventListener('click', () => openPanel(lane.key));
      }
      // The diagnostic of a failed or blocked lane's final stop is always shown,
      // after its last bar (the summary line in the label column repeats it).
      if (finalBox && lane.final.diagnostic) {
        const cls = 'diag' + (lane.status === 'blocked' ? ' blocked' : ''), tx = Math.max(finalBox.x1, lastEnd) + 6;
        const width = lane.final.diagnostic.length * 6.9;
        // Near the right edge the text goes before the lane's first bar instead.
        if (tx + width < w - 4) text(bars, { class: cls, x: tx, y: y0 + LANE_H - 9 }, lane.final.diagnostic);
        else {
          const first = Math.min.apply(null, lane.segments.map((o) => x(o.queued)));
          if (first - width - 8 > 0) text(bars, { class: cls, x: first - 8, y: y0 + LANE_H - 9, 'text-anchor': 'end' }, lane.final.diagnostic);
        }
      }
      // Marker row above the bars; overlapping markers cluster.
      const my = y0 + 9;
      for (const c of clusters(lane.markers)) {
        const px = (c.px0 + c.px1) / 2;
        if (px < -10 || px > w + 10) continue;
        const g = svg('g', { class: 'marker' }, bars);
        const top = c.items.reduce((a, m) => (MARKER_PRIORITY[m.type] > MARKER_PRIORITY[a.type] ? m : a), c.items[0]);
        marker(g, top.type, px, my);
        if (c.items.length > 1) {
          svg('rect', { x: px + 5, y: my - 8, width: 13, height: 11, rx: 5.5, fill: '#f0efec', stroke: 'var(--surface-1)', 'stroke-width': 1 }, g);
          text(g, { class: 'count', x: px + 11.5, y: my + 0.6, 'text-anchor': 'middle' }, c.items.length > 9 ? '9+' : String(c.items.length));
        }
        const hit = svg('rect', { class: 'hit', x: c.px0 - 9, y: my - 9, width: c.px1 - c.px0 + 18 + (c.items.length > 1 ? 12 : 0), height: 18 }, g);
        hit.addEventListener('mousemove', (ev) => showTooltip(ev, markerTooltip(lane, c.items)));
        hit.addEventListener('mouseleave', hideTooltip);
        hit.addEventListener('click', () => openPanel(lane.key));
      }
    });

    // Now line.
    const nx = x(now);
    if (nx >= 0 && nx <= w) {
      svg('line', { class: 'now-line', x1: nx, x2: nx, y1: 0, y2: height }, chart);
      const g = svg('g', { class: 'tick' }, axis);
      svg('title', {}, g).textContent = t('now') + ' · ' + fmtUTC(now);
      const pill = t('now.pill').length * 6.6 + 8;
      svg('rect', { class: 'now-pill', x: nx - pill / 2, y: 22, width: pill, height: 12, rx: 3 }, g);
      text(g, { class: 'now-label', x: nx, y: 31, 'text-anchor': 'middle' }, t('now.pill'));
    }
    if (state.selected) renderPanel();
    renderFeed(false);
    // Render timing, readable from the DOM for load measurements.
    const took = Math.round(performance.now() - t0perf), b = document.body.dataset;
    b.renderMs = String(took);
    b.renderMax = String(Math.max(took, Number(b.renderMax || 0)));
    b.renders = String(Number(b.renders || 0) + 1);
    b.events = String(state.events.size);
    b.lanes = String(state.lanes.size);
    if (params.get('perf') === '1') {
      $('perf').hidden = false;
      $('perf').textContent = 'perf · ' + state.events.size + ' events · ' + state.lanes.size + ' lanes · history loaded in ' + (b.readyMs || '–') + ' ms · last render ' + took + ' ms · max ' + b.renderMax + ' ms · ' + b.renders + ' renders';
    }
  }

  // ---------- tooltip ----------
  function segmentTooltip(lane, s) {
    const rows = [];
    rows.push([t('tt.stage'), stageName(s.stage) + ' (' + s.events[0].component + ')']);
    rows.push([t('tt.outcome'), outcomeText(s.outcome)]);
    if (s.diagnostic) rows.push([t('tt.diagnostic'), s.diagnostic]);
    if (s.attempt) rows.push([t('tt.attempt'), String(s.attempt)]);
    if (s.start !== null && s.queued < s.start) rows.push([t('tt.waited'), fmtDuration(s.start - s.queued)]);
    if (s.start !== null) rows.push([t('tt.started'), fmtClock(s.start, true) + ' · ' + fmtUTC(s.start)]);
    if (s.end !== null) rows.push([t('tt.ended'), fmtClock(s.end, true) + ' · ' + fmtUTC(s.end)]);
    if (s.start !== null && !INSTANT[s.stage]) rows.push([t('tt.duration'), fmtDuration((s.end !== null ? s.end : Date.now()) - s.start) + (s.end === null ? t('tt.sofar') : '')]);
    if (s.calls.length) {
      const lat = s.calls.map((c) => c.latencyMs).filter(Boolean);
      rows.push([t('tt.calls'), s.calls.length + (lat.length ? ' · p50 ' + fmtDuration(median(lat)) + ' · ' + t('tt.max') + ' ' + fmtDuration(Math.max.apply(null, lat)) : '')]);
    }
    if (s.effects) rows.push([t('tt.effects'), String(s.effects)]);
    if (s.degraded.length) rows.push([t('tt.degraded'), s.degraded.map((d) => d.diagnostic).join(', ')]);
    if (s.stale) rows.push([t('tt.stale'), t('tt.queuedover', { d: fmtDuration(Number(s.stale.attributes && s.stale.attributes.waitedMs) || 0) })]);
    const attrs = s.terminal && s.terminal.attributes;
    if (attrs) for (const k of ['phase', 'exitCode', 'changes', 'reason']) if (attrs[k]) rows.push([k, attrs[k]]);
    return { title: (lane.issue ? '#' + lane.issue + ' · ' : '') + stageName(s.stage), rows };
  }
  function markerTooltip(lane, items) {
    const rows = [];
    for (const m of items) {
      const e = m.e;
      rows.push([fmtClock(e.t, true), markerName(m.type) + ' · ' + stageName(e.stage) + (e.diagnostic ? ' · ' + e.diagnostic : '') + (e.attempt ? ' · ' + t('attempt', { n: e.attempt }) : '')]);
    }
    rows.push(['UTC', fmtUTC(items[0].e.t)]);
    return { title: (lane.issue ? '#' + lane.issue + ' · ' : '') + (items.length > 1 ? t('tt.markers', { n: items.length }) : markerName(items[0].type)), rows };
  }
  function showTooltip(ev, content) {
    tooltip.textContent = '';
    html('div', 'tt-title', content.title, tooltip);
    const dl = html('dl', null, null, tooltip);
    for (const [k, v] of content.rows) { html('dt', null, k, dl); html('dd', null, v, dl); }
    tooltip.hidden = false;
    const gap = 14, r = tooltip.getBoundingClientRect();
    let left = ev.clientX + gap, top = ev.clientY + gap;
    if (left + r.width > window.innerWidth - 8) left = ev.clientX - r.width - gap;
    if (top + r.height > window.innerHeight - 8) top = ev.clientY - r.height - gap;
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
    const title = $('panel-title');
    title.textContent = '';
    if (lane.issue) issueLink(title, lane.repository, lane.issue, t('panel.title', { n: lane.issue }), 'issue-link');
    else title.textContent = t('panel.workitem');
    renderBreakdown(lane);
    const meta = $('panel-meta');
    meta.textContent = '';
    const add = (k, v) => { html('dt', null, k, meta); html('dd', null, v, meta); };
    if (lane.repository) add(t('panel.repo'), lane.repository);
    add(t('panel.status'), t('status.' + lane.status));
    add(t('panel.now'), laneSummary(lane, Date.now()).text);
    add(t('panel.attempts'), String(Math.max(lane.attempts, 1)));
    if (lane.delivery !== null) add(t('panel.ttd'), fmtDuration(lane.delivery));
    add(t('panel.events'), String(lane.events.length));
    const list = $('panel-events');
    list.textContent = '';
    let group = '';
    const events = lane.events;
    for (let i = 0; i < events.length; i++) {
      const e = events[i];
      const g = e.attempt ? t('panel.attempt', { n: e.attempt }) : t('panel.pre');
      if (g !== group) { group = g; html('li', 'group', g, list); }
      // Consecutive successful model calls of one stage collapse into one row.
      let j = i;
      const isCall = (o) => o && o.kind === 'progressed' && o.step === 'model-call' && o.stage === e.stage && o.attempt === e.attempt;
      while (isCall(e) && isCall(events[j + 1])) j++;
      const li = html('li', null, null, list);
      li.tabIndex = list.querySelector('li[tabindex="0"]') ? -1 : 0;
      li.dataset.id = e.id;
      const time = html('span', 'time', fmtClock(e.t, true), li);
      time.title = fmtUTC(e.t);
      const sw = html('span', 'glyph', null, li);
      sw.style.background = KIND_COLOR[e.kind] || 'var(--waiting)';
      sw.setAttribute('aria-hidden', 'true');
      const details = [];
      if (j > i) {
        const calls = events.slice(i, j + 1), lat = calls.map((c) => c.latencyMs).filter(Boolean);
        html('span', 'what', t('panel.calls', { stage: stageName(e.stage), n: calls.length, kind: kindLabel('progressed') }), li);
        details.push(t('panel.until', { t: fmtClock(events[j].t, true) }));
        if (lat.length) details.push(t('panel.latency', { p50: fmtDuration(median(lat)), max: fmtDuration(Math.max.apply(null, lat)) }));
        i = j;
      } else {
        html('span', 'what', stageName(e.stage) + (e.step ? ' · ' + e.step : '') + ' — ' + kindLabel(e.kind) + (e.diagnostic ? ' (' + e.diagnostic + ')' : ''), li);
        if (e.durationMs) details.push(t('panel.duration', { d: fmtDuration(e.durationMs) }));
        if (e.latencyMs) details.push(t('panel.lat', { d: fmtDuration(e.latencyMs) }));
        for (const k in e.attributes || {}) details.push(fmtAttribute(k, e.attributes[k]));
      }
      details.push(e.component);
      html('span', 'detail', details.join(' · '), li);
    }
  }

  // ---------- per-stage breakdown ----------
  // The latest attempt as one bar: each stage's queue wait (hatched) and run
  // (solid), in order, proportional to time, so a bottleneck shows at once.
  function renderBreakdown(lane) {
    const box = $('breakdown');
    box.textContent = '';
    if (!lane.segments) return; // not built yet; the next render fills it
    const attempt = lane.attempts || 0;
    const segs = lane.segments.filter((s) => !INSTANT[s.stage] && s.stage !== 'admission' && (s.attempt || 0) === attempt && s.start !== null)
      .sort((a, b) => a.queued - b.queued);
    if (!segs.length) return;
    const parts = [];
    let prevEnd = null;
    for (const s of segs) {
      const queued = prevEnd !== null && prevEnd < s.queued ? prevEnd : s.queued;
      const wait = Math.max(0, s.start - queued), run = Math.max(0, s.drawEnd - s.start);
      if (wait > 0) parts.push({ stage: s.stage, wait: true, ms: wait });
      parts.push({ stage: s.stage, wait: false, ms: run, open: s.open });
      prevEnd = Math.max(prevEnd || 0, s.drawEnd);
    }
    const total = parts.reduce((n, p) => n + p.ms, 0);
    if (total <= 0) return;
    const head = html('div', 'bd-head', null, box);
    html('span', 'bd-title', t('bd.title'), head);
    html('span', 'bd-sub', (attempt ? t('bd.attempt', { n: attempt }) + ' · ' : '') + fmtDuration(total), head);
    const bar = html('div', 'bd-bar', null, box);
    const label = (p) => (p.wait ? t('bd.waitfor', { stage: stageName(p.stage) }) : stageName(p.stage)) + ' ' + fmtDuration(p.ms);
    bar.setAttribute('role', 'img');
    bar.setAttribute('aria-label', t('bd.aria', { list: parts.map(label).join(', ') }));
    for (const p of parts) {
      const seg = html('span', 'bd-seg' + (p.wait ? ' wait' : '') + (p.open ? ' open' : ''), null, bar);
      seg.style.flexGrow = String(p.ms);
      seg.style.setProperty('--seg', STAGE_COLOR[p.stage]);
      seg.title = label(p);
      if (!p.wait && p.ms / total > 0.08) seg.textContent = STAGE_INITIAL[p.stage];
    }
    const top = parts.reduce((a, b) => (b.ms > a.ms ? b : a), parts[0]);
    html('div', 'bd-note', t('bd.bottleneck', { what: top.wait ? t('bd.waitfor', { stage: stageName(top.stage) }) : stageName(top.stage), d: fmtDuration(top.ms), p: Math.round(top.ms / total * 100), total: fmtDuration(total) }), box);
  }

  // ---------- legends ----------
  function legend() {
    const ul = $('legend');
    ul.textContent = '';
    const item = (draw, label) => {
      const li = html('li', null, null, ul);
      const s = svg('svg', { width: 40, height: 20, viewBox: '0 0 40 20', 'aria-hidden': 'true' }, li);
      draw(s);
      html('span', null, label, li);
    };
    const color = STAGE_COLOR.execution;
    item((s) => { svg('rect', { x: 1, y: 2, width: 32, height: 16, rx: 4, fill: color }, s); check(s, 26, 10); }, t('lg.completed'));
    item((s) => { defs(s); svg('rect', { x: 1, y: 2, width: 30, height: 16, rx: 4, fill: color }, s); svg('rect', { x: 1, y: 2, width: 30, height: 16, rx: 4, fill: 'url(#stripes)' }, s); svg('path', { d: 'M30,2 l6,8 l-6,8 z', fill: color }, s); }, t('lg.running'));
    item((s) => svg('rect', { x: 1.75, y: 2.75, width: 30.5, height: 14.5, rx: 4, fill: 'none', stroke: color, 'stroke-width': 1.5, 'stroke-dasharray': '4 2' }, s), t('lg.waiting'));
    item((s) => { svg('rect', { x: 1, y: 2, width: 28, height: 16, rx: 4, fill: color }, s); badge(s, 'failed', 29, 10); }, t('lg.failed'));
    item((s) => { svg('rect', { x: 1, y: 2, width: 28, height: 16, rx: 4, fill: color }, s); badge(s, 'stopped-safely', 29, 10); }, t('lg.stopped'));
    item((s) => { defs(s); svg('rect', { x: 1, y: 2, width: 28, height: 16, rx: 4, fill: 'url(#hatch-execution)' }, s); badge(s, 'blocked', 29, 10); }, t('lg.blocked'));
    item((s) => { defs(s); svg('rect', { x: 1, y: 2, width: 28, height: 16, rx: 4, fill: 'url(#hatch-execution)' }, s); badge(s, 'quarantined', 29, 10); }, t('lg.quarantined'));
    item((s) => marker(s, 'degraded', 20, 11), t('lg.degraded'));
    item((s) => marker(s, 'retried', 20, 10), t('lg.retried'));
    item((s) => marker(s, 'stale', 20, 10), t('lg.stale'));
    item((s) => { marker(s, 'quarantined', 16, 10); svg('rect', { x: 21, y: 2, width: 13, height: 11, rx: 5.5, fill: '#f0efec' }, s); text(s, { class: 'count', x: 27.5, y: 10.6, 'text-anchor': 'middle' }, '3'); }, t('lg.cluster'));
    item((s) => { svg('rect', { x: 1, y: 2, width: 32, height: 16, rx: 4, fill: color }, s); for (const px of [8, 14, 18, 26]) svg('line', { x1: px, x2: px, y1: 14, y2: 17, stroke: 'rgba(14,14,13,.7)' }, s); }, t('lg.calls'));
    item((s) => closedTick(s, 18, 10), t('lg.closed'));
  }

  // ---------- interaction ----------
  function setView(t0, t1, follow) {
    state.viewChosen = true;
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
  $('funnel').addEventListener('click', (ev) => {
    const b = ev.target.closest('button');
    if (!b) return;
    state.filter.current = state.filter.current === b.dataset.current ? '' : b.dataset.current;
    schedule();
  });
  $('feed-toggle').addEventListener('click', () => setFeed(!state.feedOpen));
  $('trend-table').addEventListener('toggle', schedule);
  $('feed-close').addEventListener('click', () => setFeed(false));
  $('feed-filter').addEventListener('input', () => renderFeed(true));
  $('feed-kind').addEventListener('change', () => renderFeed(true));
  $('feed-every').addEventListener('change', () => renderFeed(true));
  $('problem-list').addEventListener('click', (ev) => {
    const b = ev.target.closest('button');
    if (!b) return;
    if (b.dataset.key && state.lanes.has(b.dataset.key)) openPanel(b.dataset.key);
    else if (b.dataset.a) setView(Number(b.dataset.a) - 15 * 60e3, Number(b.dataset.b) + 15 * 60e3, false);
  });
  $('problem-more').addEventListener('click', () => { state.problemsExpanded = !state.problemsExpanded; schedule(); });
  $('feed-new').addEventListener('click', () => renderFeed(true));
  $('feed-list').addEventListener('click', (ev) => { const li = ev.target.closest('li'); if (li && state.lanes.has(li.dataset.key)) openPanel(li.dataset.key); });
  $('feed-list').addEventListener('keydown', (ev) => {
    const li = ev.target.closest('li');
    if (!li) return;
    if (ev.key === 'Enter' && state.lanes.has(li.dataset.key)) openPanel(li.dataset.key);
    if (ev.key === 'ArrowDown' && li.nextElementSibling) { ev.preventDefault(); li.nextElementSibling.focus(); }
    if (ev.key === 'ArrowUp' && li.previousElementSibling) { ev.preventDefault(); li.previousElementSibling.focus(); }
  });
  $('sort').addEventListener('change', (ev) => { state.sort = ev.target.value === 'problems' ? 'problems' : 'recent'; schedule(); });
  $('panel-close').addEventListener('click', closePanel);
  document.addEventListener('keydown', (ev) => { if (ev.key === 'Escape' && state.selected) closePanel(); });
  labels.addEventListener('click', (ev) => { if (ev.target.closest('a')) return; const row = ev.target.closest('.label'); if (row) openPanel(row.dataset.key); });
  function focusLane(index) {
    const lanes = state.visible || [];
    if (!lanes.length) return;
    index = Math.max(0, Math.min(lanes.length - 1, index));
    state.focusIndex = index;
    const LANE_H = laneHeight(), top = index * LANE_H;
    if (top < lanesBox.scrollTop) lanesBox.scrollTop = top;
    else if (top + LANE_H > lanesBox.scrollTop + lanesBox.clientHeight) lanesBox.scrollTop = top + LANE_H - lanesBox.clientHeight;
    render();
    const row = labels.querySelector('[data-index="' + index + '"]');
    if (row) row.focus();
  }
  labels.addEventListener('keydown', (ev) => {
    const row = ev.target.closest('.label');
    if (!row) return;
    const index = Number(row.dataset.index), page = Math.max(1, Math.floor(lanesBox.clientHeight / laneHeight()) - 1);
    const moves = { ArrowDown: index + 1, ArrowUp: index - 1, PageDown: index + page, PageUp: index - page, Home: 0, End: Infinity };
    if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); openPanel(row.dataset.key); }
    else if (ev.key in moves) { ev.preventDefault(); focusLane(moves[ev.key] === Infinity ? (state.visible || []).length - 1 : moves[ev.key]); }
  });
  labels.addEventListener('focusin', (ev) => { const row = ev.target.closest('.label'); if (row) state.focusIndex = Number(row.dataset.index); });
  lanesBox.addEventListener('scroll', schedule, { passive: true });
  // Detail panel: arrows move between its events.
  $('panel-events').addEventListener('keydown', (ev) => {
    const li = ev.target.closest('li');
    if (!li) return;
    const items = Array.from($('panel-events').querySelectorAll('li:not(.group)'));
    const at = items.indexOf(li);
    const to = { ArrowDown: at + 1, ArrowUp: at - 1, Home: 0, End: items.length - 1 }[ev.key];
    if (to === undefined) return;
    ev.preventDefault();
    const next = items[Math.max(0, Math.min(items.length - 1, to))];
    for (const o of items) o.tabIndex = -1;
    next.tabIndex = 0;
    next.focus();
  });
  // Reduced motion: follows the OS setting until the viewer chooses.
  const motion = $('motion');
  function setReducedMotion(on, remember) {
    document.body.classList.toggle('reduce-motion', on);
    motion.setAttribute('aria-pressed', String(on));
    if (remember) { try { localStorage.setItem('flow-console-reduce-motion', on ? '1' : '0'); } catch (_) { /* storage unavailable */ } }
  }
  let stored = null;
  try { stored = localStorage.getItem('flow-console-reduce-motion'); } catch (_) { /* storage unavailable */ }
  setReducedMotion(stored !== null ? stored === '1' : window.matchMedia('(prefers-reduced-motion: reduce)').matches, false);
  motion.addEventListener('click', () => setReducedMotion(motion.getAttribute('aria-pressed') !== 'true', true));

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
    if (pending || state.replaying) return;
    pending = true;
    requestAnimationFrame(() => { pending = false; render(); });
  }

  // ---------- stream ----------
  function setSource(value, cls) {
    $('source-text').textContent = value;
    $('source-dot').className = 'dot' + (cls ? ' ' + cls : '');
  }
  function describeStatus() {
    const s = state.status;
    if (!state.connected) return setSource(t('src.reconnecting'), 'down');
    // A gap stays visible: the hub had already evicted events this page
    // never received, so the view is incomplete until it is rebuilt.
    const gap = state.gap ? t('src.gap') : '';
    if (!s) return setSource(t('src.connected') + gap, gap ? 'down' : 'live');
    const source = s.source === 'fixture' ? t('src.fixture') : t('src.journal');
    if (!s.healthy) return setSource(t('src.unavailable', { source }) + gap, 'down');
    const when = s.observedAt ? t('src.read', { t: fmtClock(Date.parse(s.observedAt), true) }) : '';
    setSource(t('src.live', { source }) + (s.problem === 'window-truncated' ? t('src.truncated') : '') + when + gap, gap ? 'down' : 'live');
  }
  let stream = null, cursor = '', retryDelay = 1000;
  function connect() {
    // Resume after the last received cursor, so a reconnect never duplicates.
    stream = new EventSource('events' + (cursor ? '?since=' + encodeURIComponent(cursor) : ''));
    state.replaying = true;
    stream.addEventListener('open', () => { state.connected = true; retryDelay = 1000; describeStatus(); });
    // Events arrive in "flows" batches: [{cursor, event}, ...].
    stream.addEventListener('flows', (m) => {
      if (m.lastEventId) cursor = m.lastEventId;
      let batch = [];
      try { batch = JSON.parse(m.data); } catch (_) { /* ignore malformed */ }
      if (Array.isArray(batch)) for (const item of batch) if (item && item.event && typeof item.event.time === 'string') addEvent(item.event);
      // During the replay only a counter updates; the page renders once at "ready".
      if (!state.replaying) schedule();
      else setSource(t('src.loading', { n: state.events.size }), 'live');
    });
    stream.addEventListener('status', (m) => { try { state.status = JSON.parse(m.data); describeStatus(); } catch (_) { /* ignore */ } });
    stream.addEventListener('ready', (m) => {
      try { const c = JSON.parse(m.data).cursor; if (c) cursor = String(c); } catch (_) { /* ignore */ }
      state.replaying = false;
      if (!document.body.dataset.readyMs) document.body.dataset.readyMs = String(Math.round(performance.now()));
      state.connected = true; describeStatus(); schedule();
      const open = params.get('open');
      if (open && state.lanes.has(open) && !state.selected) openPanel(open);
    });
    stream.addEventListener('gap', () => { state.gap = true; describeStatus(); });
    stream.addEventListener('error', () => {
      state.connected = false;
      describeStatus();
      // The browser retries a dropped stream itself (CONNECTING); a refused
      // one (for example 503 when every stream slot is taken) is CLOSED and
      // needs a new EventSource, with a growing delay.
      if (stream.readyState === EventSource.CLOSED) {
        stream.close();
        setTimeout(connect, retryDelay);
        retryDelay = Math.min(retryDelay * 2, 60000);
      }
    });
  }

  // Bound the page's own history like the server's: at most MAX_EVENTS, and
  // for the journal source nothing older than its window plus a margin.
  const MAX_EVENTS = 60000;
  function windowMs(value) {
    const m = /^(?:(\d+)h)?(?:(\d+)m)?(?:[\d.]+s)?$/.exec(value || '');
    return m && (m[1] || m[2]) ? (Number(m[1] || 0) * 3600e3 + Number(m[2] || 0) * 60e3) : 0;
  }
  function prune() {
    let cutoff = -Infinity;
    const span = state.status && state.status.source === 'journal' ? windowMs(state.status.window) : 0;
    if (span) cutoff = Date.now() - span - 2 * 3600e3;
    let excess = state.events.size - MAX_EVENTS;
    if (excess <= 0 && cutoff === -Infinity) return;
    const ordered = Array.from(state.events.values()).sort((a, b) => a.t - b.t);
    const drop = new Set();
    for (const e of ordered) {
      if (excess > 0 || e.t < cutoff) { drop.add(e.id); excess--; } else break;
    }
    if (!drop.size) return;
    for (const id of drop) state.events.delete(id);
    for (const [key, lane] of state.lanes) {
      const kept = lane.events.filter((e) => !drop.has(e.id));
      if (kept.length === lane.events.length) continue;
      if (!kept.length) { state.lanes.delete(key); if (state.selected === key) closePanel(); continue; }
      lane.events = kept;
      lane.dirty = true;
    }
    schedule();
  }

  // Static strings: data-i18n sets the text, data-i18n-<attribute> an attribute.
  function applyStrings() {
    document.documentElement.lang = lang;
    document.title = t('title');
    for (const el of document.querySelectorAll('[data-i18n]')) el.textContent = t(el.dataset.i18n);
    for (const el of document.querySelectorAll('*')) {
      for (const attr of el.getAttributeNames()) {
        if (attr.startsWith('data-i18n-')) el.setAttribute(attr.slice(10), t(el.getAttribute(attr)));
      }
    }
    $('timezone').textContent = t('tz', { tz: tzOffset });
    $('timezone').title = t('tz.title', { name: tzName ? tzName + ', ' : '', tz: tzOffset });
    // Compact toggles: a code or a symbol, with the full name as label.
    $('theme').textContent = state.theme === 'light' ? '☾' : '☀';
    $('theme').setAttribute('aria-label', state.theme === 'light' ? t('theme.dark') : t('theme.light'));
    $('theme').title = $('theme').getAttribute('aria-label');
    $('lang').textContent = lang === 'es' ? 'EN' : 'ES';
    $('lang').setAttribute('aria-label', t('lang.title'));
    $('funnel').textContent = '';
    legend();
    describeStatus();
  }
  function remember(key, value) { try { localStorage.setItem(key, value); } catch (_) { /* storage unavailable */ } }
  function recall(key) { try { return localStorage.getItem(key); } catch (_) { return null; } }
  function setLang(next, save) {
    lang = STRINGS[next] ? next : 'en';
    if (save) remember('flow-console-lang', lang);
    applyStrings();
    schedule();
  }
  function setTheme(next, save) {
    state.theme = next === 'light' ? 'light' : 'dark';
    document.documentElement.dataset.theme = state.theme;
    Object.assign(STAGE_COLOR, STAGE_COLORS[state.theme]);
    if (save) remember('flow-console-theme', state.theme);
    applyStrings();
    schedule();
  }
  $('lang').addEventListener('click', () => setLang(lang === 'es' ? 'en' : 'es', true));
  $('theme').addEventListener('click', () => setTheme(state.theme === 'light' ? 'dark' : 'light', true));
  // Printing draws every lane, not only those scrolled into view.
  window.addEventListener('beforeprint', () => { state.printing = true; render(); });
  window.addEventListener('afterprint', () => { state.printing = false; render(); });
  const wantedLang = params.get('lang') || recall('flow-console-lang') || ((navigator.language || 'en').toLowerCase().startsWith('es') ? 'es' : 'en');
  lang = STRINGS[wantedLang] ? wantedLang : 'en';
  const wantedTheme = params.get('theme') || recall('flow-console-theme') || (window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark');
  setTheme(wantedTheme, false);
  if (params.get('feed') === '1') setFeed(true);
  if (FUNNEL.some((f) => f.key === params.get('now'))) state.filter.current = params.get('now');
  if (params.get('issue')) { state.filter.issue = params.get('issue').slice(0, 100); $('issue-filter').value = state.filter.issue; }
  if (params.get('sort') === 'problems') { state.sort = 'problems'; $('sort').value = 'problems'; }
  const initialStatus = params.get('status');
  if (['inflight', 'delivered', 'failed', 'blocked'].indexOf(initialStatus) >= 0) {
    state.filter.status = initialStatus;
    document.querySelectorAll('#status-filter button').forEach((o) => o.setAttribute('aria-pressed', String(o.dataset.status === initialStatus)));
  }
  connect();
  // Advance the now line and open bars.
  setInterval(schedule, 5000);
  setInterval(prune, 60000);
})();
