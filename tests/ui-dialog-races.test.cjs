const test=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
class Element{
 constructor(){this.children=[];this.dataset={};this.value='';this.classList={add(){},remove(){}}}
 append(...items){this.children.push(...items)}replaceChildren(...items){this.children=items;this.text=''}
 set textContent(value){this.text=value;this.children=[]}get textContent(){return this.text||''}
 setAttribute(){}showModal(){this.open=true}close(){this.open=false}reset(){this.resets=(this.resets||0)+1}closest(){return false}
 click(){return this.onclick?.()}
}
function deferred(){let resolve,reject;const promise=new Promise((yes,no)=>{resolve=yes;reject=no});return{promise,resolve,reject}}
const tick=()=>new Promise(resolve=>setImmediate(resolve));
const folder=(name,folders=[])=>({Path:name,Home:'/home',Parent:'/',Folders:folders});
async function setup(){
 const elements=new Map(),get=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id)},requests=[];
 const responders={profiles:()=>[{ID:'one',Name:'One',Identity:'identity-one'}],save:req=>req.Profile,buckets:()=>['bucket'],browse:req=>({Objects:[{Key:'current.txt',Size:1,Folder:false}],Next:'',ConnectionIdentity:'identity-'+req.ProfileID}),transfers:()=>[],'save-locations':req=>folder(req.Path||'/home',['child']),download:()=>({ID:'download'})};
 const context={document:{getElementById:get,createElement:()=>new Element()},location:{hash:'#test',pathname:'/'},sessionStorage:{getItem(){},setItem(){}},history:{replaceState(){}},setInterval(){},setTimeout,clearTimeout,confirm:()=>true,
 FormData:class{get(name){return get('connection-'+name).value}has(name){return !!get('connection-'+name).checked}},
 fetch:async(url,options)=>{const route=url.slice('/api/'.length),data=JSON.parse(options.body);requests.push({route,data});assert.ok(responders[route],'Unexpected route '+route);const value=await responders[route](data);return{ok:true,json:async()=>value}}};
 vm.createContext(context);vm.runInContext(fs.readFileSync(path.join(__dirname,'../frontend/app.js'),'utf8'),context);await tick();
 return{context,get,set:(route,fn)=>responders[route]=fn,calls:route=>requests.filter(req=>req.route===route).map(req=>req.data),edit:(name='One')=>context.editConnection({ID:'one',Name:name}),submit:()=>get('form').onsubmit({preventDefault(){},target:get('form')}),openSave:()=>context.saveDownload({ProfileID:'one',Bucket:'bucket',Key:'report.txt',ConnectionIdentity:'identity-one'}),save:()=>get('save-form').onsubmit({preventDefault(){}})};
}

test('save-folder navigation blocks submission and only the latest success or error can update the dialog',async()=>{
 for(const outcome of ['success','failure']){
  const h=await setup();await h.openSave();const slow=deferred();h.set('save-locations',req=>req.Path==='/slow'?slow.promise:folder(req.Path,['new-child']));
  const old=h.context.loadSaveFolder('/slow');assert.equal(h.get('save-confirm').disabled,true);await h.save();assert.equal(h.calls('download').length,0);
  await h.context.loadSaveFolder('/new');assert.equal(h.get('save-path').value,'/new');assert.equal(h.get('save-confirm').disabled,false);
  if(outcome==='success')slow.resolve(folder('/stale',['old-child']));else slow.reject(Error('Old folder error'));await old;
  assert.equal(h.get('save-path').value,'/new');assert.equal(h.get('save-error').textContent,'');assert.equal(h.get('save-folders').children[0].textContent,'▸ new-child');
  await h.save();assert.equal(h.calls('download')[0].Path,'/new');
 }
});

test('a superseded save-folder request cannot release the latest request submission guard',async()=>{
 const h=await setup();await h.openSave();const first=deferred(),last=deferred();h.set('save-locations',req=>req.Path==='/first'?first.promise:last.promise);
 const a=h.context.loadSaveFolder('/first'),b=h.context.loadSaveFolder('/last');first.resolve(folder('/first'));await a;
 assert.equal(h.get('save-confirm').disabled,true);await h.save();assert.equal(h.calls('download').length,0);
 last.resolve(folder('/last'));await b;assert.equal(h.get('save-confirm').disabled,false);await h.save();assert.equal(h.calls('download')[0].Path,'/last');
});

