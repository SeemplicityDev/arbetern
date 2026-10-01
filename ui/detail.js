/* Detail pages rendered inside the console shell: one workflow, dashboard or project.
   Loaded before app.js, which owns routing and the shared helpers. */

let detailRoute = null;
let detailEntity = null;
let detailTimer = null;
let detailToken = 0;

function detailRootFor(kind) { return document.getElementById({ workflow: 'wf-detail', dashboard: 'dash-detail', project: 'proj-detail' }[kind]); }
function detailPath(kind, agent, id) { return `/ui/${encodeURIComponent(agent)}/${kind}/${encodeURIComponent(id)}`; }
function detailApi(kind, agent, id) { return `/api/${kind}s/${encodeURIComponent(agent)}/${encodeURIComponent(id)}`; }

function openDetail(kind, agent, id) {
  const same = detailRoute && detailRoute.kind === kind && detailRoute.agent === agent && detailRoute.id === id;
  if (!same) {
    detailToken++;
    stopDetailPolling();
    detailRoute = { kind, agent, id };
    detailEntity = null;
    const root = detailRootFor(kind);
    delete root.dataset.rendered;
    root.innerHTML = `<div class="empty tall">Loading ${kind}…</div>`;
  }
  showPage(kind);
  if (same) {
    if (detailEntity) renderDetail(detailRootFor(kind), detailRoute, detailEntity);
    return;
  }
  loadDetail();
}

function leaveDetail() {
  if (!detailRoute) return;
  detailToken++;
  stopDetailPolling();
  detailRoute = null;
  detailEntity = null;
}

function stopDetailPolling() {
  if (detailTimer) { clearTimeout(detailTimer); detailTimer = null; }
}

function scheduleDetailPoll(ms) {
  stopDetailPolling();
  const token = detailToken;
  detailTimer = setTimeout(() => {
    if (token !== detailToken) return;
    if (document.visibilityState === 'hidden') { scheduleDetailPoll(ms); return; }
    loadDetail();
  }, ms);
}

function detailPollMs(kind, e) {
  if (kind === 'workflow') return e.running ? 3000 : 30000;
  if (kind === 'project') return e.stats && e.stats.active_sessions > 0 ? 10000 : 30000;
  if (e.kind === 'prompt' && !dashIsTemplate(e) && !(e.markdown && e.markdown.trim()) && !e.last_error) return 5000;
  return 15000;
}

async function loadDetail() {
  const route = detailRoute;
  if (!route) return;
  const token = detailToken;
  const root = detailRootFor(route.kind);
  try {
    const entity = await fetchJSON(detailApi(route.kind, route.agent, route.id));
    if (token !== detailToken) return;
    detailEntity = entity;
    if (entity.source === 'gitops' && !gitops[route.kind + 's']) loadGitops(route.kind + 's');
    renderDetail(root, route, entity);
    scheduleDetailPoll(detailPollMs(route.kind, entity));
  } catch (err) {
    if (token !== detailToken) return;
    const gone = err && err.status === 404;
    const off = route.kind === 'project' && err && err.status === 503;
    const parent = DETAIL_PARENT[route.kind];
    root.innerHTML = `<div class="empty-state">
        <div class="empty-state-icon">${gone ? '&#x1f50d;' : '&#x26a0;&#xfe0f;'}</div>
        <p>${gone ? `This ${route.kind} no longer exists.` : off ? 'Projects are not enabled in this deployment.' : `The ${route.kind} could not be loaded (${escapeHtml(err && err.message ? err.message : String(err))}).`}</p>
        <p><a href="/ui/${parent}" data-page="${parent}">Back to ${PAGE_TITLES[parent].toLowerCase()}</a></p>
      </div>`;
    if (!gone) scheduleDetailPoll(30000);
  }
}

function renderDetail(root, route, entity) {
  if (route.kind === 'workflow') renderWorkflowDetail(root, entity);
  else if (route.kind === 'project') renderProjectDetail(root, entity);
  else renderDashboardDetail(root, entity);
}

function refreshDetailIf(kind) {
  if (detailRoute && detailRoute.kind === kind) loadDetail();
}

/* Shared pieces */
function fmtDateTime(iso) {
  if (!iso) return '—';
  const t = new Date(iso);
  return isNaN(t.getTime()) ? String(iso) : t.toLocaleString();
}

function fmtDuration(ms) {
  ms = ms || 0;
  if (ms < 1000) return ms + ' ms';
  if (ms < 60000) return (ms / 1000).toFixed(1) + ' s';
  return `${Math.floor(ms / 60000)}m ${Math.round((ms % 60000) / 1000)}s`;
}

function sourceRefToURL(ref) {
  if (!ref || typeof ref !== 'string') return null;
  const m = ref.match(/^([^/\s]+)\/([^@\s]+)@([^:\s]+):(.+)$/);
  if (!m) return null;
  return 'https://github.com/' + encodeURIComponent(m[1]) + '/' + encodeURIComponent(m[2]) +
    '/blob/' + encodeURIComponent(m[3]) + '/' + m[4].split('/').map(encodeURIComponent).join('/');
}

function detailHeadHtml({ parent, agent, name, description, tags, actions }) {
  return `<div class="page-head detail-head">
      <div class="detail-title">
        <div class="crumbs"><a href="/ui/${parent}" data-page="${parent}">${PAGE_TITLES[parent]}</a><span class="crumb-sep">/</span>${agentChip(agent)}</div>
        <h1 class="page-title">${escapeHtml(name)}</h1>
        ${description ? `<p class="page-desc">${escapeHtml(description)}</p>` : ''}
        <div class="detail-tags">${tags}</div>
      </div>
      <div class="page-actions detail-actions">${actions}</div>
    </div>`;
}

function metaItemHtml(label, value) {
  return `<div class="meta-item"><div class="label">${escapeHtml(label)}</div><div class="value">${escapeHtml(value)}</div></div>`;
}

function gitopsBannerHtml(kind, e) {
  const link = sourceRefToURL(e.source_ref);
  const src = link
    ? `Edit it at <a href="${escapeHtml(link)}" target="_blank" rel="noopener">${escapeHtml(e.source_ref)}</a>.`
    : 'Edits here would be overwritten on the next sync.';
  const line = gitopsHtml(kind);
  return `<div class="detail-banner"><strong>Managed via GitOps.</strong> ${src}${line ? `<div class="gitops-line">${line}</div>` : ''}</div>`;
}

function openDetailsKeys(root, selector) {
  return new Set([...root.querySelectorAll(selector + '[open]')].map(el => el.dataset.key));
}
function reopenDetails(root, selector, keys) {
  root.querySelectorAll(selector).forEach(el => { if (keys.has(el.dataset.key)) el.open = true; });
}

/* Workflow */
function wfPattern(w) {
  const type = (w.trigger && w.trigger.type) || 'schedule';
  if (Array.isArray(w.tasks) && w.tasks.length) return 'subflows';
  if (type === 'on_success' || type === 'on_failure') return 'event-triggered';
  return type === 'manual' ? 'manual' : 'monoflow';
}

function renderWorkflowDetail(root, w) {
  const [cls, label, title] = wfStatus(w);
  const managed = w.source === 'gitops';
  const hasTasks = Array.isArray(w.tasks) && w.tasks.length > 0;
  const trig = w.trigger || {};
  const trigType = trig.type || 'schedule';
  const pattern = wfPattern(w);
  const runs = w.runs || [];
  document.title = `${appTitle} — ${w.name || w.id}`;

  const tags = `<span class="status-pill ${cls}" title="${escapeHtml(title)}">${label}</span>
    <span class="tag">${pattern}</span>
    ${trigType === 'schedule' && w.cron ? `<span class="tag" title="cron, UTC">${escapeHtml(w.cron)} UTC</span>` : ''}
    ${(trigType === 'on_success' || trigType === 'on_failure') ? `<span class="tag">${trigType.replace('_', ' ')} · ${escapeHtml(trig.ref || '')}</span>` : ''}
    ${w.model ? `<span class="tag" title="model override">${escapeHtml(w.model)}</span>` : ''}
    ${managed ? '<span class="tag gitops" title="Managed from git; read-only here">gitops</span>' : ''}
    <span class="tag" title="workflow id">${escapeHtml(w.id)}</span>`;
  const actions = `
    <button class="btn-mini primary" type="button" onclick="wfdRun(this)"${w.running || !w.enabled ? ' disabled' : ''} title="${w.enabled ? 'Trigger this workflow now, outside its schedule.' : 'Resume the workflow before running it.'}">${w.running ? 'Running…' : 'Run now'}</button>
    <button class="btn-mini" type="button" onclick="wfdToggle(this)" title="${w.enabled ? 'Stop ticking and stop reacting to triggers until resumed.' : 'Start ticking and reacting to triggers again.'}">${w.enabled ? 'Pause' : 'Resume'}</button>
    ${managed ? '' : '<button class="btn-mini" type="button" onclick="wfdEdit()">Edit</button>'}
    ${managed ? '' : '<button class="btn-mini danger" type="button" onclick="wfdDelete()">Delete</button>'}
    <a class="btn-mini" href="${detailApi('workflow', w.agent, w.id)}" target="_blank" rel="noopener">JSON</a>`;

  let banners = '';
  if (!w.enabled && w.disabled_reason) banners += `<div class="detail-banner danger"><strong>Auto-disabled.</strong> ${escapeHtml(w.disabled_reason)}</div>`;
  if (managed) banners += gitopsBannerHtml('workflows', w);

  const meta = [
    ['Agent', agentLabel(w.agent)],
    ['Pattern', pattern],
    ['Model', w.model || 'default'],
    ['Cron (UTC)', trigType === 'schedule' ? (w.cron || '—') : '—'],
    ['Trigger', trigType + (trig.ref ? ' · ' + trig.ref : '')],
    ['Enabled', w.enabled ? 'yes' : 'no'],
    ['Created by', w.created_by || '—'],
    ['Created', fmtDateTime(w.created_at)],
    ['Last run', w.last_run ? fmtDateTime(w.last_run) : '—'],
  ];
  const allText = hasTasks ? w.tasks.map(t => (t.name || '') + ' ' + (t.prompt || '')).join('\n') : (w.prompt || '');
  const body = hasTasks
    ? `<section class="panel"><h2>Tasks <small>${plural(w.tasks.length, 'step')}</small></h2><div class="task-pipeline">${w.tasks.map((t, i) => taskRowHtml(t, i, i === w.tasks.length - 1)).join('')}</div></section>`
    : `<section class="panel"><h2>Prompt</h2><pre class="prompt">${escapeHtml(w.prompt || '')}</pre></section>`;

  const opened = openDetailsKeys(root, 'details.run');
  root.innerHTML = detailHeadHtml({ parent: 'workflows', agent: w.agent, name: w.name || w.id, description: w.description, tags, actions })
    + banners
    + `<div class="meta-grid">${meta.map(([k, v]) => metaItemHtml(k, v)).join('')}</div>
    <div class="detail-stack">
      <section class="panel"><h2>Flow <small>${pattern}</small></h2>${renderFlowDiagram(w, detectFlowIntegrations(allText))}</section>
      ${body}
      <section class="panel"><h2>Run history <small>${plural(runs.length, 'run')}</small></h2>${runs.length ? runs.map((r, i) => runHtml(r, i === 0 && !opened.size)).join('') : '<div class="empty">No runs yet.</div>'}</section>
    </div>
    <div class="detail-foot">Refreshed ${escapeHtml(new Date().toLocaleTimeString())} · updates every ${w.running ? '3' : '30'} seconds</div>`;
  reopenDetails(root, 'details.run', opened);
}

function taskRowHtml(t, i, isLast) {
  return `<div class="task-row">
      <div class="task-index"><div class="num">${i + 1}</div>${isLast ? '' : '<div class="bar"></div>'}</div>
      <div class="task-body">
        <div class="task-name">${escapeHtml(t.name)}</div>
        ${taskChipsHtml(t.prompt)}
        <div class="task-prompt">${escapeHtml(t.prompt)}</div>
      </div>
    </div>`;
}

