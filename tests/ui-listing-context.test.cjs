const test=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
const source=fs.readFileSync(path.join(__dirname,'../frontend/app.js'),'utf8');
class Element{
 constructor(tag='div'){this.tag=tag;this.children=[];this.dataset={};this.attributes={};this.value='';this.classes=new Set();this.classList={add:v=>this.classes.add(v),remove:v=>this.classes.delete(v)}}
 set textContent(v){this.text=v;this.children=[]}get textContent(){return this.children.length?this.children.map(e=>e.textContent).join(''):this.text||''}
 append(e){e.parentElement=this;this.children.push(e)}replaceChildren(...items){this.text='';this.children=[];items.forEach(e=>this.append(e))}
 remove(){if(this.parentElement)this.parentElement.children=this.parentElement.children.filter(e=>e!==this)}
 showModal(){this.open=true}close(){this.open=false}setAttribute(k,v){this.attributes[k]=v}closest(){return false}
 // Invoke handlers even for disabled or detached controls to exercise their own guards.
 click(){return this.onclick?.()}
}
function deferred(){let resolve,reject;const promise=new Promise((yes,no)=>{resolve=yes;reject=no});return{promise,resolve,reject}}
const file=(key,size=1)=>({Key:key,Size:size,Folder:false,ETag:'same-content-etag'});
const page=(objects,next='')=>({Objects:objects.map(o=>({...o})),Next:next});
async function setup(){
 const elements=new Map(),get=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id)},requests=[];
 const responders={profiles:()=>[{ID:'one',Name:'One'},{ID:'two',Name:'Two'}],buckets:()=>['bucket-a','bucket-b'],browse:req=>page([file(req.Prefix+'shared.txt'),{Key:req.Prefix+'folder/',Folder:true}],'initial-token'),'save-locations':()=>({Path:'/Downloads',Home:'/home',Parent:'/',Folders:[]}),download:req=>({ID:'download',Path:req.Path+'/'+req.Name}),'delete-file':req=>req.Key,transfers:()=>[]};
 const context={document:{getElementById:get,createElement:tag=>new Element(tag)},location:{hash:'#test',pathname:'/'},sessionStorage:{getItem(){},setItem(){}},history:{replaceState(){}},setInterval(){},setTimeout,clearTimeout,URL,URLSearchParams,confirm:()=>true,prompt:()=>null,
 fetch:async(url,options)=>{const route=url.slice('/api/'.length),data=JSON.parse(options.body);requests.push({route,data});assert.ok(responders[route],'Unexpected endpoint '+route);const value=await responders[route](data);return{ok:true,json:async()=>value}}};
 vm.createContext(context);vm.runInContext(source,context);await new Promise(r=>setImmediate(r));
 const h={context,get,requests,setResponder:(route,fn)=>responders[route]=fn,calls:route=>requests.filter(r=>r.route===route).map(r=>r.data),selectBucket:name=>get('buckets').children.find(row=>row.children[0].textContent==='▱ '+name).children[0].click(),selectConnection:id=>get('profiles').children.find(row=>row.children[0].dataset.profile===id).children[0].click(),row:key=>get('objects').children.find(row=>row.children[1].textContent===key||row.children[1].textContent==='▸ '+key),go:value=>{get('path').value=value;return get('go').click()},selected:()=>Array.from(context.selectedFiles(),o=>o.Key)};
 await h.selectConnection('one');await h.selectBucket('bucket-a');return h;
}
function assertUnavailable(h){assert.equal(h.get('objects').children.length,0);assert.equal(h.get('download').disabled,true);assert.equal(h.get('delete').disabled,true);assert.equal(h.get('select-all').disabled,true);assert.equal(h.get('more').hidden,true);assert.deepEqual(h.selected(),[])}
async function tryOldActions(h,row){await row.click();h.get('select-all').click();await h.get('download').click();await h.get('delete').click();assert.equal(h.calls('save-locations').length,0);assert.equal(h.calls('download').length,0);assert.equal(h.calls('delete-file').length,0);assertUnavailable(h)}
async function queueSelectedDownload(h){await h.get('download').click();assert.equal(h.get('save-dialog').open,true);await h.get('save-form').onsubmit({preventDefault(){}})}

test('slow and failed bucket navigation makes old rows and actions unavailable; recovery uses the destination bucket',async()=>{
 const h=await setup(),oldRow=h.row('shared.txt'),waiting=deferred();await oldRow.click();
 h.setResponder('browse',req=>req.Bucket==='bucket-b'?waiting.promise:page([file('shared.txt')],'a-token'));
 const navigation=h.selectBucket('bucket-b');assertUnavailable(h);assert.equal(h.get('path').value,'');
 await tryOldActions(h,oldRow);const requests=h.calls('browse').length;await h.get('more').click();assert.equal(h.calls('browse').length,requests,'old pagination must not send a token to the new bucket');
 waiting.reject(Error('Bucket access denied'));await navigation;assertUnavailable(h);assert.match(h.get('message').textContent,/Bucket access denied/);
 await tryOldActions(h,oldRow);
 h.setResponder('browse',req=>page([file('shared.txt',req.Bucket==='bucket-b'?2:1)]));await h.get('refresh').click();await h.row('shared.txt').click();
 await queueSelectedDownload(h);assert.equal(h.calls('download').length,1);assert.equal(h.calls('download')[0].Bucket,'bucket-b');assert.equal(h.calls('download')[0].ProfileID,'one');assert.equal(h.calls('download')[0].Key,'shared.txt');
 await h.get('delete').click();assert.equal(h.calls('delete-file').length,1);assert.deepEqual(h.calls('delete-file')[0],{ProfileID:'one',Bucket:'bucket-b',Key:'shared.txt',ETag:'same-content-etag'});
});