test('cancel and Escape invalidate save-folder results before reopening a new download',async()=>{
 for(const escape of [false,true])for(const failure of [false,true]){
  const h=await setup(),slow=deferred();h.set('save-locations',()=>slow.promise);const old=h.openSave();
  if(escape){h.get('save-dialog').oncancel({preventDefault(){}});h.get('save-dialog').close()}else h.get('save-cancel').click();
  h.set('save-locations',()=>folder('/new'));await h.openSave();h.get('save-name').value='new-name.txt';
  if(failure)slow.reject(Error('Stale failure'));else slow.resolve(folder('/stale'));await old;
  assert.equal(h.get('save-dialog').open,true);assert.equal(h.get('save-path').value,'/new');assert.equal(h.get('save-name').value,'new-name.txt');assert.equal(h.get('save-error').textContent,'');
 }
});

test('explicit folder text supersedes pending navigation and detached folder buttons cannot navigate',async()=>{
 const h=await setup();await h.openSave();const oldFolder=h.get('save-folders').children[0],slow=deferred();h.set('save-locations',()=>slow.promise);
 const old=h.context.loadSaveFolder('/slow');h.get('save-path').value='/typed';h.get('save-path').oninput();
 const count=h.calls('save-locations').length;await oldFolder.click();assert.equal(h.calls('save-locations').length,count);
 slow.resolve(folder('/stale'));await old;assert.equal(h.get('save-path').value,'/typed');assert.equal(h.get('save-confirm').disabled,false);
 await h.save();assert.equal(h.calls('download')[0].Path,'/typed');
});

test('one connection draft submits once while pending and releases the guard after a failed save',async()=>{
 const h=await setup(),slow=deferred();h.edit();h.set('save',()=>slow.promise);const first=h.submit();await h.submit();assert.equal(h.calls('save').length,1);assert.equal(h.get('connection-save').disabled,true);
 slow.reject(Error('Vault locked'));await first;assert.equal(h.get('connection-error').textContent,'Vault locked');assert.equal(h.get('connection-save').disabled,false);
 h.set('save',req=>req.Profile);await h.submit();assert.equal(h.calls('save').length,2);assert.equal(h.get('connection').open,false);
});

test('cancelled connection saves cannot reset, close, enable, or report errors in a newer pending draft',async()=>{
 for(const escape of [false,true])for(const failure of [false,true]){
  const h=await setup(),old=deferred(),current=deferred();h.edit('Old');h.set('save',req=>req.Profile.Name==='Old'?old.promise:current.promise);const a=h.submit();
  if(escape){h.get('connection').oncancel();h.get('connection').close()}else h.get('close').click();
  h.edit('New');const b=h.submit();h.get('connection-error').textContent='Current draft notice';
  if(failure)old.reject(Error('Old save error'));else old.resolve({ID:'one',Name:'Old'});await a;
  assert.equal(h.get('connection').open,true);assert.equal(h.get('connection-Name').value,'New');assert.equal(h.get('form').resets||0,0);assert.equal(h.get('connection-error').textContent,'Current draft notice');assert.equal(h.get('connection-save').disabled,true);assert.equal(h.calls('profiles').length,1);assert.equal(h.calls('buckets').length,0);
  current.reject(Error('Current save error'));await b;assert.equal(h.get('connection-error').textContent,'Current save error');assert.equal(h.get('connection-save').disabled,false);
 }
});

test('stale connection profile and bucket refresh success or failure cannot mutate a newer draft or navigate',async()=>{
 for(const stage of ['profiles','buckets'])for(const failure of [false,true]){
  const h=await setup(),slow=deferred();h.edit('Old');h.set(stage,()=>slow.promise);const old=h.submit();await tick();
  assert.equal(h.get('connection').open,false,'committed save closes before refresh');h.edit('New');h.get('connection-error').textContent='New draft notice';const status=h.get('message').textContent;
  if(failure)slow.reject(Error('Old refresh error'));else slow.resolve(stage==='profiles'?[{ID:'one',Name:'Old',Identity:'old-identity'}]:['old-bucket']);await old;
  assert.equal(h.get('connection').open,true);assert.equal(h.get('connection-Name').value,'New');assert.equal(h.get('connection-error').textContent,'New draft notice');assert.equal(h.get('message').textContent,status);assert.equal(h.get('form').resets,1);assert.equal(h.get('buckets').children.length,0);
  if(stage==='profiles'){assert.equal(h.get('profiles').children[0].children[0].textContent,'One');assert.equal(h.calls('buckets').length,0)}
 }
});

test('committed save with failed profile refresh reports success and credential cleanup warning outside the form',async()=>{
 const h=await setup(),warning='Connection saved; unused credential cleanup is pending.';h.edit();h.set('save',()=>({Value:{ID:'one'},Warning:warning}));h.set('profiles',()=>{throw Error('Refresh unavailable')});await h.submit();
 assert.equal(h.get('connection').open,false);assert.equal(h.get('connection-error').textContent,'');assert.match(h.get('message').textContent,/Connection saved; unused credential cleanup is pending.*Refresh unavailable/);assert.equal(h.get('message').className,'status-warning');
});

