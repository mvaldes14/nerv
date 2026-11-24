(() => {
  'use strict';

  const tokenKey = 'nerv.operator.token';
  const tokenExpiryKey = 'nerv.operator.token-expires';
  const tokenTTL = 30 * 60 * 1000;
  const pageSize = 25;
  let offset = 0;
  let hasMore = false;
  let selectedIncidentID = '';
  let selectedProposalID = '';
  let activeTab = 'ask';
  let filterOptionsLoaded = false;
  let prRefreshFailures = 0;

  const $ = (selector) => document.querySelector(selector);
  const incidentList = $('#incident-list');
  const incidentDetail = $('#detail');
  const remediationList = $('#remediation-list');
  const remediationDetail = $('#remediation-detail');

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

  function setAuthenticated(authenticated) {
    $('#auth-gate').hidden = authenticated;
    $('#operator-shell').hidden = !authenticated;
    $('#session-controls').hidden = !authenticated;
    if (authenticated) $('#connection').textContent = 'Token active for 30 minutes';
  }

  function resetOperatorState() {
    activeTab = 'ask';
    offset = 0;
    hasMore = false;
    selectedIncidentID = '';
    selectedProposalID = '';
    filterOptionsLoaded = false;
    incidentList.replaceChildren();
    remediationList.replaceChildren();
    const greeting = document.createElement('div');
    greeting.className = 'message assistant';
    greeting.append(text('strong', 'NERV'), text('p', 'Ask a freeform question. Each question runs as a separate investigation; responses stay visible in this browser session and can be opened from Incidents.'));
    $('#conversation').replaceChildren(greeting);
    incidentDetail.replaceChildren(text('p', 'Select an incident after reconnecting.', 'status-line'));
    remediationDetail.replaceChildren(text('p', 'Select a proposal after reconnecting.', 'status-line'));
  }

  function lockOperator() {
    clearToken();
    resetOperatorState();
    setAuthenticated(false);
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
    if (response.status === 401) lockOperator();
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
    if (!value) return 'Not recorded';
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

  function activateTab(name) {
    activeTab = name;
    for (const button of document.querySelectorAll('.tab')) {
      const selected = button.dataset.tab === name;
      button.classList.toggle('active', selected);
      button.setAttribute('aria-selected', String(selected));
    }
    for (const panel of document.querySelectorAll('.tab-panel')) panel.hidden = panel.id !== `tab-${name}`;
    history.replaceState(null, '', `#${name}`);
    if (name === 'incidents') loadFilterOptions().then(loadIncidents);
    if (name === 'remediation') loadRemediationQueue();
  }

  async function loadFilterOptions() {
    if (!token() || filterOptionsLoaded) return;
    try {
      const options = await api('/investigations/filter-options');
      populateSelect('#filters select[name="source"]', options.sources || [], 'All sources');
      populateSelect('#filters select[name="status"]', options.statuses || [], 'All statuses');
      populateSelect('#filters select[name="namespace"]', options.namespaces || [], 'All namespaces');
      populateSelect('#filters select[name="workload"]', options.workloads || [], 'All workloads');
      filterOptionsLoaded = true;
    } catch (error) {
      const status = $('#list-status');
      status.textContent = error.message;
      status.className = 'status-line error';
    }
  }

  function populateSelect(selector, values, emptyLabel) {
    const select = $(selector);
    const current = select.value;
    select.replaceChildren();
    const empty = document.createElement('option');
    empty.value = '';
    empty.textContent = emptyLabel;
    select.append(empty);
    for (const value of values) {
      const option = document.createElement('option');
      option.value = value;
      option.textContent = value;
      select.append(option);
    }
    if (values.includes(current)) select.value = current;
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
    if (!token()) return;
    const status = $('#list-status');
    status.textContent = 'Loading incidents…';
    status.className = 'status-line';
    incidentList.replaceChildren();
    try {
      const page = await api(`/investigations?${currentFilters()}`);
      hasMore = page.has_more;
      if (!page.items.length) incidentList.append(text('p', 'No incidents match these filters.', 'status-line'));
      for (const incident of page.items) incidentList.append(incidentButton(incident));
      status.textContent = `${page.items.length} incident${page.items.length === 1 ? '' : 's'} loaded`;
      $('#page-label').textContent = page.items.length ? `Showing ${offset + 1}–${offset + page.items.length}` : '';
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
    button.className = `incident${incident.id === selectedIncidentID ? ' active' : ''}`;
    const top = document.createElement('div');
    top.className = 'incident-top';
    top.append(badge(incident.status, incident.status !== 'completed'), text('span', incident.source, 'meta'));
    button.append(top, text('strong', incident.question || incident.id));
    const meta = document.createElement('div');
    meta.className = 'meta';
    meta.append(text('span', targetName(incident.target)), text('span', formatTime(incident.updated_at)));
    button.append(meta);
    button.addEventListener('click', () => loadIncidentDetail(incident.id));
    return button;
  }

  function renderIncidentDetail(data) {
    const incident = data.incident;
    const finding = data.finding;
    incidentDetail.replaceChildren();
    const heading = document.createElement('div');
    heading.className = 'section-heading';
    const title = document.createElement('div');
    title.append(text('p', incident.source, 'eyebrow'), text('h2', incident.question));
    heading.append(title, badge(incident.status, incident.status !== 'completed'));
    incidentDetail.append(heading);

    const meta = document.createElement('div');
    meta.className = 'detail-meta';
    meta.append(text('span', `ID ${incident.id}`), text('span', targetName(incident.target)), text('span', `Created ${formatTime(incident.created_at)}`), text('span', `Updated ${formatTime(incident.updated_at)}`));
    incidentDetail.append(meta);

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
      fact('Review required', finding?.decision?.review_required ? 'Yes' : 'No')
    );
    decision.append(grid);
    incidentDetail.append(decision);

    const outcome = section(finding?.decision?.action === 'suppress' ? 'Policy outcome' : 'Model-generated finding');
    outcome.append(text('div', finding?.answer || 'Finding not available while investigation is running.', 'output'));
    incidentDetail.append(outcome);

    const evidence = section('Evidence and tool output');
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
    incidentDetail.append(evidence);

    const notifications = section('Notification state');
    if (!data.notifications?.length) notifications.append(text('p', 'No notification attempts recorded.', 'status-line'));
    for (const attempt of data.notifications || []) {
      const node = document.createElement('div');
      node.className = 'evidence-item';
      node.append(text('strong', `${attempt.sink}: ${attempt.status}`), text('p', [attempt.external_id, attempt.error, formatTime(attempt.attempted_at)].filter(Boolean).join(' — ')));
      notifications.append(node);
    }
    incidentDetail.append(notifications);

    const remediationLink = section('Remediation');
    remediationLink.append(text('p', 'Approval and GitHub PR controls live in the Remediation tab.', 'status-line'));
    const open = text('button', 'Open remediation queue', 'secondary');
    open.type = 'button';
    open.addEventListener('click', () => activateTab('remediation'));
    remediationLink.append(open);
    incidentDetail.append(remediationLink);
  }

  async function loadIncidentDetail(id) {
    selectedIncidentID = id;
    incidentDetail.replaceChildren(text('p', 'Loading investigation…', 'status-line'));
    try {
      const data = await api(`/investigations/${encodeURIComponent(id)}`);
      renderIncidentDetail(data);
      for (const button of incidentList.querySelectorAll('.incident')) button.classList.toggle('active', button.dataset.incidentId === id);
    } catch (error) {
      incidentDetail.replaceChildren(text('p', error.message, 'error'));
    }
  }

  async function loadRemediationQueue() {
    if (!token()) return;
    const status = $('#remediation-list-status');
    status.textContent = 'Loading proposals…';
    status.className = 'status-line';
    remediationList.replaceChildren();
    const filter = $('#remediation-status').value;
    const query = filter ? `?status=${encodeURIComponent(filter)}` : '';
    try {
      const data = await api(`/remediation-proposals${query}`);
      const items = await refreshOpenPullRequests(data.items || []);
      if (!items.length) remediationList.append(text('p', 'No remediation proposals match this view.', 'status-line'));
      for (const proposal of items) remediationList.append(remediationButton(proposal));
      status.textContent = `${items.length} proposal${items.length === 1 ? '' : 's'} loaded${prRefreshFailures ? `; ${prRefreshFailures} PR status refresh failed` : ''}`;
      status.className = prRefreshFailures ? 'status-line error' : 'status-line';
      const selected = items.find((item) => item.id === selectedProposalID) || items[0];
      if (selected) renderRemediationDetail(selected);
    } catch (error) {
      status.textContent = error.message;
      status.className = 'status-line error';
    }
  }

  async function refreshOpenPullRequests(items) {
    prRefreshFailures = 0;
    return Promise.all(items.map(async (proposal) => {
      if (proposal.submission_status !== 'submitted' || !proposal.pull_request_number) return proposal;
      if (proposal.pull_request_state && proposal.pull_request_state !== 'open') return proposal;
      try {
        return await api(`/remediation-proposals/${encodeURIComponent(proposal.id)}/refresh`, {method: 'POST'});
      } catch (_) {
        prRefreshFailures += 1;
        return proposal;
      }
    }));
  }

  function remediationButton(proposal) {
    const button = document.createElement('button');
    button.type = 'button';
    button.dataset.proposalId = proposal.id;
    button.className = `incident${proposal.id === selectedProposalID ? ' active' : ''}`;
    const top = document.createElement('div');
    top.className = 'incident-top';
    top.append(badge(proposal.status, proposal.status !== 'approved'), badge(proposal.submission_status || 'not_submitted', proposal.submission_status === 'failed'));
    if (proposal.pull_request_state) top.append(badge(`PR ${proposal.pull_request_merged ? 'merged' : proposal.pull_request_state}`, proposal.pull_request_state === 'closed' && !proposal.pull_request_merged));
    button.append(top, text('strong', proposal.summary));
    button.append(text('div', `${proposal.target_resource?.kind || 'Workload'} ${proposal.target_resource?.namespace || ''}/${proposal.target_resource?.name || ''} · ${proposal.expected_state?.replicas ?? '?'} → ${proposal.desired_change?.replicas ?? '?'}`, 'meta'));
    button.addEventListener('click', () => renderRemediationDetail(proposal));
    return button;
  }

  function renderRemediationDetail(proposal) {
    selectedProposalID = proposal.id;
    remediationDetail.replaceChildren();
    for (const button of remediationList.querySelectorAll('.incident')) button.classList.toggle('active', button.dataset.proposalId === proposal.id);

    const heading = document.createElement('div');
    heading.className = 'section-heading';
    const title = document.createElement('div');
    title.append(text('p', 'GitOps remediation', 'eyebrow'), text('h2', proposal.summary));
    heading.append(title, badge(proposal.status, proposal.status !== 'approved'));
    remediationDetail.append(heading);

    const resource = proposal.target_resource || {};
    const grid = document.createElement('div');
    grid.className = 'decision-grid';
    grid.append(
      fact('Repository', proposal.target_repository),
      fact('Path', proposal.target_path),
      fact('Resource', [resource.api_version, resource.kind, resource.namespace, resource.name].filter(Boolean).join(' / ')),
      fact('Replica change', `${proposal.expected_state?.replicas ?? '?'} → ${proposal.desired_change?.replicas ?? '?'}`),
      fact('Risk', `${proposal.risk_level}: ${proposal.risk_detail}`),
      fact('Expires', formatTime(proposal.expires_at)),
      fact('Reviewer', proposal.decision_actor || 'Pending'),
      fact('PR submission', proposal.submission_status || 'not_submitted'),
      fact('Pull request state', proposal.pull_request_merged ? 'merged' : (proposal.pull_request_state || 'not checked')),
      fact('PR last checked', formatTime(proposal.pull_request_checked_at))
    );
    remediationDetail.append(grid, fact('Rationale', proposal.rationale));

    const evidence = section('Evidence snapshots');
    const evidenceList = document.createElement('div');
    evidenceList.className = 'evidence';
    for (const item of proposal.evidence || []) {
      const node = document.createElement('div');
      node.className = 'evidence-item';
      node.append(text('strong', `Evidence ${item.incident_evidence_ordinal}: ${item.source}`), text('p', item.summary));
      evidenceList.append(node);
    }
    evidence.append(evidenceList);
    remediationDetail.append(evidence);

    const diff = section(`Rendered diff (${proposal.render_format})`);
    diff.append(text('pre', proposal.rendered_diff, 'diff'));
    remediationDetail.append(diff);

    if (proposal.status === 'pending' && new Date(proposal.expires_at).valueOf() > Date.now()) remediationDetail.append(decisionControls(proposal));
    if (proposal.status === 'approved' && proposal.submission_status !== 'submitted' && proposal.submission_status !== 'submitting') remediationDetail.append(submissionControls(proposal));
    if (proposal.submission_error) remediationDetail.append(text('p', proposal.submission_error, 'error'));
    if (proposal.pull_request_url) {
      const refresh = text('button', 'Refresh PR status', 'secondary');
      refresh.type = 'button';
      refresh.addEventListener('click', async () => {
        refresh.disabled = true;
        try {
          const updated = await api(`/remediation-proposals/${encodeURIComponent(proposal.id)}/refresh`, {method: 'POST'});
          renderRemediationDetail(updated);
          await loadRemediationQueue();
        } catch (error) {
          remediationDetail.append(text('p', error.message, 'error'));
          refresh.disabled = false;
        }
      });
      remediationDetail.append(refresh);
      const link = document.createElement('a');
      link.className = 'pr-link';
      link.href = proposal.pull_request_url;
      link.target = '_blank';
      link.rel = 'noopener noreferrer';
      link.textContent = `Open GitHub PR #${proposal.pull_request_number}`;
      remediationDetail.append(link);
    }
  }

  function decisionControls(proposal) {
    const form = document.createElement('form');
    form.className = 'proposal-decision';
    const actor = document.createElement('input');
    actor.required = true;
    actor.maxLength = 200;
    actor.placeholder = 'Reviewer name';
    actor.setAttribute('aria-label', 'Reviewer name');
    const comment = document.createElement('textarea');
    comment.maxLength = 2000;
    comment.rows = 2;
    comment.placeholder = 'Decision comment';
    comment.setAttribute('aria-label', 'Decision comment');
    const approve = text('button', 'Approve');
    approve.type = 'button';
    const reject = text('button', 'Reject', 'secondary');
    reject.type = 'button';
    const status = text('span', '', 'status-line');
    const decide = async (decision) => {
      const reviewer = actor.value.trim();
      if (!reviewer) { status.textContent = 'Reviewer name is required.'; status.className = 'status-line error'; return; }
      if (decision === 'approve' && !window.confirm('Approve this remediation proposal? Approval alone does not create a repository change.')) return;
      approve.disabled = true;
      reject.disabled = true;
      status.textContent = `${decision === 'approve' ? 'Approving' : 'Rejecting'}…`;
      try {
        await api(`/remediation-proposals/${encodeURIComponent(proposal.id)}/${decision}`, {method: 'POST', body: JSON.stringify({actor: reviewer, comment: comment.value.trim()})});
        await loadRemediationQueue();
      } catch (error) {
        status.textContent = error.message;
        status.className = 'status-line error';
        approve.disabled = false;
        reject.disabled = false;
      }
    };
    approve.addEventListener('click', () => decide('approve'));
    reject.addEventListener('click', () => decide('reject'));
    form.append(actor, comment, approve, reject, status);
    return form;
  }

  function submissionControls(proposal) {
    const controls = document.createElement('div');
    controls.className = 'proposal-decision';
    const submit = text('button', proposal.submission_status === 'failed' ? 'Retry PR submission' : 'Submit GitHub PR');
    submit.type = 'button';
    const status = text('span', '', 'status-line');
    submit.addEventListener('click', async () => {
      if (!window.confirm('Create a Git branch, commit, and pull request? NERV will not merge it.')) return;
      submit.disabled = true;
      status.textContent = 'Submitting GitHub PR…';
      try {
        await api(`/remediation-proposals/${encodeURIComponent(proposal.id)}/submit`, {method: 'POST'});
        await loadRemediationQueue();
      } catch (error) {
        status.textContent = error.message;
        status.className = 'status-line error';
        submit.disabled = false;
      }
    });
    controls.append(submit, status);
    return controls;
  }

  $('#auth-form').addEventListener('submit', async (event) => {
    event.preventDefault();
    const value = $('#token').value.trim();
    const status = $('#auth-status');
    if (!value) return;
    saveToken(value);
    status.textContent = 'Validating…';
    status.className = 'status-line';
    try {
      await api('/auth/check');
      $('#token').value = '';
      status.textContent = '';
      setAuthenticated(true);
      activateTab(['ask', 'incidents', 'remediation'].includes(location.hash.slice(1)) ? location.hash.slice(1) : 'ask');
    } catch (error) {
      lockOperator();
      status.textContent = error.message;
      status.className = 'status-line error';
    }
  });

  $('#forget-token').addEventListener('click', lockOperator);

  function appendConversationMessage(role, content, incidentID = '') {
    const message = document.createElement('div');
    message.className = `message ${role}`;
    message.append(text('strong', role === 'assistant' ? 'NERV' : 'You'), text('p', content));
    if (incidentID) {
      const open = text('button', 'Open investigation', 'secondary');
      open.type = 'button';
      open.addEventListener('click', async () => {
        selectedIncidentID = incidentID;
        activateTab('incidents');
        await loadIncidentDetail(incidentID);
      });
      message.append(open);
    }
    $('#conversation').append(message);
    message.scrollIntoView({behavior: 'smooth', block: 'nearest'});
  }

  $('#ask-form').addEventListener('submit', async (event) => {
    event.preventDefault();
    const form = event.currentTarget;
    const data = new FormData(form);
    const status = $('#ask-status');
    const submit = form.querySelector('button[type="submit"]');
    const body = {question: String(data.get('question') || '').trim()};
    appendConversationMessage('user', body.question);
    submit.disabled = true;
    status.textContent = 'Investigation running…';
    status.className = '';
    try {
      const finding = await api('/ask', {method: 'POST', body: JSON.stringify(body)});
      status.textContent = `Completed: ${finding.id}`;
      appendConversationMessage('assistant', finding.answer || 'Investigation completed without a textual answer.', finding.id);
      form.reset();
      selectedIncidentID = finding.id;
      offset = 0;
    } catch (error) {
      status.textContent = error.message;
      status.className = 'error';
      appendConversationMessage('assistant', `Request failed: ${error.message}`);
    } finally {
      submit.disabled = false;
    }
  });

  for (const button of document.querySelectorAll('.tab')) button.addEventListener('click', () => activateTab(button.dataset.tab));
  $('#filters').addEventListener('submit', (event) => { event.preventDefault(); offset = 0; loadIncidents(); });
  $('#clear-filters').addEventListener('click', () => {
    $('#filters').reset();
    offset = 0;
    loadIncidents();
  });
  $('#refresh').addEventListener('click', async () => {
    filterOptionsLoaded = false;
    await loadFilterOptions();
    loadIncidents();
  });
  $('#previous').addEventListener('click', () => { offset = Math.max(0, offset - pageSize); loadIncidents(); });
  $('#next').addEventListener('click', () => { if (hasMore) { offset += pageSize; loadIncidents(); } });
  $('#refresh-remediation').addEventListener('click', loadRemediationQueue);
  $('#remediation-status').addEventListener('change', () => { selectedProposalID = ''; loadRemediationQueue(); });

  async function initialize() {
    setAuthenticated(false);
    if (!token()) return;
    try {
      await api('/auth/check');
      setAuthenticated(true);
      const requested = location.hash.slice(1);
      activateTab(['ask', 'incidents', 'remediation'].includes(requested) ? requested : activeTab);
    } catch (_) {
      lockOperator();
    }
  }

  initialize();
})();
