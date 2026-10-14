// Run: node --test tests/ui-picker-submit.test.cjs
// Optional native coverage: HARBOR_TEST_CHROMIUM=/path/to/chromium (otherwise autodetected).
// Every API is mocked; Chromium uses a disposable profile and never opens Harbor.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const os = require('node:os');
const {spawnSync} = require('node:child_process');
const {pathToFileURL} = require('node:url');

const markup = fs.readFileSync(path.join(__dirname, '../frontend/index.html'), 'utf8');
const source = fs.readFileSync(path.join(__dirname, '../frontend/app.js'), 'utf8');

// This small DOM model covers only the activation semantics needed here: an
// untyped button defaults to submit, and submission follows the synchronous click
// handler without waiting for its promise. The optional Chromium test below also
// exercises native form ownership, constraint validation, and browser events.
class Element {
 constructor(tag = 'div') {
  this.tag = tag; this.children = []; this.dataset = {}; this.attributes = {};
  this.listeners = {}; this.value = ''; this.disabled = false; this.classes = new Set();
  this.classList = {add: value => this.classes.add(value), remove: value => this.classes.delete(value)};
 }
 get type() { return this.attributes.type || (this.tag === 'button' ? 'submit' : 'text'); }
 set type(value) { this.attributes.type = value; }
 set textContent(value) { this.text = String(value); this.replaceChildren(); }
 get textContent() { return this.children.length ? this.children.map(e => e.textContent).join('') : this.text || ''; }
 append(...elements) { for (const e of elements) { e.parentElement = this; this.children.push(e); } }
 replaceChildren(...elements) { for (const e of this.children) e.parentElement = null; this.children = []; this.append(...elements); }
 remove() { if (this.parentElement) this.parentElement.children = this.parentElement.children.filter(e => e !== this); this.parentElement = null; }
 setAttribute(name, value) {
  this.attributes[name] = value;
  if (['disabled', 'hidden', 'required'].includes(name)) this[name] = true;
  if (['id', 'value'].includes(name)) this[name] = value;
 }
 addEventListener(name, listener) { (this.listeners[name] ||= []).push(listener); }
 closest(tag) { for (let e = this; e; e = e.parentElement) if (e.tag === tag) return e; return null; }
 showModal() { this.open = true; }
 close() { this.open = false; }
 dispatch(name, properties = {}) {
  const event = {target: this, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; }, stopPropagation() {}, ...properties};
  this['on' + name]?.(event);
  for (const listener of this.listeners[name] || []) listener(event);
  return event;
 }
 click() {
  if (this.disabled) return;
  const event = this.dispatch('click');
  if (!event.defaultPrevented && this.tag === 'button' && this.type === 'submit') {
   this.closest('form')?.dispatch('submit', {submitter: this});
  }
 }
}

