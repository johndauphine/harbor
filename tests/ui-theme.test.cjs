const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.join(__dirname, '../frontend/theme.js'), 'utf8');

function fixture({ saved, dark = false, blocked = false, loading = true } = {}) {
  const events = {}, listeners = {}, dataset = {}, writes = [];
  const control = { value: '', addEventListener: (name, fn) => { events[name] = fn; } };
  const system = { matches: dark, addEventListener: (name, fn) => { listeners[name] = fn; } };
  const storage = {
    getItem: () => { if (blocked) throw Error('storage unavailable'); return saved; },
    setItem: (key, value) => { if (blocked) throw Error('storage unavailable'); writes.push([key, value]); },
  };
  const document = {
    documentElement: { dataset }, readyState: loading ? 'loading' : 'complete',
    getElementById: id => id === 'appearance' ? control : null,
    addEventListener: (name, fn) => { events[name] = fn; },
  };
  vm.runInNewContext(source, { window: { matchMedia: () => system, localStorage: storage }, document });
  return {
    dataset, control, writes,
    ready: () => events.DOMContentLoaded?.(),
    change: value => { control.value = value; events.change(); },
    system: dark => { system.matches = dark; listeners.change(); },
  };
}

test('system appearance resolves before the control exists and follows OS changes', () => {
  const f = fixture({ dark: true });
  assert.equal(f.dataset.theme, 'dark');
  f.ready(); assert.equal(f.control.value, 'system');
  f.system(false); assert.equal(f.dataset.theme, 'light');
  f.system(true); assert.equal(f.dataset.theme, 'dark');
  assert.equal(f.writes.length, 0, 'following the system does not create an override');
});

test('explicit appearance persists across reloads and overrides OS changes', () => {
  const f = fixture({ dark: false }); f.ready(); f.change('dark');
  assert.equal(f.dataset.theme, 'dark');
  assert.deepEqual(f.writes, [['harbor-appearance', 'dark']]);
  f.system(false); assert.equal(f.dataset.theme, 'dark');
  const reopened = fixture({ saved: f.writes[0][1], dark: false, loading: false });
  assert.equal(reopened.dataset.theme, 'dark'); assert.equal(reopened.control.value, 'dark');
  reopened.change('light'); reopened.system(true); assert.equal(reopened.dataset.theme, 'light');
  reopened.change('system'); assert.equal(reopened.dataset.theme, 'dark');
});

test('unavailable storage and invalid stored choices fall back safely', () => {
  const blocked = fixture({ blocked: true, dark: true }); blocked.ready();
  assert.equal(blocked.dataset.theme, 'dark'); blocked.change('light');
  assert.equal(blocked.dataset.theme, 'light');
  const invalid = fixture({ saved: '<invalid>', dark: true }); invalid.ready();
  assert.equal(invalid.control.value, 'system'); assert.equal(invalid.dataset.theme, 'dark');
  invalid.change('not-a-theme'); assert.equal(invalid.dataset.theme, 'dark');
});