function runHtml(r, open) {
  const ok = !r.error;
  const tasks = Array.isArray(r.tasks) && r.tasks.length
    ? r.tasks.map((t, i) => `<div class="run-task-line${t.error ? ' err' : ''}"><span class="dot"></span>${i + 1}. ${escapeHtml(t.name)} — ${t.error ? 'error' : 'ok'} (${fmtDuration(t.duration_ms)})${t.error ? ' · ' + escapeHtml(t.error) : ''}</div>`).join('')
    : '';
  return `<details class="run" data-key="${escapeHtml(r.started_at || '')}"${open ? ' open' : ''}>
      <summary>
        <span class="run-time">${escapeHtml(fmtDateTime(r.started_at))}</span>
        <span class="run-duration">${escapeHtml(fmtDuration(r.duration_ms))}</span>
        ${r.triggered_by ? `<span class="tag">${escapeHtml(r.triggered_by)}</span>` : ''}
        <span class="status-pill ${ok ? 'ok' : 'failed'}">${ok ? 'ok' : 'error'}</span>
      </summary>
      <div class="run-body">
        ${tasks}
        ${r.error ? `<div class="run-error">${escapeHtml(r.error)}</div>` : ''}
        ${r.result ? `<div class="run-result">${escapeHtml(r.result)}</div>` : ''}
        ${!r.error && !r.result && !tasks ? '<div class="empty">No output recorded.</div>' : ''}
      </div>
    </details>`;
}

async function wfdRun(btn) {
  const w = detailEntity;
  if (!w) return;
  if (!(await uiConfirm('It uses the agent’s LLM tool loop and counts toward usage.', { title: 'Run this workflow now?', okLabel: 'Run now' }))) return;
  btn.disabled = true;
  btn.textContent = 'Queued…';
  try {
    await apiSend(`${detailApi('workflow', w.agent, w.id)}/run`, 'POST', {});
  } catch (err) {
    await uiError('Failed to start workflow', err);
  }
  delete lastFetched.workflows;
  loadWorkflows();
  scheduleDetailPoll(1500);
}

async function wfdToggle(btn) {
  const w = detailEntity;
  if (!w) return;
  btn.disabled = true;
  try {
    await apiSend(detailApi('workflow', w.agent, w.id), 'PATCH', { enabled: !w.enabled });
  } catch (err) {
    await uiError('Failed to update workflow', err);
  }
  delete lastFetched.workflows;
  loadWorkflows();
  await loadDetail();
}

async function wfdDelete() {
  const w = detailEntity;
  if (!w) return;
  if (!(await uiConfirm('This stops its schedule and removes its stored data.', { title: 'Delete this workflow?', tone: 'danger', okLabel: 'Delete' }))) return;
  try {
    await apiSend(detailApi('workflow', w.agent, w.id), 'DELETE');
  } catch (err) {
    await uiError('Failed to delete workflow', err);
    return;
  }
  delete lastFetched.workflows;
  navigate('workflows');
}

/* Workflow editor, in the shared form panel */
let wfdEditTasks = [];

function wfdTasksHtml() {
  if (!wfdEditTasks.length) return '<div class="empty">No tasks yet. Add one below.</div>';
  return wfdEditTasks.map((t, i) => `<div class="task-edit" data-idx="${i}">
      <div class="task-head"><div class="num">${i + 1}</div><input class="form-input wfd-task-name" placeholder="Task name" value="${escapeHtml(t.name || '')}"><button type="button" class="btn-mini danger wfd-remove-task" data-idx="${i}">Remove</button></div>
      <textarea class="form-textarea wfd-task-prompt" placeholder="Task prompt">${escapeHtml(t.prompt || '')}</textarea>
    </div>`).join('');
}

function wfdSyncTasks() {
  document.querySelectorAll('#wfd-tasks .task-edit').forEach(row => {
    const t = wfdEditTasks[+row.dataset.idx];
    if (!t) return;
    t.name = row.querySelector('.wfd-task-name').value;
    t.prompt = row.querySelector('.wfd-task-prompt').value;
  });
}

function wfdSetMode(mode) {
  document.querySelectorAll('.wfd-mode').forEach(b => b.classList.toggle('active', b.dataset.mode === mode));
  document.getElementById('wfd-prompt-row').hidden = mode !== 'prompt';
  document.getElementById('wfd-tasks-row').hidden = mode !== 'tasks';
}

function wfdEdit() {
  const w = detailEntity;
  if (!w) return;
  const trig = w.trigger || {};
  wfdEditTasks = Array.isArray(w.tasks) ? w.tasks.map(t => ({ name: t.name || '', prompt: t.prompt || '' })) : [];
  const mode = wfdEditTasks.length ? 'tasks' : 'prompt';
  const triggers = ['schedule', 'on_success', 'on_failure', 'manual'];
  openForm({
    title: 'Edit workflow',
    subtitle: w.name || w.id,
    saveLabel: 'Save changes',
    html: formField('Name', `<input class="form-input" id="wfd-name" maxlength="120" value="${escapeHtml(w.name || '')}">`)
      + formField('Description', `<input class="form-input" id="wfd-desc" value="${escapeHtml(w.description || '')}">`)
      + `<div class="form-grid2">`
      + formField('Trigger', `<select class="form-input" id="wfd-trigger">${triggers.map(t => `<option value="${t}"${(trig.type || 'schedule') === t ? ' selected' : ''}>${t}</option>`).join('')}</select>`)
      + formField('Trigger ref', `<input class="form-input" id="wfd-ref" value="${escapeHtml(trig.ref || '')}" placeholder="agent/workflow-id">`, 'For on_success and on_failure: the upstream workflow.')
      + `</div>`
      + formField('Cron (UTC)', `<input class="form-input" id="wfd-cron" value="${escapeHtml(w.cron || '')}" placeholder="0 5 * * *, */15 * * * *, @every 1h">`, 'Scheduled workflows only.')
      + `<label class="check-row"><input type="checkbox" id="wfd-enabled"${w.enabled ? ' checked' : ''}> Enabled</label>`
      + formField('Body', `<div class="mode-switch"><button type="button" class="wfd-mode${mode === 'prompt' ? ' active' : ''}" data-mode="prompt">Monoflow prompt</button><button type="button" class="wfd-mode${mode === 'tasks' ? ' active' : ''}" data-mode="tasks">Subflow tasks</button></div>`)
      + `<div id="wfd-prompt-row"${mode === 'tasks' ? ' hidden' : ''}>${formField('Prompt', `<textarea class="form-textarea" id="wfd-prompt">${escapeHtml(w.prompt || '')}</textarea>`)}</div>`
      + `<div id="wfd-tasks-row"${mode === 'prompt' ? ' hidden' : ''}>${formField('Tasks', `<div class="tasks-editor" id="wfd-tasks">${wfdTasksHtml()}</div><div><button type="button" class="btn-mini" id="wfd-add-task">Add task</button></div>`, 'Tasks run in order; each output is fed into the next prompt.')}</div>`,
    onSave: async () => {
      const body = {
        name: formValue('wfd-name'),
        description: document.getElementById('wfd-desc').value,
        enabled: document.getElementById('wfd-enabled').checked,
        trigger: { type: formValue('wfd-trigger'), ref: formValue('wfd-ref') },
      };
      const cron = formValue('wfd-cron');
      if (cron) body.cron = cron;
      if (!body.name) throw new Error('Name is required.');
      const active = document.querySelector('.wfd-mode.active');
      if (active && active.dataset.mode === 'tasks') {
        wfdSyncTasks();
        const cleaned = wfdEditTasks.map(t => ({ name: t.name.trim(), prompt: t.prompt.trim() })).filter(t => t.name || t.prompt);
        if (!cleaned.length) throw new Error('At least one task is required in subflow mode.');
        cleaned.forEach((t, i) => { if (!t.name || !t.prompt) throw new Error(`Task ${i + 1} needs both a name and a prompt.`); });
        body.tasks = cleaned;
        body.prompt = '';
      } else {
        body.prompt = document.getElementById('wfd-prompt').value;
        body.tasks = [];
        if (!body.prompt.trim()) throw new Error('Prompt is required in monoflow mode.');
      }
      await apiSend(detailApi('workflow', w.agent, w.id), 'PATCH', body);
      delete lastFetched.workflows;
      loadWorkflows();
      await loadDetail();
    },
  });
}

document.getElementById('form-body').addEventListener('click', e => {
  const mode = e.target.closest('.wfd-mode');
  if (mode) { wfdSetMode(mode.dataset.mode); return; }
  if (e.target.closest('#wfd-add-task')) {
    wfdSyncTasks();
    wfdEditTasks.push({ name: '', prompt: '' });
    document.getElementById('wfd-tasks').innerHTML = wfdTasksHtml();
    return;
  }
  const rm = e.target.closest('.wfd-remove-task');
  if (rm) {
    wfdSyncTasks();
    wfdEditTasks.splice(+rm.dataset.idx, 1);
    document.getElementById('wfd-tasks').innerHTML = wfdTasksHtml();
  }
});

/* Flow diagram: integrations a prompt touches, drawn trigger → read → agent → write */
const FLOW_INTEGRATIONS = [
  { id: 'jira', name: 'Jira', kind: 'source', keys: ['jira', 'jql', 'ticket', 'issue', 'project ='] },
  { id: 'confluence', name: 'Confluence', kind: 'source', keys: ['confluence', 'cql', 'search_confluence_pages', 'get_confluence_page', 'list_confluence_spaces', 'create_confluence_page'] },
  { id: 'github', name: 'GitHub', kind: 'both', keys: ['github', 'gh', 'repo', 'repository', 'pull request', 'pr ', 'pull-request', 'branch', 'commit', '.github'],
    readKeys: ['get_file_content', 'get_files_bulk', 'list_pull_requests', 'search_pull_requests', 'get_pull_request', 'list_commits', 'search_code', 'search_code_org', 'search_files', 'list_directory', 'list_org_repos', 'list_user_repos', 'list_repo_teams', 'resolve_owner', 'get_repo_default_branch', 'get_authenticated_user', 'get_workflow_run'],
    writeKeys: ['modify_file', 'create_file', 'delete_file', 'move_file', 'regex_replace_file', 'rerun_failed_jobs', 'rerun_workflow'] },
  { id: 'slack', name: 'Slack', kind: 'output', keys: ['slack', 'channel', 'c0', 'u0', 'post_slack', 'dm '] },
  { id: 'datadog', name: 'Datadog', kind: 'source', keys: ['datadog', 'datadog_search_logs', 'datadog_list_monitors', 'datadog_get_monitor', 'datadog_list_hosts', 'datadog_get_dashboard', 'datadog_list_dashboards', 'datadog_query_metrics', 'dd_', 'env:prod'] },
  { id: 'aws', name: 'AWS', kind: 'both', keys: ['aws_get_cost_and_usage', 'aws_get_cost_forecast', 'aws_list_dimension_values', 'aws_s3_put_object', 'aws_s3_get_object', 'aws_s3_list_objects', 'aws_athena_query', 'aws_athena_schema', 'aws_athena_catalogs', 'cost explorer', 'athena', 'amortizedcost', 'unblendedcost', 's3://'],
    readKeys: ['aws_get_cost_and_usage', 'aws_get_cost_forecast', 'aws_list_dimension_values', 'aws_s3_get_object', 'aws_s3_list_objects', 'aws_athena_query', 'aws_athena_schema', 'aws_athena_catalogs', 'cost explorer', 'athena', 'amortizedcost', 'unblendedcost'],
    writeKeys: ['aws_s3_put_object'] },
  { id: 'azure', name: 'Azure', kind: 'source', keys: ['azure_get_cost_and_usage', 'azure_get_cost_forecast', 'azure_list_dimension_values', 'cost management', 'actualcost', 'servicename', 'resourcegroup'] },
  { id: 'databricks', name: 'Databricks', kind: 'source', keys: ['databricks_query', 'databricks', 'system.billing', 'account_prices', 'usage_quantity', 'sku_name', 'dbu'] },
  { id: 'clickhouse', name: 'ClickHouse', kind: 'source', keys: ['clickhouse_usage_cost', 'clickhouse_query', 'clickhouse', 'usagecost', 'clickpipe'] },
  { id: 'salesforce', name: 'Salesforce', kind: 'source', keys: ['salesforce', 'sfdc', 'soql', 'salesforce_query', 'sf_query'] },
  { id: 'chorus', name: 'Chorus', kind: 'source', keys: ['chorus', 'call intelligence', 'zoominfo'] },
  { id: 'freshworks', name: 'Freshworks', kind: 'source', keys: ['freshdesk_list_tickets', 'freshdesk_get_ticket', 'freshdesk_search_tickets', 'freshdesk_find_agent', 'freshchat_get_conversation', 'freshchat_get_conversation_messages', 'freshworks_crm_search', 'freshworks_crm_get_contact', 'freshworks_crm_get_deal', 'freshdesk', 'freshchat', 'freshworks', 'freshsales'] },
  { id: 'google', name: 'Google Drive', kind: 'both', keys: ['drive_list_folders', 'drive_find_file', 'drive_read_file', 'sheets_read_range', 'sheets_get_spreadsheet_info', 'sheets_append_row', 'google sheet', 'google drive', 'spreadsheet'],
    readKeys: ['drive_list_folders', 'drive_find_file', 'drive_read_file', 'sheets_read_range', 'sheets_get_spreadsheet_info'],
    writeKeys: ['sheets_append_row'] },
  { id: 'nvd', name: 'NVD', kind: 'source', keys: ['nvd', 'cve', 'vulnerabilit'] },
  { id: 'document360', name: 'Document360', kind: 'source', keys: ['document360_list_workspaces', 'document360_search', 'document360_list_categories', 'document360_list_articles', 'document360_get_article', 'document360', 'knowledge base article', 'help center article'] },
  { id: 'workflow', name: 'Workflow', kind: 'both', keys: ['call_workflow', 'sub-workflow', 'child workflow'] },
];

