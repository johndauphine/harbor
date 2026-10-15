(() => {
  'use strict';
  const storageKey = 'harbor-appearance';
  const choices = new Set(['system', 'light', 'dark']);
  const system = window.matchMedia('(prefers-color-scheme: dark)');
  let preference = 'system';
  try {
    const saved = window.localStorage.getItem(storageKey);
    if (choices.has(saved)) preference = saved;
  } catch (_) {
    // Appearance still works when browser storage is unavailable.
  }

  function apply() {
    document.documentElement.dataset.theme = preference === 'system'
      ? (system.matches ? 'dark' : 'light') : preference;
  }
  apply();

  function connectControl() {
    const control = document.getElementById('appearance');
    if (!control) return;
    control.value = preference;
    control.addEventListener('change', () => {
      if (!choices.has(control.value)) return;
      preference = control.value;
      try { window.localStorage.setItem(storageKey, preference); } catch (_) {}
      apply();
    });
  }
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', connectControl, { once: true });
  } else {
    connectControl();
  }
  const systemChanged = () => { if (preference === 'system') apply(); };
  if (system.addEventListener) system.addEventListener('change', systemChanged);
  else system.addListener(systemChanged);
})();
