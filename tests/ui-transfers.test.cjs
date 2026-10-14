const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
class Element {
 constructor(){this.children=[];this.classList={add(){},remove(){}};this.value='';}
 append(e){e.parentElement=this;this.children.push(e);}
 replaceChildren(...elements){this.children=[];elements.forEach(e=>this.append(e));}
 remove(){if(this.parentElement)this.parentElement.children=this.parentElement.children.filter(e=>e!==this);}
 showModal(){this.open=true;}
 close(){this.open=false;}
 setAttribute(){}
 closest(){return false;}
 click(){return this.onclick?.();}
}
test('terminal transfers refresh once, preserve selection, and report failures and retries', async()=>{
 const elements=new Map();const get=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id);};
 let transfers=[],browseCalls=0,downloadRequests=[],uploadRequests=[];
 const context={document:{getElementById:get,createElement:()=>new Element()},location:{hash:'#fixture',pathname:'/'},sessionStorage:{getItem(){},setItem(){}},history:{replaceState(){}},setInterval(){},setTimeout,URL,URLSearchParams,confirm:()=>true,
 fetch:async(url,options)=>{
  let result;
  if(url.endsWith('/profiles'))result=[{ID:'profile',Name:'Fixture'}];
  else if(url.endsWith('/buckets'))result=['bucket'];
  else if(url.endsWith('/upload-locations'))result={Path:'/Downloads',Home:'/home',Parent:'/',Folders:[],Files:[{Name:'fixture.txt',Size:3}]};
  else if(url.endsWith('/upload-local')){uploadRequests.push(JSON.parse(options.body));result='upload-id';}
  else if(url.endsWith('/save-locations'))result={Path:'/Downloads',Home:'/home',Parent:'/',Folders:[]};
  else if(url.endsWith('/download')){downloadRequests.push(JSON.parse(options.body));result={ID:'new',Path:'/Downloads/fixture.txt'};}
  else if(url.endsWith('/transfers'))result=transfers.map(t=>({...t}));
  else if(url.endsWith('/browse')){browseCalls++;result={Objects:[{Key:'fixture.txt',Size:3,Folder:false,ETag:'etag'}],Next:''};}
  else throw Error('Unexpected endpoint '+url);
  return {ok:true,json:async()=>result};
 }};
 vm.createContext(context);vm.runInContext(fs.readFileSync(path.join(__dirname,'../frontend/app.js'),'utf8'),context);
 await new Promise(r=>setImmediate(r));
 await get('profiles').children[0].click();await get('buckets').children[0].click();await get('objects').children[0].click();
 await get('download').click();assert.equal(get('save-dialog').open,true);assert.equal(downloadRequests.length,0,'opening Save As must not queue');assert.equal(get('save-name').value,'fixture.txt');
 get('save-cancel').click();assert.equal(downloadRequests.length,0,'cancelling must not queue');
 await get('download').click();await get('save-form').onsubmit({preventDefault(){}});
 assert.equal(downloadRequests.length,1);assert.equal(downloadRequests[0].Path,'/Downloads');assert.equal(downloadRequests[0].Key,'fixture.txt');assert.equal(get('save-dialog').open,false);
 await get('upload').click();assert.equal(get('upload-dialog').open,true);assert.equal(uploadRequests.length,0);assert.equal(get('upload-confirm').disabled,true);
 await get('upload-files').children[0].click();assert.equal(get('upload-confirm').disabled,false);
 await get('upload-form').onsubmit({preventDefault(){}});assert.equal(uploadRequests.length,1);assert.equal(uploadRequests[0].Path,'/Downloads/fixture.txt');assert.equal(uploadRequests[0].Bucket,'bucket');assert.equal(get('upload-dialog').open,false);
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
 await get('profiles').children[0].click();assert.equal(get('location').children.length,0,'clear stale breadcrumbs when switching connections');

});
