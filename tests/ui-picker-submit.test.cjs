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
  if (url.startsWith('/api/upload?')) {
   const query = new URL(url, 'http://localhost').searchParams;
   const request = {ProfileID:query.get('profile'),Bucket:query.get('bucket'),Key:query.get('key'),ConnectionIdentity:query.get('identity'),file:options.body};
   if(fixture.onUpload) await fixture.onUpload(request);
   fixture.uploads.push(request); return {ok:true,json:async()=> 'fixture-upload'};
  }
  const route = url.slice(url.lastIndexOf('/') + 1), request = JSON.parse(options.body);
  if (held?.route === route && held.path === request.Path) await held.promise;
  let value;
  if (route === 'profiles') value = [{ID: 'fixture-profile', Name: 'Fixture', Identity: 'fixture-profile-identity'}];
  else if (route === 'buckets') value = ['fixture-bucket'];
  else if (route === 'browse') value = {Objects: [{Key: 'report.txt', Size: 4, ETag: 'fixture-etag', Folder: false}], Next: '', ConnectionIdentity: 'fixture-listing-identity'};
  else if (route === 'save-locations') {
   const folder = request.Path || '/fixture/save';
   value = {Path: folder, Home: '/fixture/save', Parent: '/fixture/save', Folders: folder === '/fixture/save' ? ['subfolder'] : []};
  } else if (route === 'upload-locations') {
   const folder = request.Path || '/fixture/upload';
   value = {Path: folder, Home: '/fixture/upload', Parent: '/fixture/upload', Folders: folder === '/fixture/upload' ? ['subfolder'] : [], Files: [{Name: 'upload.txt', Size: 4}]};
  } else if (route === 'download') { fixture.downloads.push(request); value = {ID: 'fixture-download', Path: request.Path + '/' + request.Name}; }
  else if (route === 'upload-local') { if(fixture.onUpload)await fixture.onUpload(request);fixture.uploads.push(request); value = 'fixture-upload'; }
  else if (route === 'desktop-upload-pick') { value=fixture.onPicker?await fixture.onPicker(request):[]; }
  else if (route === 'transfers') { if(fixture.onTransfers) await fixture.onTransfers(); value = []; }
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
 check(get('save-confirm').disabled, 'Save here is disabled while folder navigation is pending');
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
 check(fixture.downloads[0].ConnectionIdentity === 'fixture-listing-identity', 'Save uses the actual reviewed listing identity');
 check(!get('save-dialog').open, 'Successful Save closes the dialog');
}

function pickUpload(document, id, files) {
 const input=document.getElementById(id);
 Object.defineProperty(input,'files',{configurable:true,value:files});input.onchange();
}
async function exerciseUpload(document, fixture, check) {
 const get=id=>document.getElementById(id);
 get('upload').click();await settle();
 check(get('upload-dialog').open,'Upload opens the review workspace');
 check(get('upload-confirm').disabled,'Upload requires a selection');
 const first={name:'upload.txt',size:4},nested={name:'data.txt',size:7,webkitRelativePath:'reports/nested/data.txt'};
 pickUpload(document,'upload-file-input',[first]);
 pickUpload(document,'upload-folder-input',[nested]);
 check(get('upload-rows').children.length===2,'Files and folders accumulate in one review list');
 check(fixture.uploads.length===0&&fixture.uploadSubmits===0,'Selecting files never submits');
 check(get('upload-destination').textContent==='s3://fixture-bucket/','Exact destination is visible');
 check(get('upload-confirm').type==='submit','Upload is an explicit submit control');
 get('upload-confirm').click();await settle();
 check(fixture.uploadSubmits===1,'Explicit Upload submits exactly once');
 check(fixture.confirmations.length===0,'The review screen is the single confirmation');
 check(fixture.uploads.length===2,'Both selected files are queued');
 check(fixture.uploads[0].file===first,'Upload uses the selected file bytes');
 check(fixture.uploads[1].Key==='reports/nested/data.txt','Folder paths are preserved');
 check(fixture.uploads[0].Bucket==='fixture-bucket','Destination remains bound to the reviewed bucket');
 check(!get('upload-dialog').open,'Successful uploads close the workspace');
}