test('stale committed credential cleanup warnings remain visible without disturbing the new connection draft',async()=>{
 const h=await setup(),old=deferred();h.edit();h.set('save',()=>old.promise);const pending=h.submit();h.get('close').click();h.edit('New');h.get('connection-error').textContent='New draft notice';old.resolve({Value:{ID:'one'},Warning:'Connection saved; unused credential cleanup is pending.'});await pending;
 assert.equal(h.get('connection').open,true);assert.equal(h.get('connection-Name').value,'New');assert.equal(h.get('connection-error').textContent,'New draft notice');assert.equal(h.get('message').className,'status-warning');
});

test('a new draft during post-save refresh does not lose the committed credential cleanup warning',async()=>{
 for(const stage of ['profiles','buckets'])for(const failure of [false,true]){
  const h=await setup(),refresh=deferred();h.edit();h.set('save',()=>({Value:{ID:'one'},Warning:'Connection saved; unused credential cleanup is pending.'}));h.set(stage,()=>refresh.promise);const save=h.submit();await tick();h.edit('New');h.get('connection-error').textContent='New draft notice';
  if(failure)refresh.reject(Error('Refresh failed'));else refresh.resolve(stage==='profiles'?[{ID:'one',Name:'Old'}]:['bucket']);await save;
  assert.equal(h.get('connection').open,true);assert.equal(h.get('connection-Name').value,'New');assert.equal(h.get('connection-error').textContent,'New draft notice');assert.match(h.get('message').textContent,/credential cleanup is pending/);assert.equal(h.get('message').className,'status-warning');
 }
});

test('manual connection and bucket navigation supersedes a committed save profile refresh',async()=>{
 for(const destination of ['connection','bucket'])for(const failure of [false,true]){
  const h=await setup(),refresh=deferred();await h.context.selectConnection({ID:'one',Name:'One'});h.edit();h.set('profiles',()=>refresh.promise);const save=h.submit();await tick();assert.equal(h.get('connection').open,false);
  if(destination==='connection')await h.context.selectConnection({ID:'two',Name:'Two'});
  await h.get('buckets').children[0].children[0].click();const status=h.get('message').textContent,location=h.get('location').textContent,requests=h.calls('buckets').length;
  if(failure)refresh.reject(Error('Old refresh failed'));else refresh.resolve([{ID:'one',Name:'Saved',Identity:'new-one'},{ID:'two',Name:'Two',Identity:'identity-two'}]);await save;
  assert.equal(h.calls('buckets').length,requests,'save must not select its connection after newer navigation');assert.equal(h.get('location').textContent,location);assert.equal(h.get('message').textContent,status);assert.equal(h.get('objects').children.length,1);assert.equal(h.get('connection-save').disabled,false);
  if(!failure)assert.equal(h.get('profiles').children[0].children[0].textContent,'Saved','sidebar still reflects the committed save');
 }
});

test('manual navigation during a saved connection bucket refresh keeps its listing and status',async()=>{
 for(const failure of [false,true]){
  const h=await setup(),refresh=deferred();h.edit();h.set('buckets',req=>req.ProfileID==='one'?refresh.promise:['bucket']);const save=h.submit();await tick();
  await h.context.selectConnection({ID:'two',Name:'Two'});await h.get('buckets').children[0].children[0].click();const status=h.get('message').textContent,location=h.get('location').textContent;
  if(failure)refresh.reject(Error('Old bucket refresh failed'));else refresh.resolve(['old-bucket']);await save;
  assert.equal(h.get('location').textContent,location);assert.equal(h.get('message').textContent,status);assert.equal(h.get('buckets').children[0].children[0].textContent,'▱ bucket');assert.equal(h.get('objects').children.length,1);
 }
});

test('superseded save navigation still reports credential cleanup without clearing the new location',async()=>{
 const h=await setup(),refresh=deferred();h.edit();h.set('save',()=>({Value:{ID:'one'},Warning:'Connection saved; unused credential cleanup is pending.'}));h.set('profiles',()=>refresh.promise);const save=h.submit();await tick();await h.context.selectConnection({ID:'two',Name:'Two'});await h.get('buckets').children[0].children[0].click();const location=h.get('location').textContent;
 refresh.resolve([{ID:'one',Name:'One'}]);await save;assert.equal(h.get('location').textContent,location);assert.match(h.get('message').textContent,/credential cleanup is pending/);assert.equal(h.get('message').className,'status-warning');
});