const FLOW_SVGS = {
  workflow: '<svg viewBox="0 0 32 32" xmlns="http://www.w3.org/2000/svg"><circle cx="8" cy="8" r="4" fill="#8f87b8"/><circle cx="24" cy="24" r="4" fill="#7297bd"/><path stroke="#7f8a9b" stroke-width="2" fill="none" d="M8 12v8c0 2 2 4 4 4h8"/></svg>',
  clock: '<svg viewBox="0 0 32 32" xmlns="http://www.w3.org/2000/svg"><circle cx="16" cy="16" r="12" fill="none" stroke="#8f87b8" stroke-width="2"/><path d="M16 8v8l5 3" stroke="#8f87b8" stroke-width="2" fill="none" stroke-linecap="round"/></svg>',
  bolt: '<svg viewBox="0 0 32 32" xmlns="http://www.w3.org/2000/svg"><path fill="#c79a5e" d="M18 3 6 19h8l-2 10 12-16h-8z"/></svg>',
  hand: '<svg viewBox="0 0 32 32" xmlns="http://www.w3.org/2000/svg"><path fill="#7f8a9b" d="M10 14V6a2 2 0 1 1 4 0v7h1V4a2 2 0 1 1 4 0v9h1V6a2 2 0 1 1 4 0v10l-1 8c-.5 3.2-3 4-6 4h-4c-3 0-4-1-5-3l-4-8a2 2 0 1 1 3-2z"/></svg>',
  agent: '<svg viewBox="0 0 32 32" xmlns="http://www.w3.org/2000/svg"><circle cx="16" cy="12" r="6" fill="#7297bd"/><path d="M6 28c0-5.5 4.5-10 10-10s10 4.5 10 10" fill="#7297bd"/></svg>',
  arrow: '<svg viewBox="0 0 18 32" xmlns="http://www.w3.org/2000/svg"><path d="M9 2v24m0 0-5-5m5 5 5-5" stroke="currentColor" stroke-width="1.5" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>',
  generic: '<svg viewBox="0 0 32 32" xmlns="http://www.w3.org/2000/svg"><rect x="6" y="6" width="20" height="20" rx="5" fill="none" stroke="#7f8a9b" stroke-width="2"/></svg>',
};

function flowLogo(id) { return getIntegrationLogo(id) || FLOW_SVGS[id] || FLOW_SVGS.generic; }

const FLOW_KEY_RE = new Map();
function flowKeyMatcher(key) {
  if (FLOW_KEY_RE.has(key)) return FLOW_KEY_RE.get(key);
  const re = /^[a-z0-9]+$/i.test(key) ? new RegExp('\\b' + key.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '\\b', 'i') : null;
  FLOW_KEY_RE.set(key, re);
  return re;
}

function detectFlowIntegrations(text) {
  if (!text) return [];
  const lower = text.toLowerCase();
  const matches = keys => (keys || []).some(k => { const re = flowKeyMatcher(k); return re ? re.test(text) : lower.includes(k.toLowerCase()); });
  const out = [];
  for (const integ of FLOW_INTEGRATIONS) {
    if (!matches(integ.keys)) continue;
    let kind = integ.kind;
    if (kind === 'both' && (integ.readKeys || integ.writeKeys)) {
      const r = matches(integ.readKeys);
      const w = matches(integ.writeKeys);
      if (r && !w) kind = 'source';
      else if (w && !r) kind = 'output';
    }
    out.push({ ...integ, kind });
  }
  return out;
}

function flowNodeHtml(kind, iconSvg, name, sub) {
  return `<div class="flow-node ${kind}"><div class="icon">${iconSvg}</div><div class="name">${escapeHtml(name)}</div>${sub ? `<div class="sub">${escapeHtml(sub)}</div>` : ''}</div>`;
}

function flowLaneHtml(label, nodes) {
  return `<div><div class="flow-lane-label">${escapeHtml(label)}</div><div class="flow-row">${nodes}</div></div>`;
}

function renderFlowDiagram(w, integrations) {
  const type = (w.trigger && w.trigger.type) || 'schedule';
  let trigger;
  if (type === 'schedule') trigger = flowNodeHtml('trigger', FLOW_SVGS.clock, 'Schedule', 'cron ' + (w.cron || '—') + ' UTC');
  else if (type === 'on_success' || type === 'on_failure') trigger = flowNodeHtml('trigger', FLOW_SVGS.bolt, type.replace('_', ' '), w.trigger.ref);
  else trigger = flowNodeHtml('trigger', FLOW_SVGS.hand, 'Manual', 'run from here or via call_workflow');
  const sources = integrations.filter(i => i.kind === 'source' || i.kind === 'both');
  const outputs = integrations.filter(i => i.kind === 'output' || i.kind === 'both');
  const arrow = `<div class="flow-connector">${FLOW_SVGS.arrow}</div>`;
  let html = '<div class="flow">' + flowLaneHtml('Trigger', trigger) + arrow;
  html += flowLaneHtml('Read from', sources.length ? sources.map(i => flowNodeHtml('source', flowLogo(i.id), i.name)).join('') : '<div class="flow-empty">No integrations detected in the prompt.</div>') + arrow;
  html += flowLaneHtml('Agent', flowNodeHtml('agent', FLOW_SVGS.agent, agentLabel(w.agent), 'LLM tool loop'));
  if (outputs.length) html += arrow + flowLaneHtml('Write to', outputs.map(i => flowNodeHtml('output', flowLogo(i.id), i.name)).join(''));
  return html + '</div>';
}

function taskChipsHtml(prompt) {
  const integs = detectFlowIntegrations(prompt);
  if (!integs.length) return '';
  return `<div class="task-chips">${integs.map(i => `<span class="task-chip">${flowLogo(i.id)}${escapeHtml(i.name)}</span>`).join('')}</div>`;
}

/* Dashboard */
function dashIsTemplate(d) { return d.kind === 'prompt' && !d.template_id && promptInputs(d.prompt).length > 0; }

function renderDashboardDetail(root, d) {
  const [cls, label, title] = dashStatus(d);
  const managed = d.source === 'gitops';
  const isPrompt = d.kind === 'prompt';
  const isInstance = !!d.template_id;
  const isTemplate = dashIsTemplate(d);
  const hasReport = isPrompt && !!(d.markdown && d.markdown.trim());
  const key = d.agent + '/' + d.id;
  document.title = `${appTitle} — ${d.name || d.id}`;

  const kindLabel = isInstance ? 'rendered report' : isTemplate ? 'prompt template' : isPrompt ? 'prompt report' : plural((d.sources || []).length, 'source');
  const tags = `<span class="status-pill ${cls}" title="${escapeHtml(title)}">${label}</span>
    <span class="tag">${escapeHtml(kindLabel)}</span>
    <span class="tag" title="refresh interval">every ${escapeHtml(d.sync_interval || '—')}</span>
    ${d.last_sync ? `<span class="tag" title="${escapeHtml(fmtDateTime(d.last_sync))}">synced ${timeAgo(d.last_sync)}</span>` : ''}
    ${managed ? '<span class="tag gitops" title="Managed from git; read-only here">gitops</span>' : ''}
    <span class="tag" title="dashboard id">${escapeHtml(d.id)}</span>`;
  const actions = `
    ${isInstance ? '<button class="btn-mini primary" type="button" onclick="ddRerender(this)" title="Rebuild this report now with its saved inputs and the current template prompt.">Re-render</button>' : ''}
    ${hasReport ? '<button class="btn-mini" type="button" onclick="window.print()" title="Print or save the report as PDF.">PDF</button>' : ''}
    ${managed ? '' : '<button class="btn-mini danger" type="button" onclick="ddDelete()">Delete</button>'}
    <a class="btn-mini" href="${detailApi('dashboard', d.agent, d.id)}" target="_blank" rel="noopener">JSON</a>`;

  let banners = managed ? gitopsBannerHtml('dashboards', d) : '';
  if (d.last_error && !isPrompt) banners += `<div class="detail-banner danger"><strong>Last sync failed</strong>${d.last_sync ? ' (' + escapeHtml(fmtDateTime(d.last_sync)) + ')' : ''}: ${escapeHtml(d.last_error)}</div>`;
  const head = detailHeadHtml({ parent: 'dashboards', agent: d.agent, name: d.name || d.id, description: d.description, tags, actions }) + banners;
  const promptPanel = isPrompt && d.prompt
    ? `<details class="panel prompt-panel" data-key="prompt"><summary>Prompt</summary><pre class="prompt">${escapeHtml(d.prompt)}</pre></details>`
    : '';

  if (isTemplate) {
    if (root.dataset.rendered === key && root.querySelector('#dd-form')) {
      root.querySelector('#dd-head').innerHTML = head;
      ddLoadInstances(d);
      return;
    }
    root.dataset.rendered = key;
    const inputs = promptInputs(d.prompt);
    root.innerHTML = `<div id="dd-head">${head}</div>${promptPanel}
      <section class="panel">
        <h2>Render this dashboard</h2>
        <p class="panel-note">Fill in the inputs to render a live report. It refreshes on its own every ${escapeHtml(d.sync_interval || '5m')}.</p>
        <form id="dd-form" class="dd-form" autocomplete="off" onsubmit="event.preventDefault(); ddRender(this)">
          ${inputs.map(n => formField(escapeHtml(n), `<input class="form-input" data-input="${escapeHtml(n)}" placeholder="${escapeHtml(n)}">`)).join('')}
          ${formField('Refresh interval', `<input class="form-input" id="dd-interval" placeholder="${escapeHtml(d.sync_interval || '5m')}">`, 'Optional, for example 5m or 1h.')}
          <div class="form-actions"><button class="btn-mini primary" type="submit">Render</button><span class="form-error" id="dd-status"></span></div>
        </form>
      </section>
      <section class="panel"><h2>Rendered instances <small id="dd-inst-meta"></small></h2><div id="dd-instances"><div class="empty">Loading…</div></div></section>`;
    ddLoadInstances(d);
    return;
  }

  const opened = openDetailsKeys(root, 'details.prompt-panel');
  root.dataset.rendered = key;
  if (isPrompt) {
    let reportHead = '';
    if (isInstance) {
      const chips = Object.keys(d.inputs || {}).sort().map(k => `<span class="input-chip"><b>${escapeHtml(k)}</b>: ${escapeHtml(d.inputs[k])}</span>`).join('');
      reportHead = `<div class="report-inputs">${chips}<a class="report-tmpl" href="${detailPath('dashboard', d.agent, d.template_id)}" data-link>Template</a></div>`;
    }
    const errBanner = d.last_error
      ? `<div class="render-error"><strong>Last render failed</strong>${d.last_sync ? ' (' + escapeHtml(fmtDateTime(d.last_sync)) + ')' : ''}: ${escapeHtml(d.last_error)}${hasReport ? ' The previous successful report is shown below.' : ''}</div>`
      : '';
    const body = hasReport
      ? renderMarkdownDoc(d.markdown)
      : (d.last_error ? '' : '<div class="empty">Rendering… this can take up to a minute. The page refreshes on its own.</div>');
    root.innerHTML = head + promptPanel + `<article class="report">${reportHead}${errBanner}${body}</article>`;
    reopenDetails(root, 'details.prompt-panel', opened);
    return;
  }

  const cards = (d.sources || []).map((s, i) => {
    const name = s.name || (s.type + '_' + i);
    const res = (d.data || {})[name] || {};
    const meta = (res.fetched_at ? fmtDateTime(res.fetched_at) : 'pending') + (res.duration_ms != null ? ' · ' + fmtDuration(res.duration_ms) : '');
    return `<section class="panel source"><h2><span>${escapeHtml(name)}</span><span class="tag">${escapeHtml(s.type)}</span><small>${escapeHtml(meta)}</small></h2>${res.error ? `<div class="source-error">${escapeHtml(res.error)}</div>` : renderSourceContent(res.content)}</section>`;
  }).join('');
  root.innerHTML = head + `<div class="detail-stack">${cards || '<div class="empty tall">This dashboard has no sources.</div>'}</div>
    <div class="detail-foot">Refreshed ${escapeHtml(new Date().toLocaleTimeString())} · updates every 15 seconds</div>`;
}

