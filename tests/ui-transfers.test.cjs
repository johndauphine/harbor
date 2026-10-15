const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
class Element {
 constructor(tag='div'){this.tag=tag;this.children=[];this.dataset={};this.attributes={};this.classList={add(){},remove(){}};this.value='';}
 set value(value){this._value=value;if(this.tag==='progress')this.attributes.value=String(value);}
 get value(){return this._value;}
 set textContent(value){this.text=String(value);this.children=[];}
 get textContent(){return this.children.length?this.children.map(child=>child.textContent).join(''):this.text||'';}
 append(e){if(e.parentElement)e.parentElement.children=e.parentElement.children.filter(child=>child!==e);e.parentElement=this;this.children.push(e);}
 replaceChildren(...elements){this.text='';this.children=[];elements.forEach(e=>this.append(e));}
 remove(){if(this.parentElement)this.parentElement.children=this.parentElement.children.filter(e=>e!==this);}
 showModal(){this.open=true;}
 close(){this.open=false;}
 setAttribute(key,value){this.attributes[key]=String(value);}
 removeAttribute(key){delete this.attributes[key];if(key==='value')this._value='';}
 closest(){return false;}
 click(){return this.onclick?.();}
}
test('terminal transfers refresh once, preserve selection, and report failures and retries', async()=>{
 const elements=new Map();const get=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id);};
 let transfers=[],browseCalls=0,downloadRequests=[],uploadRequests=[],retryRequests=[];
 const context={document:{getElementById:get,createElement:tag=>new Element(tag)},location:{hash:'#fixture',pathname:'/'},sessionStorage:{getItem(){},setItem(){}},history:{replaceState(){}},setInterval(){},setTimeout,clearTimeout,URL,URLSearchParams,confirm:()=>true,
 fetch:async(url,options)=>{
  let result;
  if(url.endsWith('/profiles'))result=[{ID:'profile',Name:'Fixture'}];
  else if(url.endsWith('/buckets'))result=['bucket'];
  else if(url.endsWith('/upload-locations'))result={Path:'/Downloads',Home:'/home',Parent:'/',Folders:[],Files:[{Name:'fixture.txt',Size:3}]};
  else if(url.startsWith('/api/upload?')){const query=new URL(url,'http://localhost').searchParams;uploadRequests.push({Bucket:query.get('bucket'),Key:query.get('key'),file:options.body});result='upload-id';}
  else if(url.endsWith('/save-locations'))result={Path:'/Downloads',Home:'/home',Parent:'/',Folders:[]};
  else if(url.endsWith('/download')){downloadRequests.push(JSON.parse(options.body));result={ID:'new',Path:'/Downloads/fixture.txt'};}
  else if(url.endsWith('/transfers'))result=transfers.map(t=>({...t}));
  else if(url.endsWith('/retry')){retryRequests.push(JSON.parse(options.body));result='queued';}
  else if(url.endsWith('/browse')){browseCalls++;result={Objects:[{Key:'fixture.txt',Size:3,Folder:false,ETag:'etag'}],Next:''};}
  else throw Error('Unexpected endpoint '+url);
  return {ok:true,json:async()=>result};
 }};
 vm.createContext(context);vm.runInContext(fs.readFileSync(path.join(__dirname,'../frontend/app.js'),'utf8'),context);
 await new Promise(r=>setImmediate(r));
 await get('profiles').children[0].children[0].click();await get('buckets').children[0].children[0].click();await get('objects').children[0].click();
 await get('download').click();assert.equal(get('save-dialog').open,true);assert.equal(downloadRequests.length,0,'opening Save As must not queue');assert.equal(get('save-name').value,'fixture.txt');
 get('save-cancel').click();assert.equal(downloadRequests.length,0,'cancelling must not queue');
 await get('download').click();await get('save-form').onsubmit({preventDefault(){}});
 assert.equal(downloadRequests.length,1);assert.equal(downloadRequests[0].Path,'/Downloads');assert.equal(downloadRequests[0].Key,'fixture.txt');assert.equal(get('save-dialog').open,false);
 await get('upload').click();assert.equal(get('upload-dialog').open,true);assert.equal(uploadRequests.length,0);assert.equal(get('upload-confirm').disabled,true);
 get('upload-file-input').files=[{name:'fixture.txt',size:3}];get('upload-file-input').onchange();assert.equal(get('upload-confirm').disabled,false);
 await get('upload-form').onsubmit({preventDefault(){}});assert.equal(uploadRequests.length,1);assert.equal(uploadRequests[0].Key,'fixture.txt');assert.equal(uploadRequests[0].Bucket,'bucket');assert.equal(get('upload-dialog').open,false);
 const transfer={ID:'upload',Direction:'upload',ProfileID:'profile',Bucket:'bucket',Key:'fixture.txt',Done:0,Total:3,Status:'running',Error:''};transfers=[transfer];
 await context.transfers();assert.equal(browseCalls,1);
 transfer.Status='completed';transfer.Done=3;await context.transfers();assert.equal(browseCalls,2);assert.match(get('message').textContent,/Upload completed/);assert.match(get('details').textContent,/Selected: fixture.txt/);
 const stableRow=get('transfers').children[0],stableControls=stableRow.children[1];
 await context.transfers();assert.equal(browseCalls,2,'completed history must not repeatedly refresh');
 assert.equal(get('transfers').children[0],stableRow,'polling must reuse the row');assert.equal(stableRow.children[1],stableControls,'polling must retain controls');
 transfers=[{...transfer,ID:'retry',Status:'failed',Error:'Access denied'}];await context.transfers();assert.equal(browseCalls,2);assert.match(get('message').textContent,/Upload failed.*Access denied/);
 transfers[0].Status='running';await context.transfers();transfers[0].Status='completed';await context.transfers();assert.equal(browseCalls,3,'successful retry refreshes');
 transfers=[{...transfer,ID:'download',Direction:'download'}];await context.transfers();assert.equal(browseCalls,4);assert.match(get('message').textContent,/Download saved/);
 const downloadRow=get('transfers').children[0];assert.equal(downloadRow.children[1].children.length,0,'no second Save file step');
 transfers[0].SavedPath='/chosen/file.txt';transfers[0].SavedAt='2026-10-14T18:00:00Z';transfers[0].CreatedAt='2026-10-14T17:59:00Z';
 await context.transfers();await context.transfers();
 assert.match(downloadRow.children[2].textContent,/Saved to \/chosen\/file.txt/);
 assert.match(downloadRow.children[3].textContent,/Queued /);

 transfers[0].SaveError='Permission denied';await context.transfers();assert.match(downloadRow.children[2].textContent,/Save failed: Permission denied/);
 transfers=[{...transfer,ID:'elsewhere',Bucket:'other-bucket'}];await context.transfers();assert.equal(browseCalls,4,'unrelated bucket must not replace current view');
 get('path').value='first folder/second/';await get('go').click();
 assert.deepEqual(get('location').children.map(e=>e.textContent),['bucket','›','first folder','›','second']);
 await get('location').children[2].click();assert.equal(get('path').value,'first folder/');
 await get('location').children[0].click();assert.equal(get('path').value,'');
 get('path').value='first//last/';await get('go').click();await get('location').children[4].click();assert.equal(get('path').value,'first//','preserve exact S3 prefixes');
 await get('profiles').children[0].children[0].click();assert.equal(get('location').children.length,0,'clear stale breadcrumbs when switching connections');


 transfers=[{...transfer,ID:'restored',Status:'failed',Error:'Interrupted when Harbor stopped. Retry to restart this transfer.',CleanupError:'Remote multipart cleanup pending'}];
 await context.transfers();
 const restoredRow=get('transfers').children[0];
 assert.match(restoredRow.children[0].textContent,/Interrupted when Harbor stopped/);
 assert.match(restoredRow.children[2].textContent,/Remote multipart cleanup pending/);
 assert.match(restoredRow.children[2].className,/transfer-save-error/);
 assert.equal(restoredRow.children[1].children[0].textContent,'Retry');
 await restoredRow.children[1].children[0].click();
 assert.deepEqual(retryRequests,[{ID:'restored'}]);
});