test('slow and failed folder navigation cannot select or act on the previous folder; refresh recovers',async()=>{
 const h=await setup(),oldRow=h.row('shared.txt'),folder=h.row('folder/'),waiting=deferred();await oldRow.click();
 h.setResponder('browse',req=>req.Prefix==='folder/'?waiting.promise:page([file('shared.txt')]));
 const navigation=folder.click();assertUnavailable(h);assert.equal(h.get('path').value,'folder/');assert.equal(h.get('location').textContent,'bucket-a›folder');
 await tryOldActions(h,oldRow);await folder.click();assert.equal(h.calls('browse').at(-1).Prefix,'folder/');
 waiting.reject(Error('Folder access denied'));await navigation;assertUnavailable(h);assert.match(h.get('message').textContent,/Folder access denied/);
 await tryOldActions(h,oldRow);h.setResponder('browse',req=>page([file(req.Prefix+'shared.txt',3)]));await h.get('refresh').click();await h.row('shared.txt').click();
 await queueSelectedDownload(h);await h.get('delete').click();assert.equal(h.calls('download')[0].Key,'folder/shared.txt');assert.equal(h.calls('delete-file')[0].Key,'folder/shared.txt');assert.equal(h.calls('delete-file')[0].Bucket,'bucket-a');
});

test('Go clears the previous listing while a typed path probe is slow or fails',async()=>{
 const h=await setup(),oldRow=h.row('shared.txt'),waiting=deferred();await oldRow.click();h.setResponder('browse',()=>waiting.promise);
 const navigation=h.go('typed-folder');assertUnavailable(h);await tryOldActions(h,oldRow);
 waiting.reject(Error('Typed path denied'));await navigation;assertUnavailable(h);assert.match(h.get('message').textContent,/Typed path denied/);
 await tryOldActions(h,oldRow);h.setResponder('browse',req=>page([file(req.Prefix==='typed-folder'?'typed-folder/shared.txt':req.Prefix+'shared.txt')]));await h.go('typed-folder/');assert.equal(h.get('path').value,'typed-folder/');await h.row('shared.txt').click();assert.deepEqual(h.selected(),['typed-folder/shared.txt']);
});

test('late success or failure from a previous bucket leaves the newer location and status intact',async()=>{
 for(const outcome of ['success','failure']){
  const h=await setup(),waiting=deferred();h.setResponder('browse',req=>req.Bucket==='bucket-b'?waiting.promise:page([file('current-a.txt')],'current-a-token'));
  const navigation=h.selectBucket('bucket-b');await h.selectBucket('bucket-a');await h.row('current-a.txt').click();const currentRow=h.row('current-a.txt'),status=h.get('message').textContent;
  if(outcome==='success')waiting.resolve(page([file('stale-b.txt')],'stale-b-token'));else waiting.reject(Error('Old bucket failure'));
  await navigation;assert.equal(h.get('objects').children[0],currentRow);assert.equal(h.get('message').textContent,status);assert.equal(h.get('location').textContent,'bucket-a');assert.deepEqual(h.selected(),['current-a.txt']);
  h.setResponder('browse',req=>page([file('next-a.txt')]));await h.get('more').click();assert.equal(h.calls('browse').at(-1).Token,'current-a-token');assert.equal(h.calls('browse').at(-1).Bucket,'bucket-a');
 }
});

test('late folder results and typed-path probes cannot navigate after a newer request',async()=>{
 for(const typed of ['slow/','slow-file.txt']){
  for(const outcome of ['success','failure']){
   const h=await setup(),waiting=deferred();h.setResponder('browse',req=>req.Prefix===typed?waiting.promise:page([file(req.Prefix+'current.txt')]));
   const navigation=h.go(typed);await h.go('current/');await h.row('current.txt').click();const currentRow=h.row('current.txt'),status=h.get('message').textContent;
   if(outcome==='success')waiting.resolve(page([file(typed)],'stale-token'));else waiting.reject(Error('Old path failure'));
   await navigation;assert.equal(h.get('objects').children[0],currentRow);assert.equal(h.get('message').textContent,status);assert.equal(h.get('path').value,'current/');assert.equal(h.get('location').textContent,'bucket-a›current');assert.deepEqual(h.selected(),['current/current.txt']);assert.equal(h.get('more').hidden,true);
  }
 }
});

