(function () {
  'use strict';

  const mount = document.getElementById('shell-mount');
  const content = document.getElementById('page-content') || document.querySelector('main.content');
  const existingShell = document.querySelector('.shell');
  if ((!mount && !existingShell) || (mount && !content)) return;

  const activePage = document.body.dataset.shellPage || '';
  const nav = [
    ['overview', 'Overview', '<rect x="3" y="3" width="18" height="18" rx="3"/><path d="M3 9.5h18M9.5 21V9.5"/>'],
    ['integrations', 'Integrations', '<path d="M9 3v5M15 3v5M6 8h12v3a6 6 0 0 1-12 0V8zM12 17v4"/>'],
    ['mcp', 'MCP &amp; Connectors', '<circle cx="12" cy="12" r="2.5"/><circle cx="5" cy="6" r="2"/><circle cx="19" cy="6" r="2"/><circle cx="12" cy="20" r="2"/><path d="M10.2 10.5 6.6 7.4M13.8 10.5l3.6-3.1M12 14.5V18"/>'],
    ['agents', 'Agents', '<rect x="4" y="8" width="16" height="12" rx="3"/><path d="M12 4v4M8.5 20v1.5M15.5 20v1.5"/><circle cx="9" cy="14" r="1" fill="currentColor" stroke="none"/><circle cx="15" cy="14" r="1" fill="currentColor" stroke="none"/>'],
    ['chats', 'Chats', '<path d="M4 5h16v11H9.5L4 20V5z"/><path d="M8 9.5h8M8 12.5h5"/>'],
    ['skills', 'Skills', '<path d="M11 3l1.9 4.9L17.8 9.8l-4.9 1.9L11 16.6l-1.9-4.9L4.2 9.8l4.9-1.9L11 3z"/><path d="M18.5 15l.8 2 2 .8-2 .8-.8 2-.8-2-2-.8 2-.8.8-2z"/>'],
    ['workflows', 'Workflows', '<path d="M21 12a9 9 0 1 1-2.9-6.6"/><path d="M21 3v5.5h-5.5"/><path d="M12 7.5V12l3 2"/>'],
    ['dashboards', 'Dashboards', '<path d="M5 20v-7M11 20V5M17 20v-10M3 20h18"/>'],
    ['changelog', 'Changelog', '<circle cx="12" cy="12" r="3.5"/><path d="M2.5 12h6M15.5 12h6"/>'],
    ['billing', 'Usage &amp; Billing', '<path d="M5.5 3h13v18l-2.6-1.8L13.3 21 12 19.6 10.7 21l-2.6-1.8L5.5 21V3z"/><path d="M9 8.5h6M9 12.5h6"/>'],
  ];

  const groups = [['overview'], ['integrations', 'mcp'], ['agents', 'chats', 'skills'], ['workflows', 'dashboards'], ['changelog', 'billing']];
  const icon = path => `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">${path}</svg>`;
  const navItems = groups.map(group => group.map(id => {
    const item = nav.find(entry => entry[0] === id);
    const active = id === activePage ? ' active' : '';
    return `<a class="nav-item${active}" href="/ui/${id === 'overview' ? '' : id}" data-page="${id}" title="${item[1]}">${icon(item[2])}<span>${item[1]}</span></a>`;
  }).join('')).join('<div class="nav-sep"></div>');

  if (mount) {
    const shell = document.createElement('div');
    shell.innerHTML = `<header class="topbar">
    <div class="topbar-left"><button class="icon-btn" id="menu-toggle" type="button" aria-label="Open navigation">${icon('<path d="M4 7h16M4 12h16M4 17h16"/>')}</button><a class="logo" href="/ui/" data-page="overview"><div class="logo-icon" id="logo-icon">a</div><span id="header-title">arbetern</span></a></div>
    <div class="topbar-right"><button class="theme-toggle" id="theme-toggle" type="button" role="switch" aria-label="Toggle color theme"><span class="theme-toggle-knob"><svg class="tt-sun" viewBox="0 0 24 24" width="11" height="11" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round"><circle cx="12" cy="12" r="4" fill="currentColor" stroke="none"/><path d="M12 2.5v2M12 19.5v2M2.5 12h2M19.5 12h2M5 5l1.4 1.4M17.6 17.6 19 19M19 5l-1.4 1.4M6.4 17.6 5 19"/></svg><svg class="tt-moon" viewBox="0 0 24 24" width="11" height="11" fill="currentColor"><path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/></svg></span></button><div class="user-menu" id="user-menu"><button class="user-btn" id="user-button" type="button" aria-haspopup="dialog" aria-expanded="false" title="Your identity"><span class="user-avatar muted">?</span><span class="user-name">Signing in…</span></button><div class="user-pop" id="user-pop" role="dialog" aria-label="Your identity" hidden></div></div></div>
  </header><div class="shell"><aside class="sidebar" id="sidebar"><nav class="sidebar-nav" aria-label="Primary">${navItems}</nav><button class="sidebar-collapse" id="sidebar-collapse" type="button" aria-label="Collapse navigation" title="Collapse">${icon('<path d="M15 6l-6 6 6 6"/>')}<span>Collapse</span></button></aside><div class="sidebar-scrim" id="sidebar-scrim"></div></div>`;

    const chrome = shell.querySelector('.shell');
    chrome.appendChild(content);
    mount.replaceWith(shell);
  }

  const html = document.documentElement;
  try {
    if (localStorage.getItem('arbetern-sidebar') === 'collapsed') html.dataset.sidebar = 'collapsed';
  } catch (e) {}

  const themeButton = document.getElementById('theme-toggle');
  const setThemeLabel = theme => {
    themeButton.title = 'Switch to ' + (theme === 'light' ? 'dark' : 'light') + ' theme';
    themeButton.setAttribute('aria-checked', String(theme !== 'light'));
  };
  setThemeLabel(html.dataset.theme || 'dark');
  themeButton.addEventListener('click', () => {
    const next = html.dataset.theme === 'light' ? 'dark' : 'light';
    html.dataset.theme = next;
    try { localStorage.setItem('arbetern-theme', next); } catch (e) {}
    setThemeLabel(next);
    window.dispatchEvent(new CustomEvent('arbetern-theme-change', { detail: { theme: next } }));
  });

  const collapseButton = document.getElementById('sidebar-collapse');
  collapseButton.addEventListener('click', () => {
    const collapsed = html.dataset.sidebar !== 'collapsed';
    if (collapsed) html.dataset.sidebar = 'collapsed'; else delete html.dataset.sidebar;
    try { localStorage.setItem('arbetern-sidebar', collapsed ? 'collapsed' : 'expanded'); } catch (e) {}
    collapseButton.title = collapsed ? 'Expand navigation' : 'Collapse navigation';
    collapseButton.setAttribute('aria-label', collapseButton.title);
  });
  document.getElementById('menu-toggle').addEventListener('click', () => {
    if (html.dataset.drawer === 'open') delete html.dataset.drawer; else html.dataset.drawer = 'open';
  });
  document.getElementById('sidebar-scrim').addEventListener('click', () => delete html.dataset.drawer);

  const userButton = document.getElementById('user-button');
  const userPop = document.getElementById('user-pop');
  userButton.addEventListener('click', event => {
    event.stopPropagation();
    userPop.hidden = !userPop.hidden;
    userButton.setAttribute('aria-expanded', String(!userPop.hidden));
  });
  document.addEventListener('click', event => {
    if (!event.target.closest('#user-menu')) {
      userPop.hidden = true;
      userButton.setAttribute('aria-expanded', 'false');
    }
  });

  fetch('/api/me').then(response => response.ok ? response.json() : Promise.reject()).then(me => {
    if (me.anonymous || me.error) return;
    const slack = me.slack || {};
    const name = slack.real_name || slack.display_name || (me.email || '').split('@')[0] || 'Signed in';
    userButton.querySelector('.user-name').textContent = name;
    const avatar = userButton.querySelector('.user-avatar');
    avatar.textContent = name.slice(0, 1).toUpperCase();
    avatar.classList.remove('muted');
    const safeImageUrl = value => {
      try {
        const url = new URL(String(value || ''), window.location.origin);
        return url.protocol === 'https:' ? url.href : '';
      } catch (e) {
        return '';
      }
    };
    const avatarUrl = safeImageUrl(slack.avatar);
    if (avatarUrl) {
      const image = document.createElement('img');
      image.src = avatarUrl;
      image.alt = '';
      image.onerror = () => image.remove();
      avatar.appendChild(image);
    }
    const esc = value => String(value == null ? '' : value).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    const safeExternalUrl = value => { try { const url = new URL(String(value || ''), window.location.origin); return url.protocol === 'http:' || url.protocol === 'https:' ? url.href : ''; } catch (e) { return ''; } };
    const avatarMarkup = (label, url) => `<span class="user-avatar">${esc(label.slice(0, 1).toUpperCase())}${url ? `<img src="${esc(url)}" alt="" onerror="this.remove()">` : ''}</span>`;
    const rows = values => values.filter(item => item[1]).map(item => { const link = item[2] === 'link' ? safeExternalUrl(item[1]) : ''; return `<dt>${esc(item[0])}</dt><dd class="${item[2] || ''}" title="${esc(item[1])}">${link ? `<a href="${esc(link)}" target="_blank" rel="noopener">${esc(String(item[1]).replace(/^https?:\/\//, ''))}</a>` : esc(item[1])}</dd>`; }).join('');
    const slackRows = rows([
      ['Name', slack.real_name],
      ['Display name', slack.display_name ? (slack.handle ? `${slack.display_name} (@${slack.handle})` : slack.display_name) : (slack.handle ? '@' + slack.handle : '')],
      ['Title', slack.title],
      ['Time zone', slack.timezone],
      ['User ID', slack.id, 'mono'],
    ]);
    const atlassian = me.atlassian;
    const atlassianBody = !me.atlassian_connected ? '<div class="id-note">Atlassian is not connected.</div>' : atlassian ? `<dl class="id-row">${rows([['Name', atlassian.display_name], ['Email', atlassian.email], ['Account ID', atlassian.account_id, 'mono'], ['Site', atlassian.site, 'link']])}</dl>` : '<div class="id-note">No Atlassian account matches this email.</div>';
    userPop.innerHTML = `<div class="id-head">${avatarMarkup(name, avatarUrl)}<div><div class="id-title">${esc(name)}</div><div class="id-sub">${esc(me.email || '')}</div></div></div>
      <div class="id-section"><h3>Slack<span class="tag ${me.slack ? 'slack' : ''}">${me.slack ? 'matched' : 'not found'}</span></h3>${me.slack && slackRows ? `<dl class="id-row">${slackRows}</dl>` : '<div class="id-note">No Slack account matches this email.</div>'}</div>
      <div class="id-section"><h3>Atlassian<span class="tag">${!me.atlassian_connected ? 'not connected' : atlassian ? 'matched' : 'not found'}</span></h3>${atlassianBody}</div>
      <div class="id-foot">Resolved ${esc(me.resolved_at || 'just now')} · identity comes from the sign-in proxy</div>`;
  }).catch(() => {});

  fetch('/api/settings').then(response => response.ok ? response.json() : Promise.reject()).then(settings => {
    if (settings.header) document.getElementById('header-title').textContent = settings.header;
  }).catch(() => {});

  const logo = new Image();
  logo.onload = () => {
    const logoIcon = document.getElementById('logo-icon');
    logoIcon.textContent = '';
    logoIcon.classList.add('has-logo');
    logoIcon.appendChild(logo);
  };
  logo.src = '/ui/logo.png';
})();