async function ddLoadInstances(d) {
  const box = document.getElementById('dd-instances');
  if (!box) return;
  try {
    const list = await fetchJSON(`/api/dashboards?agent=${encodeURIComponent(d.agent)}`);
    const insts = (list || []).filter(x => x.template_id === d.id).sort((a, b) => (a.name || '').localeCompare(b.name || ''));
    const meta = document.getElementById('dd-inst-meta');
    if (meta) meta.textContent = plural(insts.length, 'instance');
    if (!insts.length) { box.innerHTML = '<div class="empty">None yet. Render one above.</div>'; return; }
    box.innerHTML = insts.map(x => {
      const label = x.inputs && Object.keys(x.inputs).length
        ? Object.keys(x.inputs).sort().map(k => escapeHtml(x.inputs[k])).join(' · ')
        : escapeHtml(x.name || x.id);
      const [icls, ilabel] = dashStatus(x);
      return `<a class="inst-row" href="${detailPath('dashboard', x.agent, x.id)}" data-link><span class="inst-label">${label}</span><span class="inst-when"><span class="status-pill ${icls}">${ilabel}</span>${x.last_sync ? timeAgo(x.last_sync) : 'pending'}</span></a>`;
    }).join('');
  } catch (err) {
    box.innerHTML = `<div class="empty">Could not load instances: ${escapeHtml(err && err.message ? err.message : String(err))}</div>`;
  }
}

async function ddRender(form) {
  const d = detailEntity;
  if (!d) return;
  const status = document.getElementById('dd-status');
  const inputs = {};
  form.querySelectorAll('input[data-input]').forEach(el => { inputs[el.dataset.input] = el.value.trim(); });
  const missing = Object.keys(inputs).filter(k => !inputs[k]);
  if (missing.length) { status.textContent = 'Fill in: ' + missing.join(', '); return; }
  const btn = form.querySelector('button[type=submit]');
  btn.disabled = true;
  status.textContent = '';
  try {
    const res = await apiSend(`${detailApi('dashboard', d.agent, d.id)}/render`, 'POST', { inputs, interval: formValue('dd-interval') });
    delete lastFetched.dashboards;
    loadDashboards();
    routeTo(detailPath('dashboard', d.agent, res.id));
  } catch (err) {
    status.textContent = 'Render failed: ' + (err.message || err);
    btn.disabled = false;
  }
}

async function ddRerender(btn) {
  const d = detailEntity;
  if (!d || !d.template_id) return;
  btn.disabled = true;
  btn.textContent = 'Re-rendering…';
  try {
    await apiSend(`${detailApi('dashboard', d.agent, d.template_id)}/render`, 'POST', { inputs: d.inputs || {}, interval: d.sync_interval || '' });
    scheduleDetailPoll(3000);
  } catch (err) {
    await uiError('Re-render failed', err);
    btn.disabled = false;
    btn.textContent = 'Re-render';
  }
}

async function ddDelete() {
  const d = detailEntity;
  if (!d) return;
  if (!(await uiConfirm('This stops its sync and removes its stored data.', { title: 'Delete this dashboard?', tone: 'danger', okLabel: 'Delete' }))) return;
  try {
    await apiSend(detailApi('dashboard', d.agent, d.id), 'DELETE');
  } catch (err) {
    await uiError('Failed to delete dashboard', err);
    return;
  }
  delete lastFetched.dashboards;
  navigate('dashboards');
}

/* Project */
const PJD_TASK_STATUS = {
  dispatching: ['running', 'dispatching'],
  running: ['running', 'running'],
  pr_open: ['open', 'PR open'],
  closed: ['failed', 'closed'],
  failed: ['failed', 'failed'],
};
const PJD_OUTCOMES = { fixed: 'fixed', not_reproducible: 'not reproducible', cannot_fix: 'cannot fix', already_fixed: 'already fixed' };
const PJD_NOTE_SOURCES = ['admin', 'session', 'review'];
const PJD_LIMITS = [
  ['max_open_prs', 'Open pull requests', 1, 20, 3, 'Counts running sessions too'],
  ['max_active_sessions', 'Sessions at once', 1, 5, 1],
  ['max_sessions_per_day', 'Sessions per day', 1, 48, 6, 'UTC day'],
  ['max_attempts', 'Attempts per error', 1, 5, 2],
  ['cooldown_hours', 'Cooldown, hours', 1, 720, 72, 'After a closed PR or no fix'],
  ['session_timeout_minutes', 'Session timeout, minutes', 30, 600, 240],
  ['recurrence_grace_hours', 'Recurrence grace, hours', 0, 168, 24, 'Deploy lag after a merge'],
  ['recurrence_window_hours', 'Recurrence window, hours', 24, 720, 168, 'Quiet this long counts as resolved'],
];
const pjdExpanded = new Set();

function pjdTaskStatus(t) {
  const o = t.outcome || {};
  if (t.status === 'merged') {
    if (t.recurrence === 'resolved') return ['ok', 'resolved', 'Merged, and the error stayed quiet through the recurrence window'];
    if (t.recurrence === 'recurred') return ['failed', 'recurred', 'Merged, but the error came back after the grace period'];
    return ['ok', 'merged', 'Merged; watching whether the error comes back'];
  }
  if (t.status === 'no_fix') return ['paused', PJD_OUTCOMES[o.status] || 'no fix', o.summary || ''];
  const [cls, label] = PJD_TASK_STATUS[t.status] || ['paused', t.status || 'unknown'];
  return [cls, label, t.error || o.summary || ''];
}

function pjdChips(facts) {
  const shown = facts.filter(f => f[1]);
  return shown.length ? `<div class="chip-row">${shown.map(([k, v]) => `<span class="chip">${escapeHtml(k)} <b class="key">${escapeHtml(v)}</b></span>`).join('')}</div>` : '';
}

function pjdGroupCell(g, key) {
  return `<button type="button" class="proj-toggle" data-key="${escapeHtml(key)}" aria-expanded="${pjdExpanded.has(key)}">${escapeHtml(g.kind || 'error')}</button>${g.service ? ` <span class="tag">${escapeHtml(g.service)}</span>` : ''}
        <span class="sub" title="${escapeHtml(g.pattern || '')}">${escapeHtml(g.pattern || g.fingerprint || '')}</span>`;
}

function pjdGroupDetailHtml(g) {
  const samples = (g.samples || []).map(s => `<div class="proj-label">Sample${s.at ? ' · ' + escapeHtml(fmtDateTime(s.at)) : ''}</div>
      <pre class="run-result">${escapeHtml([s.message, s.stack].filter(Boolean).join('\n\n'))}</pre>`).join('');
  return pjdChips([
    ['fingerprint', g.fingerprint], ['service', g.service], ['kind', g.kind], ['frame', g.frame],
    ['first seen', g.first_seen && fmtDateTime(g.first_seen)], ['last seen', g.last_seen && fmtDateTime(g.last_seen)],
  ]) + (g.pattern ? `<div class="proj-label">Message pattern</div><div class="run-result">${escapeHtml(g.pattern)}</div>` : '') + samples;
}

function pjdTaskDetailHtml(t) {
  const o = t.outcome;
  const pr = t.pr || {};
  let html = pjdChips([
    ['task', t.id], ['branch', t.branch || (o && o.branch)], ['session', t.session_id], ['exit', t.exit_reason],
    ['dispatched', t.dispatched_at && fmtDateTime(t.dispatched_at)], ['finished', t.finished_at && fmtDateTime(t.finished_at)],
    ['lines', t.pr && (pr.additions || pr.deletions) ? `+${fmtInt(pr.additions)} −${fmtInt(pr.deletions)}` : ''],
    ['learnings', t.learnings ? fmtInt(t.learnings) : ''],
  ]);
  if (t.error) html += `<div class="run-error">${escapeHtml(t.error)}</div>`;
  if (o && o.summary) {
    const head = [PJD_OUTCOMES[o.status] || o.status, o.reported_at && fmtDateTime(o.reported_at)].filter(Boolean).map(escapeHtml).join(' · ');
    html += `<div class="proj-label">Session report${head ? ' · ' + head : ''}</div>${o.title ? `<div class="proj-title">${escapeHtml(o.title)}</div>` : ''}<div class="run-result">${escapeHtml(o.summary)}</div>`;
  }
  if (o && o.testing) html += `<div class="proj-label">Testing</div><div class="run-result">${escapeHtml(o.testing)}</div>`;
  return html + `<div class="proj-label">Error group</div>${pjdGroupDetailHtml(t.group || {})}`;
}

function pjdTaskRowsHtml(p, t) {
  const key = 't:' + (t.id || '');
  const [cls, label, title] = pjdTaskStatus(t);
  const g = t.group || {};
  const session = safeExternalUrl(t.session_url);
  const maxAttempts = (p.limits || {}).max_attempts;
  let prCell = '<span class="muted">—</span>';
  if (t.pr) {
    const n = Number(t.pr.number) || 0;
    const url = safeExternalUrl(t.pr.url);
    const text = n ? '#' + n : 'PR';
    prCell = `${url ? `<a href="${escapeHtml(url)}" target="_blank" rel="noopener" title="${escapeHtml(t.pr.title || '')}">${text}</a>` : text} <span class="tag">${escapeHtml(t.pr.state || '')}</span>`;
  }
  return `<tr>
      <td><span class="status-pill ${cls}" title="${escapeHtml(title)}">${escapeHtml(label)}</span></td>
      <td>${pjdGroupCell(g, key)}</td>
      <td class="n">${fmtInt(g.count)}</td>
      <td class="n">${fmtInt(t.attempt)}${maxAttempts ? ' / ' + fmtInt(maxAttempts) : ''}</td>
      <td>${session ? `<a href="${escapeHtml(session)}" target="_blank" rel="noopener">Open</a>` : '<span class="muted">—</span>'}</td>
      <td>${prCell}</td>
      <td class="muted" title="${escapeHtml(t.updated_at ? fmtDateTime(t.updated_at) : '')}">${t.updated_at ? timeAgo(t.updated_at) : '—'}</td>
    </tr>
    <tr class="proj-more"${pjdExpanded.has(key) ? '' : ' hidden'}><td colspan="7">${pjdTaskDetailHtml(t)}</td></tr>`;
}