function modelDocument() {
 const elements = new Map(), root = new Element('document'), stack = [root];
 const voidTags = new Set(['area', 'base', 'br', 'col', 'embed', 'hr', 'img', 'input', 'link', 'meta', 'param', 'source', 'track', 'wbr']);
 // The real HTML supplies the form ancestry and explicit button attributes.
 for (const token of markup.matchAll(/<(\/?)([a-z][\w-]*)\b([^>]*)>/gi)) {
  const [, closing, tag, attributes] = token;
  if (closing) { stack.pop(); continue; }
  const element = new Element(tag);
  for (const match of attributes.matchAll(/([^\s=]+)(?:="([^"]*)")?/g)) element.setAttribute(match[1], match[2] || '');
  if (element.id) elements.set(element.id, element);
  stack.at(-1).append(element);
  if (!voidTags.has(tag)) stack.push(element);
 }
 return {getElementById: id => elements.get(id), createElement: tag => new Element(tag)};
}

function fixtureAPI() {
 const fixture = {downloads: [], uploads: [], confirmations: [], saveSubmits: 0, uploadSubmits: 0};
 let held;
 fixture.holdLocation = (route, path) => {
  let release;
  const promise = new Promise(resolve => release = resolve);
  held = {route, path, promise};
  return () => { held = null; release(); };
 };
 fixture.confirm = text => { fixture.confirmations.push(text); return true; };
 fixture.fetch = async (url, options) => {
  const route = url.slice(url.lastIndexOf('/') + 1), request = JSON.parse(options.body);
  if (held?.route === route && held.path === request.Path) await held.promise;
  let value;
  if (route === 'profiles') value = [{ID: 'fixture-profile', Name: 'Fixture'}];
  else if (route === 'buckets') value = ['fixture-bucket'];
  else if (route === 'browse') value = {Objects: [{Key: 'report.txt', Size: 4, ETag: 'fixture-etag', Folder: false}], Next: ''};
  else if (route === 'save-locations') {
   const folder = request.Path || '/fixture/save';
   value = {Path: folder, Home: '/fixture/save', Parent: '/fixture/save', Folders: folder === '/fixture/save' ? ['subfolder'] : []};
  } else if (route === 'upload-locations') {
   const folder = request.Path || '/fixture/upload';
   value = {Path: folder, Home: '/fixture/upload', Parent: '/fixture/upload', Folders: folder === '/fixture/upload' ? ['subfolder'] : [], Files: [{Name: 'upload.txt', Size: 4}]};
  } else if (route === 'download') { fixture.downloads.push(request); value = {ID: 'fixture-download', Path: request.Path + '/' + request.Name}; }
  else if (route === 'upload-local') { fixture.uploads.push(request); value = 'fixture-upload'; }
  else if (route === 'transfers') value = [];
  else throw Error('Unexpected fixture route: ' + route);
  return {ok: true, json: async () => value};
 };
 return fixture;
}

async function settle() { await new Promise(resolve => setTimeout(resolve, 0)); }

async function selectFixture(document) {
 const get = id => document.getElementById(id);
 await settle(); get('profiles').children[0].children[0].click();
 await settle(); get('buckets').children[0].children[0].click();
 await settle(); get('objects').children[0].click(); await settle();
}

function recordSubmissions(document, fixture) {
 document.getElementById('save-form').addEventListener('submit', () => fixture.saveSubmits++);
 document.getElementById('upload-form').addEventListener('submit', () => fixture.uploadSubmits++);
}

async function exerciseSave(document, fixture, check) {
 const get = id => document.getElementById(id);
 get('download').click(); await settle();
 check(get('save-dialog').open, 'Download opens Save');
 check(fixture.downloads.length === 0, 'Opening Save does not queue a download');
 const release = fixture.holdLocation('save-locations', '/fixture/save/subfolder');
 get('save-folders').children[0].click(); await settle();
 check(fixture.saveSubmits === 0, 'Choosing a Save subfolder must not submit while its listing is pending');
 check(fixture.downloads.length === 0, 'Choosing a Save subfolder must not queue into the previous folder');
 release(); await settle();
 check(get('save-path').value === '/fixture/save/subfolder', 'Save navigation reaches the chosen folder');
 check(get('save-dialog').open, 'Save remains open after navigation');
 check(fixture.downloads.length === 0, 'Completed Save navigation does not queue');
 check(get('save-confirm').type === 'submit', 'Save here remains an explicit submit control');
 get('save-confirm').click(); await settle();
 check(fixture.saveSubmits === 1, 'Save here submits exactly once');
 check(fixture.downloads.length === 1, 'Save here queues exactly one download');
 check(fixture.downloads[0].Path === '/fixture/save/subfolder', 'Save here uses the chosen folder');
 check(fixture.downloads[0].Name === 'report.txt', 'Save here retains the filename');
 check(!get('save-dialog').open, 'Successful Save closes the dialog');
}

async function exerciseUpload(document, fixture, check) {
 const get = id => document.getElementById(id);
 get('upload').click(); await settle();
 check(get('upload-dialog').open, 'Upload opens the picker');
 check(get('upload-confirm').disabled, 'Upload is disabled before file selection');
 get('upload-files').children[1].click(); await settle();
 check(!get('upload-confirm').disabled, 'Selecting a file enables Upload');
 check(fixture.uploadSubmits === 0, 'Selecting an upload file must not submit');
 check(fixture.uploads.length === 0 && fixture.confirmations.length === 0, 'File selection must not queue or prompt for upload');
 const release = fixture.holdLocation('upload-locations', '/fixture/upload/subfolder');
 get('upload-files').children[0].click(); await settle();
 check(fixture.uploadSubmits === 0, 'Upload folder navigation must not submit with the previous file selection');
 check(fixture.uploads.length === 0, 'Upload folder navigation must not queue the previous file');
 release(); await settle();
 check(get('upload-path').value === '/fixture/upload/subfolder', 'Upload navigation reaches the chosen folder');
 check(get('upload-confirm').disabled, 'Folder navigation clears the previous file selection');
 get('upload-files').children[0].click(); await settle();
 check(fixture.uploadSubmits === 0 && fixture.uploads.length === 0, 'Selecting the new file still waits for explicit Upload');
 check(get('upload-confirm').type === 'submit', 'Upload remains an explicit submit control');
 get('upload-confirm').click(); await settle();
 check(fixture.uploadSubmits === 1, 'Explicit Upload submits exactly once');
 check(fixture.confirmations.length === 1, 'Explicit Upload prompts exactly once');
 check(fixture.uploads.length === 1, 'Explicit Upload queues exactly one upload');
 check(fixture.uploads[0].Path === '/fixture/upload/subfolder/upload.txt', 'Upload uses the chosen local file');
 check(fixture.uploads[0].Bucket === 'fixture-bucket' && fixture.uploads[0].Key === 'upload.txt', 'Upload uses the intended storage destination');
 check(!get('upload-dialog').open, 'Successful Upload closes the picker');
}

async function modelFixture() {
 const document = modelDocument(), fixture = fixtureAPI();
 const context = {document, fetch: fixture.fetch, confirm: fixture.confirm, location: {hash: '#fixture', pathname: '/'}, sessionStorage: {getItem() {}, setItem() {}}, history: {replaceState() {}}, setInterval() {}, setTimeout, clearTimeout, URL, URLSearchParams};
 vm.createContext(context); vm.runInContext(source, context);
 recordSubmissions(document, fixture); await selectFixture(document);
 return {document, fixture};
}

test('default-submit model activates before an async click handler finishes', async () => {
 const form = new Element('form'), button = new Element('button'), events = [];
 form.append(button); form.onsubmit = () => events.push('submit');
 button.onclick = async () => { await Promise.resolve(); events.push('click finished'); };
 button.click(); assert.deepEqual(events, ['submit']);
 await settle(); assert.deepEqual(events, ['submit', 'click finished']);
 events.length = 0; button.type = 'button'; button.click();
 assert.deepEqual(events, []); await settle(); assert.deepEqual(events, ['click finished']);
 button.type = 'submit'; events.length = 0; button.onclick = e => e.preventDefault(); button.click();
 assert.deepEqual(events, [], 'preventDefault suppresses submission');
 button.onclick = undefined; button.disabled = true; button.click();
 assert.deepEqual(events, [], 'disabled buttons do not activate');
});

test('Save folder buttons navigate without submitting; Save here queues the chosen folder (DOM activation model)', async () => {
 const {document, fixture} = await modelFixture(); await exerciseSave(document, fixture, assert.ok);
});

test('Upload file and folder buttons wait for explicit Upload (DOM activation model)', async () => {
 const {document, fixture} = await modelFixture(); await exerciseUpload(document, fixture, assert.ok);
});

function findChromium() {
 if (process.env.HARBOR_TEST_CHROMIUM) {
  assert.ok(fs.existsSync(process.env.HARBOR_TEST_CHROMIUM), 'HARBOR_TEST_CHROMIUM must name an existing Chromium executable');
  return process.env.HARBOR_TEST_CHROMIUM;
 }
 const candidates = (process.env.PATH || '').split(path.delimiter).flatMap(directory => ['chromium', 'chromium-browser', 'google-chrome', 'chrome-headless-shell'].map(name => path.join(directory, name)));
 const cache = process.env.PLAYWRIGHT_BROWSERS_PATH || (process.platform === 'darwin' ? path.join(os.homedir(), 'Library/Caches/ms-playwright') : process.platform === 'win32' ? path.join(process.env.LOCALAPPDATA || os.homedir(), 'ms-playwright') : path.join(os.homedir(), '.cache/ms-playwright'));
 if (fs.existsSync(cache)) {
  for (const entry of fs.readdirSync(cache).filter(name => /^chromium/.test(name)).sort().reverse()) {
   for (const binary of ['chrome-headless-shell-mac-arm64/chrome-headless-shell', 'chrome-headless-shell-mac-x64/chrome-headless-shell', 'chrome-headless-shell-linux64/chrome-headless-shell', 'chrome-headless-shell-win64/chrome-headless-shell.exe', 'chrome-linux/chrome', 'chrome-linux64/chrome', 'chrome-win/chrome.exe']) candidates.push(path.join(cache, entry, binary));
  }
 }
 if (process.platform === 'darwin') candidates.push('/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', '/Applications/Chromium.app/Contents/MacOS/Chromium');
 return candidates.find(candidate => fs.existsSync(candidate));
}

test('picker navigation and explicit confirmation use native browser form activation (headless Chromium)', {timeout: 25000}, t => {
 const browser = findChromium();
 if (!browser) { t.skip('Chromium unavailable; set HARBOR_TEST_CHROMIUM to run native browser coverage. DOM activation regressions still run.'); return; }
 const temporary = fs.mkdtempSync(path.join(os.tmpdir(), 'harbor-picker-test-'));
 try {
  const before = `const fixture = (${fixtureAPI})(); window.fetch = fixture.fetch; window.confirm = fixture.confirm; window.setInterval = () => 0;`;
  const after = `${settle}\n${selectFixture}\n${recordSubmissions}\n${exerciseSave}\n${exerciseUpload}\n(async () => {
   const output = document.createElement('pre'); output.id = 'picker-test-result'; document.body.append(output);
   try { recordSubmissions(document, fixture); await selectFixture(document); const check = (condition, message) => { if (!condition) throw Error(message); }; await exerciseSave(document, fixture, check); await exerciseUpload(document, fixture, check); output.textContent = JSON.stringify({ok: true}); }
   catch (error) { output.textContent = JSON.stringify({ok: false, error: error.message, stack: error.stack}); }
  })();`;
  const inline = code => '<script>' + code.replace(/<\/script/gi, '<\\/script') + '</script>';
  const html = markup.replace(/<link\b[^>]*>/gi, '').replace('<script src="/app.js"></script>', inline(before) + inline(source) + inline(after));
  assert.ok(html.includes('picker-test-result'), 'Actual HTML must include the injected browser scenario');
  const filename = path.join(temporary, 'fixture.html'); fs.writeFileSync(filename, html);
  const result = spawnSync(browser, ['--headless', '--no-sandbox', '--disable-gpu', '--disable-background-networking', '--disable-extensions', '--disable-component-update', '--disable-sync', '--no-first-run', '--no-default-browser-check', '--host-resolver-rules=MAP * ~NOTFOUND', '--user-data-dir=' + path.join(temporary, 'profile'), '--dump-dom', '--virtual-time-budget=4000', pathToFileURL(filename).href + '#fixture'], {encoding: 'utf8', timeout: 20000, maxBuffer: 2 * 1024 * 1024});
  assert.ifError(result.error); assert.equal(result.status, 0, result.stderr);
  const output = result.stdout.match(/<pre id="picker-test-result">([\s\S]*?)<\/pre>/);
  assert.ok(output, 'Browser must produce a test result: ' + result.stderr);
  const report = JSON.parse(output[1].replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&'));
  assert.equal(report.ok, true, report.error + '\n' + report.stack);
 } finally { fs.rmSync(temporary, {recursive: true, force: true}); }
});