async function transferFixture(){
 const elements=new Map(),get=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id);};
 let list=[];const requests=[];
 const context={document:{getElementById:get,createElement:tag=>new Element(tag)},location:{hash:'#fixture',pathname:'/'},sessionStorage:{getItem(){},setItem(){}},history:{replaceState(){}},setInterval(){},setTimeout,clearTimeout,URL,URLSearchParams,confirm:()=>true,
 fetch:async(url,options)=>{const route=url.slice('/api/'.length),data=JSON.parse(options.body);requests.push({route,data});let result;
 if(route==='profiles')result=[{ID:'one',Name:'Production west'},{ID:'two',Name:'Archive east'}];
 else if(route==='buckets')result=['assets'];
 else if(route==='browse')result={Objects:[],Next:''};
 else if(route==='transfers')result=list.map(t=>({...t}));
 else if(route==='cancel'||route==='retry')result='queued';
 else throw Error('Unexpected endpoint '+route);
 return{ok:true,json:async()=>result};}};
 vm.createContext(context);vm.runInContext(fs.readFileSync(path.join(__dirname,'../frontend/app.js'),'utf8'),context);await new Promise(r=>setImmediate(r));
 return{get,context,requests,setTransfers:async transfers=>{list=transfers;await context.transfers();},row:id=>get('transfers').children.find(row=>row.textContent.includes(id))};
}
function descendants(element){return[element,...element.children.flatMap(descendants)];}
function part(row,name){return descendants(row).find(element=>element.className==='transfer-'+name);}
function transfer(overrides={}){return{ID:'transfer',Direction:'upload',ProfileID:'one',Bucket:'assets',Key:'folder/photo.jpg',Done:512,Total:1024,Status:'running',Error:'',...overrides};}