function pjdBacklogRowsHtml(c) {
  const g = (c && c.group) || {};
  const key = 'g:' + (g.fingerprint || '');
  const when = iso => `<td class="muted" title="${escapeHtml(iso ? fmtDateTime(iso) : '')}">${iso ? timeAgo(iso) : '—'}</td>`;
  return `<tr>
      <td>${pjdGroupCell(g, key)}</td>
      <td class="n">${fmtInt(g.count)}</td>
      ${when(g.first_seen)}
      ${when(g.last_seen)}
    </tr>
    <tr class="proj-more"${pjdExpanded.has(key) ? '' : ' hidden'}><td colspan="4">${pjdGroupDetailHtml(g)}</td></tr>`;
}

function pjdStatsHtml(s) {
  const decided = (s.prs_merged || 0) + (s.prs_closed || 0);
  const settled = (s.resolved || 0) + (s.recurred || 0);
  const sessions = [
    s.active_sessions ? `${fmtInt(s.active_sessions)} running` : '',
    s.failed_sessions ? `${fmtInt(s.failed_sessions)} failed` : '',
    s.no_fix ? `${fmtInt(s.no_fix)} no fix` : '',
  ].filter(Boolean).join(' · ');
  const tiles = [
    ['Sessions', fmtInt(s.sessions), sessions],
    ['PRs opened', fmtInt(s.prs_opened), s.prs_open ? `${fmtInt(s.prs_open)} open now` : ''],
    ['Merged', fmtInt(s.prs_merged), s.lines_changed ? `${fmtInt(s.lines_changed)} lines changed` : ''],
    ['Closed unmerged', fmtInt(s.prs_closed), ''],
    ['Merge rate', projRate(s.merge_rate, decided), decided ? `of ${plural(decided, 'decided PR')}` : 'no PR decided yet'],
    ['Median time to merge', fmtHours(s.median_merge_hours), s.median_session_minutes ? `median session ${fmtHours(s.median_session_minutes / 60)}` : ''],
    ['Resolved', fmtInt(s.resolved), 'error stayed quiet'],
    ['Recurred', fmtInt(s.recurred), 'error came back'],
    ['Resolution rate', projRate(s.resolution_rate, settled), settled ? `of ${fmtInt(settled)} settled` : 'no fix settled yet'],
    ['Backlog', fmtInt(s.backlog), s.error_groups ? `${plural(s.error_groups, 'error group')} tracked` : ''],
  ];
  return `<div class="stats proj-stats">${tiles.map(([k, v, note]) => `<div class="stat"><div class="k">${escapeHtml(k)}</div><div class="v">${escapeHtml(v)}</div>${note ? `<div class="proj-stat-note">${escapeHtml(note)}</div>` : ''}</div>`).join('')}</div>`;
}

function pjdChartHtml(s) {
  const days = fillDays((s.daily || []).map(d => ({ ...d, key: d.date })), 30, key => ({ key, opened: 0, merged: 0 }));
  const max = Math.max(1, ...days.map(d => Math.max(d.opened || 0, d.merged || 0)));
  const height = v => (v ? Math.max(4, v / max * 100).toFixed(1) : 0);
  const total = k => days.reduce((n, d) => n + (d[k] || 0), 0);
  if (!total('opened') && !total('merged')) {
    return `<section class="panel proj-chart-panel"><h2>Pull requests by day <small>last 30 days</small></h2>${emptyHtml('No pull request was opened or merged in the last 30 days.')}</section>`;
  }
  const cols = days.map(d => `<div class="proj-day"><i class="proj-o" style="height:${height(d.opened)}%"></i><i class="proj-m" style="height:${height(d.merged)}%"></i><span>${escapeHtml(d.key)} · ${fmtInt(d.opened)} opened · ${fmtInt(d.merged)} merged</span></div>`).join('');
  return `<section class="panel proj-chart-panel">
      <h2>Pull requests by day <small>last 30 days · ${fmtInt(total('opened'))} opened · ${fmtInt(total('merged'))} merged</small>
        <span class="proj-legend"><span><i class="proj-o"></i>Opened</span><span><i class="proj-m"></i>Merged</span></span></h2>
      <div class="proj-bars">${cols}</div>
      <div class="axis"><span>${escapeHtml(days[0].key)}</span><span>${escapeHtml(days[days.length - 1].key)}</span></div>
    </section>`;
}

function pjdMetaHtml(p) {
  const r = p.repo || {}, sig = p.signal || {}, d = p.dispatch || {}, l = p.limits || {};
  const repoUrl = r.owner && r.name ? safeExternalUrl(`https://github.com/${encodeURIComponent(r.owner)}/${encodeURIComponent(r.name)}`) : '';
  const nextDispatch = p.next_dispatch_after && new Date(p.next_dispatch_after).getTime() > Date.now() ? fmtDateTime(p.next_dispatch_after) : '—';
  const wide = (label, html) => `<div class="meta-item proj-wide"><div class="label">${escapeHtml(label)}</div><div class="value">${html}</div></div>`;
  return `<div class="meta-grid proj-meta">
      ${metaItemHtml('Agent', agentLabel(p.agent))}
      <div class="meta-item"><div class="label">Repository</div><div class="value">${repoUrl ? `<a href="${escapeHtml(repoUrl)}" target="_blank" rel="noopener">${escapeHtml(r.owner + '/' + r.name)}</a>` : '—'}</div></div>
      ${metaItemHtml('Base branch', r.base_branch || '—')}
      ${metaItemHtml('Datadog site', sig.site ? sig.site.toUpperCase() : '—')}
      ${metaItemHtml('Interval', p.interval ? 'every ' + p.interval : '—')}
      ${metaItemHtml('First scan', 'last ' + (sig.lookback || '24h'))}
      ${metaItemHtml('Routine', d.routine_id || '—')}
      ${metaItemHtml('Trigger token', d.token ? d.token + (d.token_configured === false ? ' (missing)' : '') : '—')}
      ${metaItemHtml('Created by', p.created_by || '—')}
      ${metaItemHtml('Created', fmtDateTime(p.created_at))}
      ${metaItemHtml('Last tick', p.last_tick ? fmtDateTime(p.last_tick) : '—')}
      ${metaItemHtml('Next dispatch after', nextDispatch)}
      ${wide('Datadog query', `<code>${escapeHtml(sig.query || '—')}</code>`)}
      ${sig.pattern ? wide('Pattern', `<code>${escapeHtml(sig.pattern)}</code>`) : metaItemHtml('Pattern', 'none')}
      ${wide('Limits', `<div class="chip-row">${PJD_LIMITS.map(([k, label]) => `<span class="chip">${escapeHtml(label)} <b>${fmtInt(l[k])}</b></span>`).join('')}</div>`)}
    </div>`;
}

function pjdNoteHtml(n, manage) {
  const src = PJD_NOTE_SOURCES.includes(n.source) ? n.source : 'session';
  const meta = [n.by, n.task ? 'task ' + n.task : '', n.at ? timeAgo(n.at) : ''].filter(Boolean).map(escapeHtml).join(' · ');
  return `<div class="proj-note">
      <span class="tag proj-src-${src}">${escapeHtml(n.source || src)}</span>
      <div><div class="proj-note-text">${escapeHtml(n.text)}</div>${meta ? `<div class="proj-note-meta" title="${escapeHtml(n.at ? fmtDateTime(n.at) : '')}">${meta}</div>` : ''}</div>
      ${manage && n.id ? `<button class="btn-mini danger" type="button" data-pjd="delete-note" data-note="${escapeHtml(n.id)}">Delete</button>` : ''}
    </div>`;
}

function pjdMemoryHtml(p, manage) {
  const notes = p.memory || [];
  const add = manage ? '<button class="btn-mini h2-btn" type="button" data-pjd="add-note">Add note</button>' : '';
  const body = notes.length
    ? `<div class="proj-notes">${notes.map(n => pjdNoteHtml(n, manage)).join('')}</div>`
    : emptyHtml(p.state_error ? 'Project memory could not be loaded.' : 'No notes yet. Admin notes, lessons sessions record and feedback from closed pull requests collect here and go into every work order.');
  return `<section class="panel"><h2>Project memory <small>${notes.length ? plural(notes.length, 'note') : ''}</small>${add}</h2>${body}</section>`;
}

function renderProjectDetail(root, p) {
  const key = p.agent + '/' + p.id;
  if (root.dataset.rendered !== key) pjdExpanded.clear();
  root.dataset.rendered = key;
  const s = p.stats || {};
  const r = p.repo || {};
  const d = p.dispatch || {};
  const [cls, label, title] = projStatus(p);
  const tasks = p.tasks || [];
  const backlog = p.backlog || [];
  const inFlight = tasks.filter(t => t.status === 'dispatching' || t.status === 'running').length;
  const manage = canManageProject(p.agent);
  document.title = `${appTitle} — ${p.name || p.id}`;

  const tags = `<span class="status-pill ${cls}" title="${escapeHtml(title)}">${label}</span>
    ${r.owner && r.name ? `<span class="tag" title="repository and base branch">${escapeHtml(r.owner + '/' + r.name)}${r.base_branch ? '@' + escapeHtml(r.base_branch) : ''}</span>` : ''}
    ${p.interval ? `<span class="tag" title="tick interval">every ${escapeHtml(p.interval)}</span>` : ''}
    <span class="tag" title="project id">${escapeHtml(p.id)}</span>`;
  const actions = (manage ? `
    <button class="btn-mini primary" type="button" onclick="pjdRun(this)"${p.enabled ? '' : ' disabled'} title="${p.enabled ? 'Scan, sync pull requests and dispatch now, outside the interval.' : 'Resume the project before running it.'}">Run now</button>
    <button class="btn-mini" type="button" onclick="pjdToggle(this)" title="${p.enabled ? 'Stop ticking; running sessions finish on their own.' : 'Start ticking again.'}">${p.enabled ? 'Pause' : 'Resume'}</button>
    <button class="btn-mini" type="button" onclick="pjdEdit()">Edit</button>
    <button class="btn-mini danger" type="button" onclick="pjdDelete()">Delete</button>` : '')
    + `<a class="btn-mini" href="${detailApi('project', p.agent, p.id)}" target="_blank" rel="noopener">JSON</a>`;

  let banners = '';
  if (!p.enabled && p.disabled_reason) {
    const reason = p.disabled_reason.replace(/^auto-disabled\s+/i, '');
    banners += `<div class="detail-banner danger"><strong>Auto-disabled${reason === p.disabled_reason ? '.' : ''}</strong> ${escapeHtml(reason)}<br>Resume the project once the cause is fixed.</div>`;
  }
  if (d.token_configured === false) banners += `<div class="detail-banner warn"><strong>Trigger token missing.</strong> This deployment holds no token named “${escapeHtml(d.token || '')}”, so no sessions are dispatched until it is added.</div>`;
  if (p.state_error) banners += `<div class="detail-banner danger"><strong>Project state could not be loaded.</strong> Tasks, backlog and memory are missing from this view: ${escapeHtml(p.state_error)}</div>`;
  if (p.last_error && !(p.disabled_reason && p.disabled_reason.includes(p.last_error))) {
    banners += `<div class="detail-banner danger"><strong>Last tick failed</strong>${p.last_tick ? ' (' + escapeHtml(fmtDateTime(p.last_tick)) + ')' : ''}: ${escapeHtml(p.last_error)}</div>`;
  }

  const tasksBody = tasks.length
    ? `<div class="list-scroll" data-scroll="tasks"><table class="data-table"><thead><tr>
        <th>Status</th><th>Error group</th><th class="n">Count</th><th class="n">Attempt</th><th>Session</th><th>Pull request</th><th>Updated</th>
      </tr></thead><tbody>${tasks.map(t => pjdTaskRowsHtml(p, t)).join('')}</tbody></table></div>`
    : emptyHtml(p.state_error ? 'Tasks could not be loaded.' : 'No sessions dispatched yet. When the query finds an error group and the limits allow, its task shows up here.');
  const backlogBody = backlog.length
    ? `<div class="list-scroll" data-scroll="backlog"><table class="data-table"><thead><tr>
        <th>Error group</th><th class="n">Count</th><th>First seen</th><th>Last seen</th>
      </tr></thead><tbody>${backlog.map(pjdBacklogRowsHtml).join('')}</tbody></table></div>`
    : emptyHtml(p.state_error ? 'The backlog could not be loaded.' : 'No error group is waiting for a session.');
  const waiting = backlog.length
    ? `${s.backlog > backlog.length ? `top ${fmtInt(backlog.length)} of ${fmtInt(s.backlog)}` : fmtInt(backlog.length)} waiting for capacity`
    : '';
  const goal = `<section class="panel"><h2>Goal</h2><div class="report proj-doc">${renderMarkdownDoc(p.goal || '')}</div>
      ${p.instructions ? `<h2 class="proj-subhead">Instructions</h2><div class="report proj-doc">${renderMarkdownDoc(p.instructions)}</div>` : ''}</section>`;

  // Polling repaints the whole page; keep scroll positions and keyboard focus where the reader left them.
  const scrolls = [...root.querySelectorAll('[data-scroll]')].map(el => [el.dataset.scroll, el.scrollTop, el.scrollLeft]);
  const focused = root.contains(document.activeElement) ? document.activeElement : null;
  const focusSel = focused && ['data-key', 'data-note', 'data-pjd', 'onclick']
    .filter(a => focused.hasAttribute(a)).map(a => `[${a}="${CSS.escape(focused.getAttribute(a))}"]`)[0];
  root.innerHTML = detailHeadHtml({ parent: 'projects', agent: p.agent, name: p.name || p.id, description: p.description, tags, actions })
    + banners
    + pjdStatsHtml(s)
    + pjdChartHtml(s)
    + pjdMetaHtml(p)
    + `<div class="detail-stack">
      <section class="panel"><h2>Tasks <small>${tasks.length ? plural(tasks.length, 'task') : ''}${inFlight ? ` · ${fmtInt(inFlight)} in flight` : ''}</small></h2>${tasksBody}</section>
      <section class="panel"><h2>Backlog <small>${waiting}</small></h2>${backlogBody}</section>
      ${pjdMemoryHtml(p, manage)}
      ${goal}
    </div>
    <div class="detail-foot">Refreshed ${escapeHtml(new Date().toLocaleTimeString())} · updates every ${s.active_sessions > 0 ? '10' : '30'} seconds</div>`;
  scrolls.forEach(([k, top, left]) => {
    const el = root.querySelector(`[data-scroll="${k}"]`);
    if (el) { el.scrollTop = top; el.scrollLeft = left; }
  });
  const refocus = focusSel && root.querySelector(focusSel);
  if (refocus) refocus.focus({ preventScroll: true });
}