test('failed connection transitions disable old actions and selecting the connection again recovers',async()=>{
 const h=await setup(),oldRow=h.row('shared.txt'),waiting=deferred();await oldRow.click();h.setResponder('buckets',req=>req.ProfileID==='two'?waiting.promise:['bucket-a','bucket-b']);
 const navigation=h.selectConnection('two');assertUnavailable(h);assert.equal(h.get('location').textContent,'Choose a bucket');await tryOldActions(h,oldRow);
 waiting.reject(Error('Connection denied'));await navigation;assertUnavailable(h);assert.match(h.get('message').textContent,/Connection denied/);
 h.setResponder('buckets',()=>['bucket-a']);await h.selectConnection('two');await h.selectBucket('bucket-a');await h.row('shared.txt').click();await queueSelectedDownload(h);assert.equal(h.calls('download')[0].ProfileID,'two');
});

test('connection request identity discards earlier responses even when the same profile is selected again',async()=>{
 for(const outcome of ['success','failure']){
  const h=await setup(),waiting=deferred();let first=true;h.setResponder('buckets',req=>{if(req.ProfileID==='one'&&first){first=false;return waiting.promise}return req.ProfileID==='one'?['current-one']:['two-only']});
  const navigation=h.selectConnection('one');await h.selectConnection('two');await h.selectConnection('one');const currentBuckets=h.get('buckets').children[0],status=h.get('message').textContent;
  if(outcome==='success')waiting.resolve(['stale-one']);else waiting.reject(Error('Old connection failure'));
  await navigation;assert.equal(h.get('buckets').children[0],currentBuckets);assert.equal(currentBuckets.children[0].textContent,'▱ current-one');assert.equal(h.get('message').textContent,status);
 }
});

test('quiet refresh and pagination retain scoped rows and preserve selection changes made while loading',async()=>{
 const h=await setup();h.setResponder('browse',()=>page([file('a.txt'),file('b.txt')],'first-token'));await h.get('refresh').click();await h.row('a.txt').click();
 const refresh=deferred(),originalRow=h.row('a.txt');h.setResponder('browse',()=>refresh.promise);const pendingRefresh=h.context.browse(false,{quiet:true,preserveSelection:true});
 assert.equal(h.row('a.txt'),originalRow);assert.equal(h.get('download').disabled,false);assert.equal(h.get('more').disabled,true);await h.row('b.txt').click();refresh.resolve(page([file('a.txt'),file('b.txt'),file('c.txt')],'second-token'));await pendingRefresh;assert.deepEqual(h.selected(),['a.txt','b.txt']);
 const more=deferred(),loadedRow=h.row('a.txt');h.setResponder('browse',()=>more.promise);const pendingMore=h.get('more').click();assert.equal(h.row('a.txt'),loadedRow);assert.equal(h.calls('browse').at(-1).Token,'second-token');assert.equal(h.get('download').disabled,false);await h.row('a.txt').click();more.resolve(page([file('d.txt')]));await pendingMore;
 assert.equal(h.get('objects').children.length,4);assert.deepEqual(h.selected(),['b.txt']);assert.equal(h.get('more').hidden,true);
});

test('a same-location refresh failure retains the existing listing and safely scoped actions',async()=>{
 const h=await setup();await h.row('shared.txt').click();const loadedRow=h.row('shared.txt');h.setResponder('browse',()=>Promise.reject(Error('Refresh denied')));await h.get('refresh').click();
 assert.equal(h.row('shared.txt'),loadedRow);assert.deepEqual(h.selected(),['shared.txt']);assert.equal(h.get('download').disabled,false);assert.equal(h.get('delete').disabled,false);assert.equal(h.get('more').disabled,false);assert.match(h.get('message').textContent,/Refresh denied/);
 await queueSelectedDownload(h);assert.equal(h.calls('download')[0].Bucket,'bucket-a');assert.equal(h.calls('download')[0].Key,'shared.txt');
 h.setResponder('browse',()=>page([file('shared.txt',2)]));await h.get('refresh').click();assert.deepEqual(h.selected(),['shared.txt']);assert.match(h.get('details').textContent,/2 bytes/);
});

test('an open download and an in-flight batch deletion keep their original bucket after navigation',async()=>{
 const h=await setup();h.setResponder('browse',()=>page([file('a.txt'),file('b.txt')]));await h.get('refresh').click();h.get('select-all').click();await h.get('download').click();
 const firstDelete=deferred();let first=true;h.setResponder('delete-file',req=>{if(first){first=false;return firstDelete.promise}return req.Key});const deleting=h.get('delete').click();assert.equal(h.calls('delete-file').length,1);
 await h.selectBucket('bucket-b');firstDelete.resolve('a.txt');await deleting;assert.deepEqual(h.calls('delete-file').map(req=>req.Bucket),['bucket-a','bucket-a']);assert.deepEqual(h.calls('delete-file').map(req=>req.Key),['a.txt','b.txt']);assert.equal(h.get('location').textContent,'bucket-b');assert.deepEqual(h.selected(),[]);
 await h.get('save-form').onsubmit({preventDefault(){}});assert.deepEqual(h.calls('download').map(req=>req.Bucket),['bucket-a','bucket-a']);assert.deepEqual(h.calls('download').map(req=>req.Key),['a.txt','b.txt']);
});
