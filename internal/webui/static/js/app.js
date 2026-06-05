// aigoproxy dashboard JS — minimal, no framework
// Uses fetch for actions; cards refresh via meta refresh on the page.

(function () {
  'use strict';

  // ─── Card actions ─────────────────────────────────────
  window.recapture = function (host) {
    fetch('/api/recapture', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ host }),
    })
      .then((r) => r.json())
      .then((data) => {
        if (data.status === 'ok') {
          // reload just the image
          const img = document.querySelector(`.card-screenshot img[alt="${host} preview"]`);
          if (img) img.src = `/screenshots/${host}.png?t=${Date.now()}`;
        } else {
          alert('recapture failed: ' + (data.error || 'unknown'));
        }
      })
      .catch((e) => alert('error: ' + e));
  };

  window.captureAll = function () {
    fetch('/api/recapture-all', { method: 'POST' })
      .then((r) => r.json())
      .then((data) => {
        if (data.status === 'ok') {
          setTimeout(() => window.location.reload(), 1000);
        } else {
          alert('failed: ' + (data.error || 'unknown'));
        }
      });
  };

  window.rescan = function () {
    fetch('/api/rescan', { method: 'POST' })
      .then((r) => r.json())
      .then(() => window.location.reload())
      .catch((e) => alert('error: ' + e));
  };

  window.copyUrl = function (host) {
    const url = 'https://' + host;
    navigator.clipboard.writeText(url).then(
      () => {
        // tiny visual feedback
        const btn = event.target;
        const orig = btn.textContent;
        btn.textContent = '✓';
        setTimeout(() => (btn.textContent = orig), 800);
      },
      () => alert('clipboard not available')
    );
  };

  window.removeRoute = function (host) {
    if (!confirm(`Remove route ${host}?`)) return;
    fetch('/mcp', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        jsonrpc: '2.0',
        method: 'tools/call',
        params: { name: 'aigoproxy_remove', arguments: { host } },
        id: 1,
      }),
    })
      .then((r) => r.json())
      .then((data) => {
        if (data.result && data.result.status === 'ok') {
          window.location.reload();
        } else {
          alert('failed: ' + JSON.stringify(data));
        }
      });
  };

  window.register = function (port, nameGuess) {
    const host = prompt(`Hostname (e.g. ${nameGuess}.sapsucker-hirajoshi.ts.net):`, `${nameGuess}.sapsucker-hirajoshi.ts.net`);
    if (!host) return;
    const upstream = `http://127.0.0.1:${port}`;
    const path_prefix = `/${nameGuess}`;
    const auth = 'none';
    fetch('/mcp', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        jsonrpc: '2.0',
        method: 'tools/call',
        params: {
          name: 'aigoproxy_add',
          arguments: { host, upstream, path_prefix, auth },
        },
        id: 1,
      }),
    })
      .then((r) => r.json())
      .then((data) => {
        if (data.result && data.result.status === 'ok') {
          // Also configure Tailscale Funnel for this path
          return fetch('/api/enable-funnel', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ host, path: path_prefix }),
          });
        }
        throw new Error(JSON.stringify(data));
      })
      .then(() => window.location.reload())
      .catch((e) => alert('register failed: ' + e));
  };

  // ─── Relative time ────────────────────────────────────
  function relativeTime(unixNano) {
    if (!unixNano || unixNano === '0') return 'never';
    const ms = Date.now() - Number(unixNano) / 1e6;
    if (ms < 1000) return 'just now';
    if (ms < 60000) return Math.floor(ms / 1000) + 's ago';
    if (ms < 3600000) return Math.floor(ms / 60000) + 'm ago';
    if (ms < 86400000) return Math.floor(ms / 3600000) + 'h ago';
    return Math.floor(ms / 86400000) + 'd ago';
  }

  // Refresh all "last seen" timestamps every 5s
  setInterval(() => {
    document.querySelectorAll('[data-lastreq]').forEach((el) => {
      el.textContent = 'last: ' + relativeTime(el.dataset.lastreq);
    });
  }, 5000);

  // ─── Poll active conns ────────────────────────────────
  function pollActive() {
    fetch('/api/active-conns')
      .then((r) => r.json())
      .then((data) => {
        const el = document.getElementById('active-conns');
        if (el) el.textContent = data.total || 0;
      })
      .catch(() => {});
  }
  setInterval(pollActive, 3000);
  pollActive();
})();