function pjdToggleRow(btn) {
  const more = btn.closest('tr').nextElementSibling;
  if (!more || !more.classList.contains('proj-more')) return;
  more.hidden = !more.hidden;
  btn.setAttribute('aria-expanded', String(!more.hidden));
  if (more.hidden) pjdExpanded.delete(btn.dataset.key); else pjdExpanded.add(btn.dataset.key);
}

document.getElementById('proj-detail').addEventListener('click', e => {
  const toggle = e.target.closest('.proj-toggle');
  if (toggle) { pjdToggleRow(toggle); return; }
  const act = e.target.closest('[data-pjd]');
  if (!act) return;
  if (act.dataset.pjd === 'add-note') pjdAddNote();
  else if (act.dataset.pjd === 'delete-note') pjdDeleteNote(act.dataset.note, act);
});

async function pjdRun(btn) {
  const p = detailEntity;
  if (!p) return;
  if (!(await uiConfirm('It scans Datadog, syncs the project’s pull requests and dispatches sessions if the limits allow. Sessions count toward the organization’s Claude Code usage.', { title: 'Run this project now?', okLabel: 'Run now' }))) return;
  btn.disabled = true;
  btn.textContent = 'Queued…';
  try {
    await apiSend(`${detailApi('project', p.agent, p.id)}/run`, 'POST', {});
  } catch (err) {
    await uiError('Failed to start a run', err);
  }
  delete lastFetched.projects;
  loadProjects();
  scheduleDetailPoll(3000);
}

async function pjdToggle(btn) {
  const p = detailEntity;
  if (!p) return;
  btn.disabled = true;
  try {
    await apiSend(detailApi('project', p.agent, p.id), 'PATCH', { enabled: !p.enabled });
  } catch (err) {
    await uiError('Failed to update project', err);
  }
  delete lastFetched.projects;
  loadProjects();
  await loadDetail();
}

async function pjdDelete() {
  const p = detailEntity;
  if (!p) return;
  if (!(await uiConfirm('This stops the project and removes its tasks, memory and statistics. Sessions still running can no longer report back; pull requests it opened stay on GitHub.', { title: 'Delete this project?', tone: 'danger', okLabel: 'Delete' }))) return;
  try {
    await apiSend(detailApi('project', p.agent, p.id), 'DELETE');
  } catch (err) {
    await uiError('Failed to delete project', err);
    return;
  }
  delete lastFetched.projects;
  navigate('projects');
}

function pjdEdit() {
  if (detailEntity) projectForm(detailEntity);
}

function pjdAddNote() {
  const p = detailEntity;
  if (!p) return;
  openForm({
    title: 'Add a note',
    subtitle: p.name || p.id,
    saveLabel: 'Add note',
    html: formField('Note', '<textarea class="form-textarea proj-textarea" id="pjn-text" maxlength="500" placeholder="The HTTP client already retries timeouts; do not wrap it in another retry loop."></textarea>',
      'Up to 500 characters. Admin notes go into every work order, ahead of what sessions have learned.'),
    onSave: async () => {
      const text = formValue('pjn-text');
      if (!text) throw new Error('Write the note first.');
      await apiSend(`${detailApi('project', p.agent, p.id)}/memory`, 'POST', { text });
      await loadDetail();
    },
  });
}

async function pjdDeleteNote(noteId, btn) {
  const p = detailEntity;
  if (!p || !noteId) return;
  if (!(await uiConfirm('Work orders stop including it.', { title: 'Delete this note?', tone: 'danger', okLabel: 'Delete' }))) return;
  btn.disabled = true;
  try {
    await apiSend(`${detailApi('project', p.agent, p.id)}/memory/${encodeURIComponent(noteId)}`, 'DELETE');
  } catch (err) {
    await uiError('Failed to delete note', err);
  }
  await loadDetail();
}

/* Project editor, shared by New project and Edit */
function projectForm(p) {
  const isNew = !p;
  const agents = agentsData.filter(a => canManageProject(a.id));
  if (isNew && !agents.length) {
    uiAlert('Creating a project needs access to at least one agent.', { title: 'No agent available', tone: 'warn' });
    return;
  }
  const r = (p && p.repo) || {}, sig = (p && p.signal) || {}, d = (p && p.dispatch) || {}, l = (p && p.limits) || {};
  const input = (id, value, attrs) => `<input class="form-input" id="${id}" value="${escapeHtml(value == null ? '' : value)}"${attrs || ''}>`;
  const code = ' autocapitalize="off" spellcheck="false"';
  const site = sig.site === 'eu' ? 'eu' : 'us';
  const limits = PJD_LIMITS.map(([k, label, min, max, def, hint]) => formField(escapeHtml(label),
    `<input class="form-input" id="pjf-l-${k}" type="number" min="${min}" max="${max}" step="1" inputmode="numeric" value="${l[k] ? escapeHtml(l[k]) : ''}" placeholder="${def}">`,
    `${min}–${max}, default ${def}${hint ? '. ' + escapeHtml(hint) : ''}`)).join('');
  openForm({
    title: isNew ? 'New project' : 'Edit project',
    subtitle: isNew ? 'An agent keeps fixing the errors a Datadog query finds in one repository' : (p.name || p.id),
    saveLabel: isNew ? 'Create project' : 'Save changes',
    html: formField('Name', input('pjf-name', p && p.name, ' maxlength="80" placeholder="Fix production errors"'))
      + (isNew ? formField('Agent', `<select class="form-input" id="pjf-agent">${agents.map(a => `<option value="${escapeHtml(a.id)}">${escapeHtml(a.name)}</option>`).join('')}</select>`, 'Its skills go into every work order. It needs Datadog access.') : '')
      + formField('Description', input('pjf-desc', p && p.description, ' maxlength="240" placeholder="One line on what this project covers"'))
      + formField('Goal', `<textarea class="form-textarea proj-textarea" id="pjf-goal" maxlength="4000" placeholder="Fix the root cause of each production error with the smallest change that makes it stop.">${escapeHtml((p && p.goal) || '')}</textarea>`, 'Every work order starts with it.')
      + formField('Instructions', `<textarea class="form-textarea proj-textarea" id="pjf-instructions" maxlength="8000" placeholder="Optional. Conventions the sessions should follow in this repository.">${escapeHtml((p && p.instructions) || '')}</textarea>`)
      + '<div class="form-section">Repository</div>'
      + `<div class="form-grid3">${formField('Owner', input('pjf-owner', r.owner, ' placeholder="owner"' + code))}${formField('Name', input('pjf-repo', r.name, ' placeholder="repo"' + code))}${formField('Base branch', input('pjf-base', r.base_branch, ' placeholder="main"' + code), isNew ? 'Empty uses the default branch.' : '')}</div>`
      + '<div class="form-section">Datadog</div>'
      + `<div class="form-grid2">${formField('Site', `<select class="form-input" id="pjf-site"><option value="us"${site === 'us' ? ' selected' : ''}>US</option><option value="eu"${site === 'eu' ? ' selected' : ''}>EU</option></select>`)}${formField('First scan', input('pjf-lookback', sig.lookback, ' placeholder="24h"' + code), 'How far back the first scan looks, 1h to 168h.')}</div>`
      + formField('Query', input('pjf-query', sig.query, ' maxlength="1000" placeholder="service:example status:error"' + code), 'Log search query. Matching errors are grouped by service, error kind, message and top stack frame.')
      + formField('Pattern', input('pjf-pattern', sig.pattern, ' maxlength="500" placeholder="(?i)timeout|KeyError"' + code), 'Optional regular expression. A group is kept when it matches the error kind, the message or a sample.')
      + '<div class="form-section">Dispatch</div>'
      + `<div class="form-grid3">${formField('Routine id', input('pjf-routine', d.routine_id, ' placeholder="trig_0123456789abcdef"' + code))}${formField('Trigger token', input('pjf-token', d.token, ' maxlength="32" placeholder="default"' + code), 'A token name this deployment holds, never the token.')}${formField('Check every', input('pjf-interval', p && p.interval, ' placeholder="15m"' + code), '5m to 24h.')}</div>`
      + `<details class="form-more"><summary>Limits</summary><div class="form-grid2">${limits}</div></details>`
      + `<label class="check-row"><input type="checkbox" id="pjf-enabled"${!p || p.enabled ? ' checked' : ''}> Enabled</label>`,
    onSave: async () => {
      const body = pjfBody(isNew);
      if (isNew) {
        const res = await apiSend('/api/projects', 'POST', body);
        delete lastFetched.projects;
        await loadProjects();
        const aid = safeId(res && res.agent), pid = safeId(res && res.id);
        if (aid && pid) routeTo(detailPath('project', aid, pid));
        return;
      }
      const changes = pjfChanges(p, body);
      if (!Object.keys(changes).length) return;
      await apiSend(detailApi('project', p.agent, p.id), 'PATCH', changes);
      delete lastFetched.projects;
      loadProjects();
      await loadDetail();
    },
  });
}

