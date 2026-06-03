// aigoproxy dashboard JS — minimal, no framework
// Uses htmx for inline form submission if loaded; falls back to fetch.
(function () {
  'use strict';
  const hasHtmx = !!window.htmx;
  if (hasHtmx) return; // htmx will handle the form

  // Fallback: hook the form manually
  const form = document.getElementById('add-form');
  if (form) {
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      const fd = new FormData(form);
      const body = {
        host: fd.get('host'),
        upstream: fd.get('upstream'),
        auth: fd.get('auth'),
        health: fd.get('health'),
        strip_prefix: fd.get('strip_prefix'),
      };
      const r = await fetch('/api/routes', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (r.ok) {
        location.reload();
      } else {
        const t = await r.text();
        alert('Add failed: ' + t);
      }
    });
  }
})();