async function modelFixture(native=false) {
 const document = modelDocument(), fixture = fixtureAPI();
 const context = {document, fetch: fixture.fetch, confirm: fixture.confirm, location: {hash: '#fixture', pathname: '/'}, sessionStorage: {getItem() {}, setItem() {}}, history: {replaceState() {}}, setInterval() {}, setTimeout, clearTimeout, URL, URLSearchParams};
 if(native)context.window={runtime:{}};
 vm.createContext(context); vm.runInContext(source, context);
 recordSubmissions(document, fixture); await selectFixture(document);
 return {document, fixture, context};
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

test('desktop folder picker stages by local path, preserves nested keys and retries failed preparation',async()=>{
 const {document,fixture}=await modelFixture(true),get=id=>document.getElementById(id);
 fixture.onPicker=async request=>{assert.equal(request.Folder,true);return [{name:'large.bin',size:731*1024**2,path:'Downloads/nested/large.bin',localPath:'C:\\Downloads\\nested\\large.bin'}]};
 get('upload').click();await get('upload-add-folder').onclick();
 assert.equal(get('upload-rows').children.length,1);assert.equal(fixture.uploads.length,0);
 fixture.onUpload=async()=>{throw Error('File is no longer available')};
 await get('upload-form').onsubmit({preventDefault(){}});
 assert.match(get('upload-error').textContent,/File is no longer available/);assert.equal(get('upload-confirm').disabled,false);
 fixture.onUpload=null;await get('upload-form').onsubmit({preventDefault(){}});
 assert.equal(fixture.uploads.length,1);assert.equal(fixture.uploads[0].Key,'Downloads/nested/large.bin');assert.equal(fixture.uploads[0].Path,'C:\\Downloads\\nested\\large.bin');assert.equal(fixture.uploads[0].file,undefined);assert.equal(fixture.uploads[0].ConnectionIdentity,'fixture-listing-identity');
 assert.equal(get('upload-dialog').open,false);
});

test('desktop picker errors and late results leave the dialog usable',async()=>{
 const {document,fixture}=await modelFixture(true),get=id=>document.getElementById(id);
 get('upload').click();fixture.onPicker=async()=>{throw Error('Cannot read folder')};
 await get('upload-add-folder').onclick();assert.match(get('upload-error').textContent,/Cannot read folder/);assert.equal(get('upload-add-folder').disabled,false);
 let release;fixture.onPicker=()=>new Promise(resolve=>release=resolve);
 const pending=get('upload-add-folder').onclick();await settle();get('upload-cancel').click();get('upload').click();
 release([{name:'late.txt',size:1,path:'Downloads/late.txt',localPath:'C:\\Downloads\\late.txt'}]);await pending;
 assert.equal(get('upload-dialog').open,true);assert.equal(get('upload-rows').children.length,0);assert.equal(get('upload-add-folder').disabled,false);
});

test('desktop large dropped files direct users to native pickers before reading bytes',async()=>{
 const {document,fixture}=await modelFixture(true),get=id=>document.getElementById(id);
 get('upload').click();pickUpload(document,'upload-file-input',[{name:'large.bin',size:731*1024**2}]);
 assert.equal(get('upload-rows').children.length,0);assert.match(get('upload-error').textContent,/use Add files or Add folder/);assert.equal(fixture.uploads.length,0);
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
  const after = `${settle}\n${selectFixture}\n${recordSubmissions}\n${exerciseSave}\n${pickUpload}\n${exerciseUpload}\n(async () => {
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

test('upload selection rejects duplicates and oversize files; removal and Cancel never upload',async()=>{
 const {document,fixture}=await modelFixture(),get=id=>document.getElementById(id);
 get('upload').click();await settle();
 pickUpload(document,'upload-file-input',[{name:'one.txt',size:2},{name:'one.txt',size:3},{name:'too-large.bin',size:5*1024**3+1}]);
 assert.equal(get('upload-rows').children.length,1);assert.match(get('upload-error').textContent,/already in the list.*5 GiB/s);
 get('upload-rows').children[0].children[4].children[0].click();assert.equal(get('upload-confirm').disabled,true);
 pickUpload(document,'upload-file-input',[{name:'two.txt',size:0}]);get('upload-clear').click();assert.equal(get('upload-rows').children.length,0);
 pickUpload(document,'upload-file-input',[{name:'two.txt',size:0}]);get('upload-cancel').click();assert.equal(fixture.uploads.length,0);
 get('upload').click();await settle();assert.equal(get('upload-confirm').disabled,true,'cancelled draft is discarded');
});

test('batch failure preserves failed files and retries without duplicating successful uploads',async()=>{
 const {document,fixture}=await modelFixture(),get=id=>document.getElementById(id);
 get('upload').click();await settle();pickUpload(document,'upload-file-input',[{name:'ok.txt',size:1},{name:'failed.txt',size:2}]);
 fixture.onUpload=async request=>{if(request.Key==='failed.txt')throw Error('Connection interrupted')};
 await get('upload-form').onsubmit({preventDefault(){}});
 assert.equal(fixture.uploads.length,1);assert.equal(get('upload-dialog').open,true);assert.equal(get('upload-rows').children.length,1);assert.match(get('upload-error').textContent,/failed.txt: Connection interrupted/);
 fixture.onUpload=null;await get('upload-form').onsubmit({preventDefault(){}});
 assert.deepEqual(fixture.uploads.map(x=>x.Key),['ok.txt','failed.txt']);assert.equal(get('upload-dialog').open,false);
});

test('Stop after this file preserves already queued work and leaves remaining files for review',async()=>{
 const {document,fixture}=await modelFixture(),get=id=>document.getElementById(id);
 get('upload').click();await settle();pickUpload(document,'upload-file-input',[{name:'first.txt',size:1},{name:'later.txt',size:2}]);
 let release;fixture.onUpload=()=>new Promise(resolve=>release=resolve);
 const pending=get('upload-form').onsubmit({preventDefault(){}});await settle();
 get('upload-cancel').click();release();await pending;
 assert.equal(fixture.uploads.length,1);assert.equal(get('upload-dialog').open,true);assert.equal(get('upload-rows').children.length,1);assert.match(get('upload-error').textContent,/Already queued transfers continue/);
});

test('dropped folder reads every directory page and preserves nested paths',async()=>{
 const {document,fixture}=await modelFixture(),get=id=>document.getElementById(id);
 get('upload').click();await settle();
 const file=name=>({isFile:true,name,file:resolve=>resolve({name,size:2})});
 const directory=(name,batches)=>({isDirectory:true,name,createReader:()=>({readEntries:resolve=>resolve(batches.shift()||[])})});
 const entry=directory('reports',[[file('a.txt')],[directory('nested',[[file('b.txt')]])]]);
 await get('upload-dropzone').ondrop({preventDefault(){},dataTransfer:{items:[{kind:'file',webkitGetAsEntry:()=>entry}]}});
 assert.equal(get('upload-rows').children.length,2);await get('upload-form').onsubmit({preventDefault(){}});
 assert.deepEqual(fixture.uploads.map(x=>x.Key),['reports/a.txt','reports/nested/b.txt']);
});

test('late dropped-directory results cannot populate a cancelled or reopened draft',async()=>{
 const {document}=await modelFixture(),get=id=>document.getElementById(id);get('upload').click();await settle();
 let release;const entry={isFile:true,name:'late.txt',file:resolve=>release=()=>resolve({name:'late.txt',size:1})};
 const pending=get('upload-dropzone').ondrop({preventDefault(){},dataTransfer:{items:[{kind:'file',webkitGetAsEntry:()=>entry}]}});
 get('upload-cancel').click();get('upload').click();await settle();release();await pending;
 assert.equal(get('upload-rows').children.length,0);assert.equal(get('upload-confirm').disabled,true);
});


test('upload keeps the reviewed destination and encodes special characters exactly',async()=>{
 const {document,fixture,context}=await modelFixture(),get=id=>document.getElementById(id);
 vm.runInContext("prefix='reviewed/';",context);get('upload').click();await settle();
 pickUpload(document,'upload-file-input',[{name:'a & + # ü.txt',size:1}]);
 vm.runInContext("bucket='elsewhere';prefix='changed/';",context);
 await get('upload-form').onsubmit({preventDefault(){}});
 assert.equal(fixture.uploads[0].Bucket,'fixture-bucket');assert.equal(fixture.uploads[0].Key,'reviewed/a & + # ü.txt');
 assert.equal(fixture.uploads[0].ConnectionIdentity,'fixture-profile-identity','without a current listing, upload captures the selected profile identity');
});

test('upload retains the actual listing identity through profile edits and navigation',async()=>{
 const {document,fixture,context}=await modelFixture(),get=id=>document.getElementById(id);get('upload').click();await settle();pickUpload(document,'upload-file-input',[{name:'reviewed.txt',size:1}]);
 vm.runInContext("connectionProfiles[0].Identity='replacement-identity';bucket='elsewhere';prefix='changed/';",context);await get('upload-form').onsubmit({preventDefault(){}});
 assert.equal(fixture.uploads[0].ConnectionIdentity,'fixture-listing-identity');assert.equal(fixture.uploads[0].Bucket,'fixture-bucket');assert.equal(fixture.uploads[0].Key,'reviewed.txt');
});


test('a delayed status refresh cannot close or overwrite a newer upload draft',async()=>{
 const {document,fixture}=await modelFixture(),get=id=>document.getElementById(id);get('upload').click();await settle();
 pickUpload(document,'upload-file-input',[{name:'first.txt',size:1}]);
 let release;fixture.onTransfers=()=>new Promise(resolve=>release=resolve);
 const previous=get('upload-form').onsubmit({preventDefault(){}});await settle();
 assert.equal(get('upload-dialog').open,false,'completed draft finalizes before polling');
 get('upload').click();await settle();pickUpload(document,'upload-file-input',[{name:'new-draft.txt',size:2}]);
 release();await previous;
 assert.equal(get('upload-dialog').open,true);assert.equal(get('upload-rows').children.length,1);assert.match(get('upload-rows').textContent,/new-draft.txt/);assert.equal(get('upload-confirm').disabled,false);
});