function pjfBody(isNew) {
  const body = {
    name: formValue('pjf-name'),
    description: formValue('pjf-desc'),
    goal: formValue('pjf-goal'),
    instructions: formValue('pjf-instructions'),
    repo: { owner: formValue('pjf-owner'), name: formValue('pjf-repo'), base_branch: formValue('pjf-base') },
    signal: { type: 'datadog', site: formValue('pjf-site'), query: formValue('pjf-query'), pattern: formValue('pjf-pattern'), lookback: formValue('pjf-lookback') },
    dispatch: { routine_id: formValue('pjf-routine'), token: formValue('pjf-token') },
    limits: Object.fromEntries(PJD_LIMITS.map(([k]) => [k, parseInt(formValue('pjf-l-' + k), 10) || 0])),
    interval: formValue('pjf-interval'),
    enabled: document.getElementById('pjf-enabled').checked,
  };
  if (!body.name) throw new Error('Name is required.');
  if (!body.goal) throw new Error('Goal is required.');
  if (!body.repo.owner || !body.repo.name) throw new Error('Repository owner and name are required.');
  if (!isNew && !body.repo.base_branch) throw new Error('Base branch is required.');
  if (!body.signal.query) throw new Error('A Datadog query is required.');
  if (!body.dispatch.routine_id || !body.dispatch.token) throw new Error('Routine id and trigger token name are required.');
  return isNew ? { agent: formValue('pjf-agent'), ...body } : body;
}

function pjfChanges(p, body) {
  const r = p.repo || {}, sig = p.signal || {}, d = p.dispatch || {}, l = p.limits || {};
  const before = {
    name: p.name || '', description: p.description || '', goal: p.goal || '', instructions: p.instructions || '',
    repo: { owner: r.owner || '', name: r.name || '', base_branch: r.base_branch || '' },
    signal: { type: 'datadog', site: sig.site || '', query: sig.query || '', pattern: sig.pattern || '', lookback: sig.lookback || '' },
    dispatch: { routine_id: d.routine_id || '', token: d.token || '' },
    limits: Object.fromEntries(PJD_LIMITS.map(([k]) => [k, l[k] || 0])),
    interval: p.interval || '',
    enabled: !!p.enabled,
  };
  const out = {};
  for (const k of Object.keys(body)) if (JSON.stringify(body[k]) !== JSON.stringify(before[k])) out[k] = body[k];
  return out;
}