test('transfer identity, literal filenames, and bounded accessible progress remain clear across polls',async()=>{
 const h=await transferFixture(),key='folder/<img src=x onerror=alert(1)>.txt';
 await h.setTransfers([transfer({Key:key,Done:4096})]);const row=h.get('transfers').children[0],progress=part(row,'progress'),cancel=row.children[1].children[0];
 assert.equal(part(row,'name').textContent,'<img src=x onerror=alert(1)>.txt');
 assert.equal(part(row,'name').children.length,0,'untrusted S3 keys must remain literal text');
 assert.match(part(row,'context').textContent,/Upload.*Production west.*assets\/folder\//);
 assert.equal(progress.tag,'progress');assert.equal(progress.max,1024);assert.equal(progress.value,1024,'reported bytes above total must not overrun progress');assert.equal(progress.attributes['aria-label'],'Upload '+key);
 assert.equal(part(row,'status').textContent,'Awaiting confirmation');assert.match(part(row,'metrics').textContent,/read.*awaiting storage confirmation/);assert.doesNotMatch(part(row,'metrics').textContent,/%/,'source bytes read must not falsely present an unconfirmed upload as complete');assert.match(progress.attributes['aria-valuetext'],/source bytes read.*upload still in progress/);
 await h.setTransfers([transfer({Key:key,Done:-5})]);assert.equal(part(row,'progress').value,0,'negative reported bytes must clamp to zero');assert.equal(row.children[1].children[0],cancel,'same-state polling must preserve the focused action');
 await cancel.click();assert.deepEqual(h.requests.filter(r=>r.route==='cancel').map(r=>r.data),[{ID:'transfer'}]);
 await h.setTransfers([transfer({Key:key,Total:0,Done:8192})]);assert.equal(progress.attributes.value,undefined,'unknown total must become indeterminate after a determinate poll');assert.equal(progress.max,1);assert.doesNotMatch(part(row,'metrics').textContent,/%/);assert.match(part(row,'metrics').textContent,/8 KiB transferred/);
 await h.setTransfers([transfer({Key:key,Direction:'download',ProfileID:'two',Done:1073741824,Total:2147483648})]);assert.match(part(row,'context').textContent,/Download.*Archive east.*assets\//);assert.match(part(row,'metrics').textContent,/1 GiB of 2 GiB.*50%/);
 await h.setTransfers([transfer({Key:key,Direction:'download',Done:1024,Total:1024})]);assert.equal(part(row,'status').textContent,'Awaiting confirmation');assert.match(part(row,'metrics').textContent,/received.*awaiting completion/);assert.match(progress.attributes['aria-valuetext'],/download still in progress/);assert.equal(row.children[1].children[0].textContent,'Cancel','a full byte counter must remain cancellable until server completion');
 await h.setTransfers([transfer({Key:key,Direction:'download',Done:1024,Total:1024,Status:'completed'})]);assert.equal(part(row,'status').textContent,'Completed');assert.equal(progress.hidden,true);assert.equal(row.children[1].children.length,0,'completed state must come from confirmed server status');
});

test('interruption recovery explains restart and preserves cleanup warnings while retrying',async()=>{
 const h=await transferFixture(),failure=transfer({Status:'failed',Error:'Interrupted when Harbor stopped.',CleanupError:'Remote multipart cleanup pending'});
 await h.setTransfers([failure]);const row=h.get('transfers').children[0],notice=row.children[2],retry=row.children[1].children[0];
 assert.equal(retry.textContent,'Retry');assert.match(part(row,'error').textContent,/Interrupted/);assert.match(notice.textContent,/Remote multipart cleanup pending/);assert.match(notice.textContent,/Retry restarts this transfer from the beginning/);assert.equal(notice.hidden,false);assert.equal(notice.attributes.role,'status');assert.match(h.get('transfer-summary').textContent,/1 need attention/);
 await h.setTransfers([failure]);assert.equal(row.children[1].children[0],retry);assert.match(notice.textContent,/cleanup pending/);
 await retry.click();assert.deepEqual(h.requests.filter(r=>r.route==='retry').map(r=>r.data),[{ID:'transfer'}]);
 await h.setTransfers([{...failure,Status:'running',Error:'',Done:0}]);assert.match(notice.textContent,/cleanup pending/,'retry must not hide unresolved cleanup');assert.equal(notice.hidden,false);assert.match(h.get('transfer-summary').textContent,/1 active.*1 need attention/);
 await h.setTransfers([{...failure,Status:'completed',Done:1024,Error:'',CleanupError:'',SavedPath:'/Downloads/photo.jpg',SaveError:'Permission denied'}]);assert.match(notice.textContent,/Save failed: Permission denied/);assert.equal(notice.hidden,false);assert.match(h.get('transfer-summary').textContent,/1 need attention/);
});

test('workspace actions follow connection and bucket availability',async()=>{
 const h=await transferFixture();
 for(const id of ['test','upload','new-folder','refresh','go','path','object-search'])assert.equal(h.get(id).disabled,true,id+' must start disabled');
 await h.get('profiles').children[0].children[0].click();assert.equal(h.get('test').disabled,false);assert.equal(h.get('upload').disabled,true);
 await h.get('buckets').children[0].children[0].click();for(const id of ['upload','new-folder','refresh','go','path','object-search'])assert.equal(h.get(id).disabled,false,id+' must enable in a bucket');assert.equal(h.get('up').disabled,true,'bucket root has no parent');
 await h.get('profiles').children[1].children[0].click();assert.equal(h.get('upload').disabled,true,'connection switch must disable stale bucket actions');assert.equal(h.get('new-folder').disabled,true);assert.match(h.get('workspace-name').textContent,/Archive east/);
});

test('failed transfers and cleanup warnings stay above history without moving controls on progress polls',async()=>{
 const h=await transferFixture(),completed=transfer({ID:'history',Key:'history.jpg',Status:'completed'}),active=transfer({ID:'live',Key:'live.jpg'}),cancelled=transfer({ID:'cancelled',Key:'cancelled.jpg',Status:'cancelled'}),failed=transfer({ID:'failed',Key:'failed.jpg',Status:'failed',Error:'Network interrupted'}),cleanup=transfer({ID:'cleanup',Key:'cleanup.jpg',Status:'completed',CleanupError:'Cleanup pending'});
 await h.setTransfers([completed,cancelled,active,failed,cleanup]);
 const container=h.get('transfers'),names=()=>container.children.map(row=>part(row,'name').textContent);
 assert.deepEqual(names(),['failed.jpg','cleanup.jpg','live.jpg','cancelled.jpg','history.jpg'],'recovery work must not be buried under completed history');assert.match(h.get('transfer-summary').textContent,/1 active.*2 need attention.*1 cancelled.*2 completed/);
 const live=container.children[2],cancel=live.children[1].children[0];let moves=0,restores=0;const append=container.append.bind(container);container.append=element=>{moves++;append(element);};
 cancel.closest=selector=>selector==='.transfer-row'?live:null;cancel.focus=options=>{assert.equal(options.preventScroll,true);restores++;};h.context.document.activeElement=cancel;
 await h.setTransfers([completed,cancelled,{...active,Done:900},failed,cleanup]);assert.equal(moves,0,'progress-only polls must not move DOM or disturb scroll/focus');assert.equal(live.children[1].children[0],cancel);
 await h.setTransfers([{...completed,Status:'failed',Error:'Save failed'},cancelled,active,failed,cleanup]);assert.deepEqual(names(),['history.jpg','failed.jpg','cleanup.jpg','live.jpg','cancelled.jpg']);assert.equal(live.children[1].children[0],cancel,'reprioritization must move existing controls');assert.equal(restores,1,'moving rows must restore existing keyboard focus without scrolling');
});

test('transfer tray reveals new work and recovered interruptions while respecting a user collapse across polls',async()=>{
 const h=await transferFixture(),panel=h.get('transfer-panel');
 await h.setTransfers([]);assert.notEqual(panel.open,true,'empty tray should stay collapsed');
 const history=transfer({ID:'history',Status:'completed'});await h.setTransfers([history]);assert.notEqual(panel.open,true,'completed history alone should not expand the tray');
 const active=transfer({ID:'new-upload'});await h.setTransfers([history,active]);assert.equal(panel.open,true,'new active work should reveal progress');
 panel.open=false;await h.setTransfers([history,{...active,Done:900}]);assert.equal(panel.open,false,'progress polling must preserve a manual collapse');
 await h.setTransfers([history,{...active,Status:'queued'}]);assert.equal(panel.open,false,'ongoing work state changes must preserve a manual collapse');
 const failed={...active,Status:'failed',Error:'Connection lost'};await h.setTransfers([history,failed]);assert.equal(panel.open,true,'new failure should reveal recovery controls');
 panel.open=false;await h.setTransfers([history,failed]);assert.equal(panel.open,false,'an unchanged failure must not reopen the tray each second');
 await h.setTransfers([history,{...failed,CleanupError:'Remote cleanup pending'}]);assert.equal(panel.open,true,'a newly reported cleanup warning should reveal attention');
 panel.open=false;await h.setTransfers([history,{...active,Status:'queued',CleanupError:'Remote cleanup pending'}]);assert.equal(panel.open,false,'retrying with an already shown cleanup warning must preserve a manual collapse');
 await h.setTransfers([history,{...active,Status:'running',CleanupError:'Remote cleanup pending'}]);assert.equal(panel.open,false,'a retry starting must not repeat an unchanged cleanup warning');
 panel.open=false;await h.setTransfers([history,{...active,Status:'cancelled'}]);assert.equal(panel.open,false,'intentional cancellation alone should not override collapse');
 await h.setTransfers([history,{...active,Status:'completed',SaveError:'Unable to save file'}]);assert.equal(panel.open,true,'save failure must reveal attention even after transfer completion');
 const restarted=await transferFixture();await restarted.setTransfers([transfer({Status:'failed',Error:'Interrupted when Harbor stopped.'})]);assert.equal(restarted.get('transfer-panel').open,true,'first poll after restart must reveal recovered failures');
 const cleanup=await transferFixture();await cleanup.setTransfers([transfer({Status:'completed',CleanupError:'Offline cleanup pending'})]);assert.equal(cleanup.get('transfer-panel').open,true,'initial recovered cleanup warnings also need attention');
});
