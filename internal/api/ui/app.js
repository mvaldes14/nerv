(() => {
  'use strict';

  const tokenKey = 'nerv.operator.token';
  const tokenExpiryKey = 'nerv.operator.token-expires';
  const tokenTTL = 30 * 60 * 1000;
  const pageSize = 25;
  let offset = 0;
  let hasMore = false;
  let selectedID = '';

  const $ = (selector) => document.querySelector(selector);
  const list = $('#incident-list');
  const detail = $('#detail');
  const tokenInput = $('#token');
  const connection = $('#connection');

  function clearToken() {
    sessionStorage.removeItem(tokenKey);
    sessionStorage.removeItem(tokenExpiryKey);
  }

  function token() {
    const value = sessionStorage.getItem(tokenKey) || '';
    const expires = Number(sessionStorage.getItem(tokenExpiryKey) || 0);
    if (!value || !expires || Date.now() >= expires) {
      clearToken();
      return '';
    }
    return value;
  }

  function saveToken(value) {
    sessionStorage.setItem(tokenKey, value);
    sessionStorage.setItem(tokenExpiryKey, String(Date.now() + tokenTTL));
  }

  function setConnection() {
    const connected = Boolean(token());
    connection.textContent = connected ? 'Token active for 30 minutes' : 'Token required';
    connection.className = connected ? 'pill' : 'pill muted';
  }

  async function api(path, options = {}) {
    const headers = new Headers(options.headers || {});
    const bearer = token();
    if (bearer) headers.set('Authorization', `Bearer ${bearer}`);
    if (options.body) headers.set('Content-Type', 'application/json');
    const response = await fetch(path, {...options, headers});
    let body = {};
    const contentType = response.headers.get('content-type') || '';
    if (contentType.includes('application/json')) {
      try { body = await response.json(); } catch (_) { body = {}; }
    } else {
      const responseText = await response.text();
      body = {error: responseText || `Unexpected response type: ${contentType || 'unknown'}`};
    }
    if (!response.ok) throw new Error(body.error || `Request failed (${response.status})`);
    return body;
  }

  function text(tag, value, className) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    node.textContent = value ?? '';
    return node;
  }

  function formatTime(value) {
    if (!value) return 'unknown time';
    const date = new Date(value);
    return Number.isNaN(date.valueOf()) ? value : date.toLocaleString();
  }

  function targetName(target) {
    if (!target) return 'No workload target';
    return [target.namespace, target.kind, target.name, target.container].filter(Boolean).join(' / ') || 'No workload target';
  }

  function badge(value, warning = false) {
    return text('span', value || 'unknown', warning ? 'pill warn' : 'pill');
  }

  function currentFilters() {
    const data = new FormData($('#filters'));
    const params = new URLSearchParams({limit: String(pageSize), offset: String(offset)});
    for (const name of ['source', 'status', 'namespace', 'workload']) {
      const value = String(data.get(name) || '').trim();
      if (value) params.set(name, value);
    }
    return params;
  }

  async function loadIncidents() {
    const status = $('#list-status');
    status.textContent = 'Loading incidents…';
    status.className = 'status-line';
    list.replaceChildren();
    try {
      const page = await api(`/investigations?${currentFilters()}`);
      hasMore = page.has_more;
      if (!page.items.length) list.append(text('p', 'No incidents match these filters.', 'status-line'));
      for (const incident of page.items) list.append(incidentButton(incident));
      status.textContent = `${page.items.length} incident${page.items.length === 1 ? '' : 's'} loaded`;
      $('#page-label').textContent = `Showing ${offset + 1}–${offset + page.items.length}`;
      $('#previous').disabled = offset === 0;
      $('#next').disabled = !hasMore;
    } catch (error) {
      status.textContent = error.message;
      status.className = 'status-line error';
      $('#page-label').textContent = '';
      $('#previous').disabled = true;
      $('#next').disabled = true;
    }
  }

  function incidentButton(incident) {
    const button = document.createElement('button');
    button.type = 'button';
    button.dataset.incidentId = incident.id;
    button.className = `incident${incident.id === selectedID ? ' active' : ''}`;
    const top = document.createElement('div');
    top.className = 'incident-top';
    top.append(badge(incident.status, incident.status !== 'completed'), text('span', incident.source, 'meta'));
    button.append(top, text('strong', incident.question || incident.id));
    const meta = document.createElement('div');
    meta.className = 'meta';
    meta.append(
      text('span', `ID ${incident.id}`),
      text('span', targetName(incident.target)),
      text('span', `Created ${formatTime(incident.created_at)}`),
      text('span', `Updated ${formatTime(incident.updated_at)}`)
    );
    button.append(meta);
    button.addEventListener('click', () => loadDetail(incident.id));
    return button;
  }

  function fact(label, value) {
    const node = document.createElement('div');
    node.className = 'fact';
    node.append(text('span', label), text('strong', value || 'Not recorded'));
    return node;
  }

  function section(title) {
    const node = document.createElement('section');
    node.className = 'detail-section';
    node.append(text('h3', title));
    return node;
  }

  function renderDetail(data) {
    const incident = data.incident;
    const finding = data.finding;
    detail.replaceChildren();
    const heading = document.createElement('div');
    heading.className = 'section-heading';
    const title = document.createElement('div');
    title.append(text('p', incident.source, 'eyebrow'), text('h2', incident.question));
    heading.append(title, badge(incident.status, incident.status !== 'completed'));
    detail.append(heading);

    const meta = document.createElement('div');
    meta.className = 'detail-meta';
    meta.append(text('span', `ID ${incident.id}`), text('span', targetName(incident.target)), text('span', `Created ${formatTime(incident.created_at)}`), text('span', `Updated ${formatTime(incident.updated_at)}`));
    detail.append(meta);

    const decision = section('Decision and recommendation');
    const grid = document.createElement('div');
    grid.className = 'decision-grid';
    grid.append(
      fact('Decision action', finding?.decision?.action || 'Pending'),
      fact('Decision provider', finding?.decision?.provider || 'Local NERV policy'),
      fact('Decision reason', finding?.decision?.reason || 'Not recorded'),
      fact('Decision request ID', finding?.decision?.request_id || 'Not provided'),
      fact('Confidence', finding?.confidence || 'Unknown / not asserted'),
      fact('Recommended action', finding?.recommended_action || 'Not recorded'),
      fact('Executed tools', finding?.tools_used?.length ? finding.tools_used.join(', ') : 'None recorded'),
      fact('Budget stop reason', finding?.budget?.stop_reason || 'Completed within budget'),
      fact('Review required', finding?.decision?.review_required ? 'Yes' : 'No')
    );
    decision.append(grid);
    detail.append(decision);

    const suppressed = finding?.decision?.action === 'suppress';
    const outcome = section(suppressed ? 'Policy outcome' : 'Model-generated finding');
    outcome.append(text('div', finding?.answer || 'Finding not available while investigation is running.', 'output'));
    if (!suppressed) {
      const modelMeta = document.createElement('div');
      modelMeta.className = 'detail-meta';
      modelMeta.append(
        text('span', `Provider: ${finding?.provider || 'not recorded'}`),
        text('span', `Model: ${finding?.model || 'not recorded'}`),
        text('span', `Rounds: ${finding?.budget?.rounds_used ?? 'not recorded'}/${finding?.budget?.max_rounds ?? 'not recorded'}`),
        text('span', `Tool calls: ${finding?.budget?.tool_calls_used ?? 'not recorded'}/${finding?.budget?.max_tool_calls ?? 'not recorded'}`),
        text('span', `Tokens: ${finding?.budget?.total_tokens ?? 'not recorded'}/${finding?.budget?.max_tokens ?? 'not recorded'}`)
      );
      outcome.append(modelMeta);
      const uncertainty = finding?.uncertainty?.length ? finding.uncertainty.join(' • ') : 'No structured uncertainty recorded; confidence remains unknown.';
      outcome.append(fact('Uncertainty', uncertainty));
    }
    detail.append(outcome);

    const evidence = section('Evidence and tool output');
    evidence.append(text('p', 'Persisted source material. Kept separate from policy and model outcomes.', 'status-line'));
    const evidenceList = document.createElement('div');
    evidenceList.className = 'evidence';
    for (const item of data.evidence || []) {
      const node = document.createElement('div');
      node.className = 'evidence-item';
      node.append(text('strong', item.source), text('p', item.summary));
      evidenceList.append(node);
    }
    if (!evidenceList.childElementCount) evidenceList.append(text('p', 'No evidence persisted yet.', 'status-line'));
    evidence.append(evidenceList);
    detail.append(evidence);

    const notifications = section('Notification state');
    if (!data.notifications?.length) {
      notifications.append(text('p', 'No notification attempts recorded.', 'status-line'));
    } else {
      for (const attempt of data.notifications) {
        const node = document.createElement('div');
        node.className = 'evidence-item';
        node.append(text('strong', `${attempt.sink}: ${attempt.status}`), text('p', [attempt.external_id, attempt.error, formatTime(attempt.attempted_at)].filter(Boolean).join(' — ')));
        notifications.append(node);
      }
    }
    detail.append(notifications);
  }

  async function loadDetail(id) {
    selectedID = id;
    detail.replaceChildren(text('p', 'Loading investigation…', 'status-line'));
    try {
      const data = await api(`/investigations/${encodeURIComponent(id)}`);
      renderDetail(data);
      for (const button of list.querySelectorAll('.incident')) button.classList.toggle('active', button.dataset.incidentId === id);
    } catch (error) {
      detail.replaceChildren(text('p', error.message, 'error'));
    }
  }

  $('#auth-form').addEventListener('submit', (event) => {
    event.preventDefault();
    const value = tokenInput.value.trim();
    if (value) saveToken(value);
    else clearToken();
    tokenInput.value = '';
    setConnection();
    offset = 0;
    loadIncidents();
  });

  $('#forget-token').addEventListener('click', () => {
    clearToken();
    tokenInput.value = '';
    setConnection();
    list.replaceChildren();
    detail.replaceChildren(text('p', 'Bearer token removed from this browser session.', 'status-line'));
  });

  $('#ask-form').addEventListener('submit', async (event) => {
    event.preventDefault();
    const form = event.currentTarget;
    const data = new FormData(form);
    const status = $('#ask-status');
    const submit = form.querySelector('button[type="submit"]');
    const target = {};
    for (const name of ['namespace', 'kind', 'name', 'container']) {
      const value = String(data.get(name) || '').trim();
      if (value) target[name] = value;
    }
    const body = {question: String(data.get('question') || '').trim()};
    if (Object.keys(target).length) body.target = target;
    submit.disabled = true;
    status.textContent = 'Investigation running…';
    status.className = '';
    try {
      const finding = await api('/ask', {method: 'POST', body: JSON.stringify(body)});
      status.textContent = `Completed: ${finding.id}`;
      selectedID = finding.id;
      offset = 0;
      await loadIncidents();
      await loadDetail(finding.id);
    } catch (error) {
      status.textContent = error.message;
      status.className = 'error';
    } finally {
      submit.disabled = false;
    }
  });

  $('#filters').addEventListener('submit', (event) => { event.preventDefault(); offset = 0; loadIncidents(); });
  $('#refresh').addEventListener('click', loadIncidents);
  $('#previous').addEventListener('click', () => { offset = Math.max(0, offset - pageSize); loadIncidents(); });
  $('#next').addEventListener('click', () => { if (hasMore) { offset += pageSize; loadIncidents(); } });

  setConnection();
  if (token()) loadIncidents();
})();