/* Content renderers */
function linkifyEscaped(s) {
  return s.replace(/(https?:\/\/[^\s<>"')]+?)(?=[)\].,;:!?]*(\s|$))/g, '<a href="$1" target="_blank" rel="noopener">$1</a>');
}

// Slack-style mrkdwn (*bold*, `code`, <url|label>, :emoji:) into safe HTML.
function renderRichText(text) {
  let s = escapeHtml(text);
  s = s.replace(/&lt;(https?:\/\/[^|&\s]+)\|([^&]+?)&gt;/g, '<a href="$1" target="_blank" rel="noopener">$2</a>');
  s = s.replace(/&lt;(https?:\/\/[^&\s]+)&gt;/g, '<a href="$1" target="_blank" rel="noopener">$1</a>');
  s = s.replace(/&lt;br\s*\/?&gt;/gi, '<br>');
  s = s.replace(/\*\*([^*\n]+)\*\*/g, '<strong>$1</strong>');
  s = s.replace(/(^|[\s(])\*([^*\n]+)\*/g, '$1<strong>$2</strong>');
  s = s.replace(/`([^`\n]+)`/g, '<code>$1</code>');
  const emoji = {
    ':white_check_mark:': '✅', ':rotating_light:': '\u{1F6A8}', ':grey_question:': '❔',
    ':warning:': '⚠️', ':x:': '❌', ':heavy_check_mark:': '✔️',
  };
  return s.replace(/:[a-z0-9_+-]+:/gi, m => emoji[m] || m);
}

// A safe subset of GitHub-flavoured Markdown: the input is escaped first and
// only the transforms below introduce tags.
function mdEmphasis(s) {
  s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
  s = s.replace(/__([^_]+)__/g, '<strong>$1</strong>');
  s = s.replace(/(^|[^*])\*([^*\n]+)\*/g, '$1<em>$2</em>');
  s = s.replace(/(^|[^_])_([^_\n]+)_/g, '$1<em>$2</em>');
  return s.replace(/~~([^~]+)~~/g, '<del>$1</del>');
}

function mdInlineDoc(s) {
  const stash = [];
  const keep = html => '\u0000' + (stash.push(html) - 1) + '\u0000';
  const link = (url, label) => keep(`<a href="${url}" target="_blank" rel="noopener">${label}</a>`);
  s = s.replace(/`([^`]+)`/g, (m, c) => keep('<code>' + c + '</code>'));
  s = s.replace(/\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)/g, (m, label, url) => link(url, mdEmphasis(label)));
  s = s.replace(/(^|[\s(])(https?:\/\/[^\s<>()]+?)(?=[)\].,;:!?]*(\s|$))/g, (m, pre, url) => pre + link(url, url));
  return mdEmphasis(s).replace(/\u0000(\d+)\u0000/g, (m, i) => stash[+i]);
}

function renderMarkdownDoc(md) {
  let src = escapeHtml(String(md || ''));
  src = src.replace(/&lt;(https?:\/\/[^|\s&<>]+)\|([^&<>]+)&gt;/g, '[$2]($1)');
  src = src.replace(/&lt;(https?:\/\/[^|\s&<>]+)&gt;/g, '[$1]($1)');
  const lines = src.split('\n');
  const isTableSep = s => /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)+\|?\s*$/.test(s);
  const splitRow = s => s.replace(/^\s*\|/, '').replace(/\|\s*$/, '').replace(/\\\|/g, '').split('|').map(c => c.trim().replace(//g, '|'));
  let html = '', i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (/^\s*```/.test(line)) {
      i++;
      const buf = [];
      while (i < lines.length && !/^\s*```/.test(lines[i])) { buf.push(lines[i]); i++; }
      i++;
      html += '<pre class="md-pre"><code>' + buf.join('\n') + '</code></pre>';
      continue;
    }
    const h = line.match(/^\s*(#{1,6})\s+(.*?)\s*#*$/);
    if (h) { const lvl = h[1].length; html += `<h${lvl} class="md-h">${mdInlineDoc(h[2])}</h${lvl}>`; i++; continue; }
    if (/^\s*([-*_])(\s*\1){2,}\s*$/.test(line)) { html += '<hr>'; i++; continue; }
    if (line.indexOf('|') !== -1 && i + 1 < lines.length && isTableSep(lines[i + 1])) {
      const head = splitRow(line);
      i += 2;
      const rows = [];
      while (i < lines.length && lines[i].indexOf('|') !== -1 && lines[i].trim() !== '') { rows.push(splitRow(lines[i])); i++; }
      let t = '<table class="md-table"><thead><tr>' + head.map(c => '<th>' + mdInlineDoc(c) + '</th>').join('') + '</tr></thead><tbody>';
      rows.forEach(r => {
        t += '<tr>';
        for (let k = 0; k < head.length; k++) {
          const val = (k === head.length - 1 && r.length > head.length) ? r.slice(k).join(' | ') : (r[k] || '');
          t += '<td>' + mdInlineDoc(val) + '</td>';
        }
        t += '</tr>';
      });
      html += t + '</tbody></table>';
      continue;
    }
    if (/^\s*&gt;\s?/.test(line)) {
      const buf = [];
      while (i < lines.length && /^\s*&gt;\s?/.test(lines[i])) { buf.push(lines[i].replace(/^\s*&gt;\s?/, '')); i++; }
      html += '<blockquote class="md-quote">' + mdInlineDoc(buf.join('<br>')) + '</blockquote>';
      continue;
    }
    if (/^\s*(\d+)[.)]\s+/.test(line)) {
      let t = '<ol class="md-list">';
      while (i < lines.length && /^\s*(\d+)[.)]\s+/.test(lines[i])) { t += '<li>' + mdInlineDoc(lines[i].replace(/^\s*(\d+)[.)]\s+/, '')) + '</li>'; i++; }
      html += t + '</ol>';
      continue;
    }
    if (/^\s*[-*+]\s+/.test(line)) {
      let t = '<ul class="md-list">';
      while (i < lines.length && /^\s*[-*+]\s+/.test(lines[i])) { t += '<li>' + mdInlineDoc(lines[i].replace(/^\s*[-*+]\s+/, '')) + '</li>'; i++; }
      html += t + '</ul>';
      continue;
    }
    if (line.trim() === '') { i++; continue; }
    const buf = [];
    while (i < lines.length && lines[i].trim() !== '' &&
           !/^\s*(#{1,6}\s|```|&gt;\s?|[-*+]\s|\d+[.)]\s)/.test(lines[i]) &&
           !(lines[i].indexOf('|') !== -1 && i + 1 < lines.length && isTableSep(lines[i + 1]))) {
      buf.push(lines[i]); i++;
    }
    html += '<p>' + mdInlineDoc(buf.join('<br>')) + '</p>';
  }
  return html;
}

function renderCellValue(v) {
  if (v === null || v === undefined) return '';
  if (typeof v === 'string') return linkifyEscaped(escapeHtml(v.length > 400 ? v.slice(0, 400) + '…' : v));
  if (typeof v !== 'object') return escapeHtml(String(v));
  if (Array.isArray(v)) {
    if (!v.length) return '<span class="muted">[]</span>';
    if (v.every(x => x === null || typeof x !== 'object')) {
      const joined = v.map(x => String(x ?? '')).join(', ');
      return escapeHtml(joined.length > 240 ? joined.slice(0, 240) + '…' : joined);
    }
    return `<details class="cell"><summary>[${v.length} items]</summary><pre>${escapeHtml(JSON.stringify(v, null, 2))}</pre></details>`;
  }
  const keys = Object.keys(v);
  if (!keys.length) return '<span class="muted">{}</span>';
  return `<details class="cell"><summary>{${keys.length} keys}</summary><pre>${escapeHtml(JSON.stringify(v, null, 2))}</pre></details>`;
}

function renderSourceContent(content) {
  if (content === null || content === undefined) return '<div class="empty">No data.</div>';
  if (typeof content === 'object' && !Array.isArray(content) && content.kind === 'datadog_metrics') return renderMetricsPayload(content);
  if (typeof content === 'string') {
    if (/[*<`:]/.test(content)) return `<div class="rich">${renderRichText(content)}</div>`;
    return `<pre class="content">${escapeHtml(content)}</pre>`;
  }
  if (Array.isArray(content)) {
    if (!content.length) return '<div class="empty">No results.</div>';
    const first = content[0];
    if (first && typeof first === 'object' && !Array.isArray(first)) {
      const keys = [...content.reduce((s, r) => { Object.keys(r || {}).forEach(k => s.add(k)); return s; }, new Set())].slice(0, 10);
      const rows = content.slice(0, 100).map(r => '<tr>' + keys.map(k => `<td>${renderCellValue(r[k])}</td>`).join('') + '</tr>').join('');
      return `<div class="table-wrap"><table><thead><tr>${keys.map(k => `<th>${escapeHtml(k)}</th>`).join('')}</tr></thead><tbody>${rows}</tbody></table></div>`
        + (content.length > 100 ? `<div class="rows-note">Showing 100 of ${content.length} rows.</div>` : '');
    }
  }
  return `<pre class="content">${escapeHtml(JSON.stringify(content, null, 2))}</pre>`;
}

/* Datadog metrics: a chart per site plus a summary table */
const metricsCharts = {};
let metricsChartSeq = 0;
let chartJsPromise = null;

function ensureChartJs() {
  if (typeof Chart !== 'undefined') return Promise.resolve(true);
  if (!chartJsPromise) {
    chartJsPromise = new Promise(resolve => {
      const load = (src, next) => {
        const s = document.createElement('script');
        s.src = src;
        s.onload = next;
        s.onerror = () => resolve(false);
        document.head.appendChild(s);
      };
      load('https://cdn.jsdelivr.net/npm/chart.js@4.5.1/dist/chart.umd.min.js', () =>
        load('https://cdn.jsdelivr.net/npm/chartjs-adapter-date-fns@3.0.0/dist/chartjs-adapter-date-fns.bundle.min.js', () => resolve(true)));
      setTimeout(() => resolve(typeof Chart !== 'undefined'), 8000);
    });
  }
  return chartJsPromise;
}

function inkTick() { return document.documentElement.dataset.theme === 'light' ? '#5d6671' : '#8a93a3'; }
function inkGrid() { return document.documentElement.dataset.theme === 'light' ? 'rgba(40,52,70,0.08)' : 'rgba(143,152,168,0.07)'; }
function applyChartTheme(ch) {
  if (!ch) return;
  const o = ch.options;
  if (o.plugins && o.plugins.legend && o.plugins.legend.labels) o.plugins.legend.labels.color = inkTick();
  for (const ax of Object.values(o.scales || {})) {
    if (ax.ticks) ax.ticks.color = inkTick();
    if (ax.grid) ax.grid.color = inkGrid();
    if (ax.title && ax.title.display) ax.title.color = inkTick();
  }
  ch.update('none');
}
document.addEventListener('themechange', () => Object.values(metricsCharts).forEach(applyChartTheme));

function colorForScope(scope, i) {
  const palette = ['#7297bd', '#83b08a', '#c0a368', '#c08a66', '#8f87b8', '#76a8b8', '#b08299', '#94a36a', '#c47f7f', '#7e95b5', '#b59a66', '#6fa39b'];
  if (!scope) return palette[i % palette.length];
  let h = 0;
  for (let k = 0; k < scope.length; k++) h = (h * 31 + scope.charCodeAt(k)) | 0;
  return palette[Math.abs(h) % palette.length];
}

function fmtMetricNum(v) {
  if (v === null || v === undefined || Number.isNaN(v)) return '—';
  const a = Math.abs(v);
  if (a === 0) return '0';
  if (a >= 1e12) return (v / 1e12).toFixed(2) + 'T';
  if (a >= 1e9) return (v / 1e9).toFixed(2) + 'G';
  if (a >= 1e6) return (v / 1e6).toFixed(2) + 'M';
  if (a >= 1e3) return (v / 1e3).toFixed(2) + 'K';
  if (a >= 10) return v.toFixed(2);
  if (a >= 1) return v.toFixed(3);
  return v.toFixed(4);
}

function inferMetricUnit(site) {
  const q = String(site.query || '').toLowerCase();
  let maxAbs = 0;
  for (const s of (site.series || [])) {
    for (const k of ['avg', 'min', 'max', 'last']) {
      const v = s[k];
      if (typeof v === 'number' && Number.isFinite(v) && Math.abs(v) > maxAbs) maxAbs = Math.abs(v);
    }
  }
  const isCPU = /kubernetes\.cpu\./.test(q);
  const isMem = /kubernetes\.memory\./.test(q);
  const coresNative = isCPU && /\.(requests|limits|capacity|allocatable)\b/.test(q);
  const endsMul100 = /\*\s*100\s*$/.test(q.trim());
  let kind = 'raw', warning = '';
  if (isCPU && endsMul100 && maxAbs <= 1000) kind = 'percent';
  else if (isCPU && endsMul100) {
    kind = 'cores';
    warning = "The query ends with '* 100' (a percentage of CPU requests) but the values are nanocore-sized, which usually means kubernetes.cpu.requests is missing for these workloads. Showing CPU usage in cores instead.";
  } else if (coresNative) kind = 'cores_native';
  else if (isCPU) kind = 'cores';
  else if (isMem) kind = 'bytes';
  else if (endsMul100 && maxAbs <= 1000) kind = 'percent';
  return { kind, warning, maxAbs };
}

function unitDisplayLabel(u) {
  switch (u.kind) {
    case 'percent': return '%';
    case 'cores': case 'cores_native': return 'CPU cores';
    case 'bytes': return 'bytes (auto-scaled)';
    default: return 'native units';
  }
}

function formatByUnit(v, u) {
  if (v === null || v === undefined || !Number.isFinite(v)) return '—';
  if (u.kind === 'percent') return (Math.abs(v) >= 10 ? v.toFixed(1) : v.toFixed(2)) + '%';
  if (u.kind === 'cores' || u.kind === 'cores_native') {
    const cores = u.kind === 'cores' ? v / 1e9 : v;
    const a = Math.abs(cores);
    if (a === 0) return '0';
    if (a >= 1) return cores.toFixed(2) + ' cores';
    return (cores * 1000).toFixed(a >= 0.001 ? 0 : 3) + 'm';
  }
  if (u.kind === 'bytes') {
    const a = Math.abs(v);
    if (a >= 1024 ** 4) return (v / 1024 ** 4).toFixed(2) + ' TiB';
    if (a >= 1024 ** 3) return (v / 1024 ** 3).toFixed(2) + ' GiB';
    if (a >= 1024 ** 2) return (v / 1024 ** 2).toFixed(2) + ' MiB';
    if (a >= 1024) return (v / 1024).toFixed(2) + ' KiB';
    return v.toFixed(0) + ' B';
  }
  return fmtMetricNum(v);
}

function chartScale(u) {
  if (u.kind === 'cores') return { div: 1e9, label: 'cores' };
  if (u.kind === 'cores_native') return { div: 1, label: 'cores' };
  if (u.kind === 'bytes') {
    const a = u.maxAbs || 0;
    if (a >= 1024 ** 3) return { div: 1024 ** 3, label: 'GiB' };
    if (a >= 1024 ** 2) return { div: 1024 ** 2, label: 'MiB' };
    if (a >= 1024) return { div: 1024, label: 'KiB' };
    return { div: 1, label: 'bytes' };
  }
  if (u.kind === 'percent') return { div: 1, label: '%' };
  return { div: 1, label: '' };
}

function describeWindow(fromMs, toMs) {
  if (!fromMs || !toMs) return '';
  const mins = Math.max(0, toMs - fromMs) / 60000;
  if (mins < 90) return Math.round(mins) + ' min';
  const hrs = mins / 60;
  if (hrs < 48) return (hrs % 1 === 0 ? hrs.toFixed(0) : hrs.toFixed(1)) + ' h';
  return (hrs / 24).toFixed(1) + ' d';
}

function metricsSummaryIntro(site, u) {
  const series = site.series || [];
  if (!series.length) return '';
  const sorted = series.filter(s => typeof s.avg === 'number' && Number.isFinite(s.avg)).sort((a, b) => b.avg - a.avg);
  const top = sorted[0], bot = sorted[sorted.length - 1];
  const win = describeWindow(site.from_date, site.to_date);
  const bits = [`<strong>${series.length}</strong> series${win ? ` over the last <strong>${win}</strong>` : ''}`];
  if (top) bits.push(`highest avg: <strong>${escapeHtml(top.scope || '*')}</strong> (${formatByUnit(top.avg, u)})`);
  if (bot && bot !== top) bits.push(`lowest avg: <strong>${escapeHtml(bot.scope || '*')}</strong> (${formatByUnit(bot.avg, u)})`);
  if (site.truncated) bits.push('+' + site.truncated + ' more hidden');
  return (u.warning ? `<div class="metrics-warning">${escapeHtml(u.warning)}</div>` : '')
    + `<div class="metrics-intro">Showing ${bits.join(' · ')}. Values formatted as ${escapeHtml(unitDisplayLabel(u))}.</div>`;
}

function datadogExploreURL(site, fromMs, toMs) {
  const host = site && site.indexOf('.') !== -1 ? site : 'datadoghq.com';
  const params = new URLSearchParams({ paused: 'true' });
  if (fromMs) params.set('start', String(fromMs));
  if (toMs) params.set('end', String(toMs));
  return 'https://app.' + host + '/metric/explorer?' + params.toString();
}

function copyMetricsQuery(btn) {
  const q = btn && btn.getAttribute('data-query');
  if (!q || !navigator.clipboard) return;
  const done = () => { const prev = btn.textContent; btn.textContent = 'Copied'; setTimeout(() => { btn.textContent = prev; }, 1200); };
  navigator.clipboard.writeText(q).then(done, done);
}

function renderMetricsPayload(payload) {
  const sites = payload.sites || [];
  if (!sites.length) return '<div class="empty">No metric series returned.</div>';
  const canvasIds = sites.map(() => 'metrics-chart-' + (++metricsChartSeq));
  const blocks = sites.map((site, idx) => {
    const series = site.series || [];
    const unit = inferMetricUnit(site);
    const unitLabel = site.unit ? escapeHtml(site.unit) : escapeHtml(unitDisplayLabel(unit));
    const qAttr = escapeHtml(site.query || '');
    const header = `<h4>[${escapeHtml(site.label || site.site || '')}] (${unitLabel}) <span class="q" title="${qAttr}">${qAttr}</span>`
      + `<button type="button" class="btn-mini" data-query="${qAttr}" onclick="copyMetricsQuery(this)">Copy query</button>`
      + `<a class="btn-mini" href="${escapeHtml(datadogExploreURL(site.site, site.from_date, site.to_date))}" target="_blank" rel="noopener" title="Opens Datadog Metrics Explorer on the same time window; paste the copied query.">Open in Datadog</a></h4>`;
    const errorBlock = site.error ? `<div class="source-error">${escapeHtml(site.error)}</div>` : '';
    if (!series.length) {
      return `<div class="metrics-site">${header}${errorBlock}${errorBlock ? '' : '<div class="empty">No data returned. The metric may not exist in this site, the tag filter may match nothing, or the window may be empty.</div>'}</div>`;
    }
    const summary = `<div class="metrics-summary"><div class="table-wrap"><table><thead><tr><th>scope</th><th>avg</th><th>min</th><th>max</th><th>last</th><th>n</th></tr></thead><tbody>`
      + series.map(s => `<tr><td>${escapeHtml(s.scope || '*')}</td><td>${formatByUnit(s.avg, unit)}</td><td>${formatByUnit(s.min, unit)}</td><td>${formatByUnit(s.max, unit)}</td><td>${formatByUnit(s.last, unit)}</td><td>${s.count ?? 0}</td></tr>`).join('')
      + `</tbody></table></div>${site.truncated ? `<div class="rows-note">… and ${site.truncated} more series (sorted by avg desc; increase top_n to see more)</div>` : ''}</div>`;
    return `<div class="metrics-site">${header}${errorBlock}${metricsSummaryIntro(site, unit)}<div class="metrics-chart"><canvas id="${canvasIds[idx]}"></canvas></div>${summary}</div>`;
  }).join('');

  ensureChartJs().then(ok => requestAnimationFrame(() => requestAnimationFrame(() => {
    sites.forEach((site, idx) => {
      const canvas = document.getElementById(canvasIds[idx]);
      if (!canvas) return;
      if (!ok) { canvas.parentElement.innerHTML = '<div class="empty">Chart library unavailable; the table below has the numbers.</div>'; return; }
      if (metricsCharts[canvasIds[idx]]) metricsCharts[canvasIds[idx]].destroy();
      const unit = inferMetricUnit(site);
      const scale = chartScale(unit);
      const datasets = (site.series || []).map((s, i) => ({
        label: s.scope || '*',
        data: (s.points || []).map(p => ({ x: p[0], y: scale.div === 1 ? p[1] : p[1] / scale.div })),
        borderColor: colorForScope(s.scope || String(i), i),
        backgroundColor: colorForScope(s.scope || String(i), i) + '33',
        borderWidth: 1.5, pointRadius: 0, pointHoverRadius: 3, tension: 0.25, spanGaps: true,
      }));
      try {
        metricsCharts[canvasIds[idx]] = new Chart(canvas.getContext('2d'), {
          type: 'line',
          data: { datasets },
          options: {
            responsive: true, maintainAspectRatio: false,
            interaction: { mode: 'nearest', intersect: false },
            plugins: {
              legend: { display: datasets.length <= 12, labels: { color: inkTick(), boxWidth: 10, font: { size: 10 } }, position: 'bottom' },
              tooltip: { callbacks: {
                title: items => items.length ? new Date(items[0].parsed.x).toLocaleString() : '',
                label: item => (item.dataset.label || '') + ': ' + formatByUnit(item.parsed.y * scale.div, unit),
              } },
            },
            scales: {
              x: { type: 'time', time: { tooltipFormat: 'MMM d, HH:mm' }, ticks: { color: inkTick(), maxRotation: 0, autoSkipPadding: 20 }, grid: { color: inkGrid() } },
              y: { ticks: { color: inkTick() }, grid: { color: inkGrid() }, beginAtZero: true, title: scale.label ? { display: true, text: scale.label, color: inkTick() } : { display: false } },
            },
          },
        });
      } catch (err) {
        console.error('chart init failed', err);
      }
    });
  })));
  return blocks;
}
