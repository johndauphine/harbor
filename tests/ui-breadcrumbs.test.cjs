const test=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
class Element{
 constructor(tag='div'){this.tag=tag;this.children=[];this.attributes={};this.value='';this.classes=new Set();this.classList={add:v=>this.classes.add(v),remove:v=>this.classes.delete(v)};}
 set textContent(v){this.text=v;this.children=[]}get textContent(){return this.children.length?this.children.map(e=>e.textContent).join(''):this.text||''}
 append(e){e.parentElement=this;this.children.push(e)}replaceChildren(...items){this.text='';this.children=[];items.forEach(e=>this.append(e))}
 showModal(){this.open=true}close(){this.open=false}
 setAttribute(k,v){this.attributes[k]=v}closest(){return false}click(){return this.onclick?.()}
}
async function setup(){
 const elements=new Map(),get=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id)};
 const requests=[];let responder=async req=>({Objects:[{Key:req.Prefix+'child/',Folder:true},{Key:req.Prefix+'file.txt',Size:4,Folder:false}],Next:''});
 const context={document:{getElementById:get,createElement:tag=>new Element(tag)},location:{hash:'#test',pathname:'/'},sessionStorage:{getItem(){},setItem(){}},history:{replaceState(){}},setInterval(){},URL,URLSearchParams,
 fetch:async(url,options)=>{const req=JSON.parse(options.body);let value;if(url.endsWith('/profiles'))value=[{ID:'one',Name:'One'},{ID:'two',Name:'Two'}];else if(url.endsWith('/buckets'))value=['bucket-a','bucket-b'];else if(url.endsWith('/browse')){requests.push(req);value=await responder(req)}else throw Error(url);return{ok:true,json:async()=>value}}};
 vm.createContext(context);vm.runInContext(fs.readFileSync(path.join(__dirname,'../frontend/app.js'),'utf8'),context);await new Promise(r=>setImmediate(r));await get('profiles').children[0].click();await get('buckets').children[0].click();
 return{get,requests,context,setResponder:fn=>responder=fn,crumbs:()=>get('location').children.filter(e=>e.tag==='button'),go:async value=>{get('path').value=value;await get('go').click()}};
}
test('deep paths: drill down, jump to every ancestor and root, clear selection and pagination',async()=>{
 const h=await setup();await h.go('one/two/three/');assert.deepEqual(h.crumbs().map(e=>e.textContent),['bucket-a','one','two','three']);
 assert.equal(h.crumbs()[3].attributes['aria-current'],'location');assert.equal(h.crumbs()[0].attributes['aria-current'],undefined);
 await h.get('objects').children[1].click();assert.equal(h.get('download').disabled,false);
 await h.crumbs()[2].click();assert.equal(h.requests.at(-1).Prefix,'one/two/');assert.equal(h.requests.at(-1).Token,'');assert.equal(h.get('download').disabled,true);
 await h.get('objects').children[0].click();assert.equal(h.get('path').value,'one/two/child/');
 await h.crumbs()[1].click();assert.equal(h.get('path').value,'one/');await h.crumbs()[0].click();assert.equal(h.get('path').value,'');assert.equal(h.crumbs().length,1);
});
test('spaces, Unicode, percent signs, HTML-like names and repeated slashes preserve exact keys',async()=>{
 const h=await setup();for(const prefix of ['a space/日本語/%2F/<img onerror=x>/','/first//last/','a.b/../tail','simple']){
 await h.go(prefix);const parts=prefix.match(/[^/]*\/|[^/]+$/g)||[];let expected='';const captured=h.crumbs();
 for(let i=0;i<parts.length;i++){expected+=parts[i];await captured[i+1].click();assert.equal(h.requests.at(-1).Prefix,expected);assert.equal(captured[i+1].children.length,0,'labels remain plain text')}
 }
});
test('switching buckets or connections removes old path segments',async()=>{
 const h=await setup();await h.go('old/path/');await h.get('buckets').children[1].click();assert.deepEqual(h.crumbs().map(e=>e.textContent),['bucket-b']);assert.equal(h.requests.at(-1).Prefix,'');
 await h.go('new/path/');await h.get('profiles').children[1].click();assert.equal(h.crumbs().length,0);assert.equal(h.get('location').textContent,'Choose a bucket');
});
test('a slow previous listing cannot overwrite newer breadcrumb navigation',async()=>{
 const h=await setup();await h.go('parent/child/');let release;
 h.setResponder(req=>req.Prefix==='slow/'?new Promise(r=>release=r):Promise.resolve({Objects:[],Next:''}));
 const pending=h.go('slow/');await h.crumbs()[0].click();release({Objects:[{Key:'slow/stale',Folder:false}],Next:'old-token'});await pending;
 assert.equal(h.get('path').value,'');assert.deepEqual(h.crumbs().map(e=>e.textContent),['bucket-a']);assert.equal(h.get('objects').children.length,0);assert.equal(h.get('more').hidden,true);
});
test('refresh and pagination retain the current breadcrumb path',async()=>{
 const h=await setup();h.setResponder(async req=>({Objects:[],Next:req.Token?'':'next-page'}));await h.go('parent/child/');await h.get('more').click();assert.equal(h.requests.at(-1).Token,'next-page');assert.equal(h.requests.at(-1).Prefix,'parent/child/');await h.get('refresh').click();assert.equal(h.requests.at(-1).Token,'');assert.deepEqual(h.crumbs().map(e=>e.textContent),['bucket-a','parent','child']);
});

test('prefix search queries storage, preserves breadcrumbs, and resets when navigating',async()=>{
 const h=await setup();await h.go('folder/');h.get('object-search').value='report';await h.get('object-search-form').onsubmit({preventDefault(){}});
 assert.equal(h.requests.at(-1).Prefix,'folder/report');assert.deepEqual(h.crumbs().map(e=>e.textContent),['bucket-a','folder']);
 await h.get('clear-search').click();assert.equal(h.requests.at(-1).Prefix,'folder/');
 h.get('object-search').value='again';await h.get('object-search-form').onsubmit({preventDefault(){}});await h.crumbs()[0].click();assert.equal(h.requests.at(-1).Prefix,'');assert.equal(h.get('object-search').value,'');
});
test('column sorting preserves explicit selection and reports loaded scope',async()=>{
 const h=await setup();h.setResponder(async()=>({Objects:[{Key:'b',Size:1,Folder:false},{Key:'a',Size:20,Folder:false}],Next:'next'}));await h.get('refresh').click();
 assert.equal(h.get('objects').children[0].children[1].textContent,'a');
 const radio=h.get('objects').children[0].children[0].children[0];await radio.onclick({stopPropagation(){}});assert.equal(h.get('selection-count').textContent,'1 file selected');
 h.get('sort-size').click();assert.equal(h.get('objects').children[0].children[1].textContent,'b');assert.match(h.get('details').textContent,/Selected: a/);assert.equal(h.get('objects').children[1].children[0].children[0].checked,true);
 assert.match(h.get('sort-scope').textContent,/loaded entries only/);assert.equal(h.get('object-count').textContent,'(2+)');
 h.get('sort-size').click();assert.equal(h.get('objects').children[0].children[1].textContent,'a');
 await h.get('profiles').children[1].click();assert.equal(h.get('object-count').textContent,'(0)');assert.equal(h.get('selection-count').textContent,'No file selected');
});

test('multiple selection survives sorting and refresh; select-all excludes folders',async()=>{
 const h=await setup();h.setResponder(async()=>({Objects:[{Key:'a',Size:1},{Key:'b',Size:2},{Key:'folder/',Folder:true}],Next:''}));await h.get('refresh').click();
 await h.get('objects').children[0].click();await h.get('objects').children[1].click();assert.equal(h.get('selection-count').textContent,'2 files selected');assert.equal(h.get('select-all').checked,true);
 h.get('sort-size').click();await h.context.browse(false,{quiet:true,preserveSelection:true});assert.equal(h.get('selection-count').textContent,'2 files selected');
 h.get('select-all').click();assert.equal(h.get('selection-count').textContent,'No file selected');h.get('select-all').click();assert.equal(h.get('selection-count').textContent,'2 files selected');assert.equal(h.get('objects').children.filter(r=>r.children[0].children[0]?.checked).length,2);
});
test('batch download queues each file once and retains only failed files for retry',async()=>{
 const h=await setup();h.setResponder(async()=>({Objects:[{Key:'a.txt',Size:1},{Key:'b.txt',Size:2}],Next:''}));await h.get('refresh').click();h.get('select-all').click();
 const original=h.context.fetch,calls=[];let fail=true;
 h.context.fetch=async(url,options)=>{const data=JSON.parse(options.body);let value;if(url.endsWith('/save-locations'))value={Path:'/Downloads',Folders:[]};else if(url.endsWith('/transfers'))value=[];else if(url.endsWith('/download')){calls.push(data);if(data.Key==='b.txt'&&fail)return{ok:false,json:async()=>({error:'Disk full'})};value={Path:'/Downloads/'+data.Name}}else return original(url,options);return{ok:true,json:async()=>value}};
 await h.get('download').click();assert.equal(h.get('save-name-label').hidden,true);await h.get('save-form').onsubmit({preventDefault(){}});assert.equal(calls.length,2);assert.match(h.get('save-error').textContent,/b.txt: Disk full/);assert.equal(h.get('save-dialog').open,true);
 fail=false;await h.get('save-form').onsubmit({preventDefault(){}});assert.equal(calls.length,3);assert.equal(calls[2].Key,'b.txt');assert.equal(h.get('save-dialog').open,false);
});
test('batch delete cancel makes no requests; partial failure remains selected',async()=>{
 const h=await setup();let files=[{Key:'a',Size:1,ETag:'a-tag'},{Key:'b',Size:2,ETag:'b-tag'}];h.setResponder(async()=>({Objects:files,Next:''}));await h.get('refresh').click();h.get('select-all').click();
 let confirmed=false,prompt='';h.context.confirm=text=>{prompt=text;return confirmed};const original=h.context.fetch,calls=[];
 h.context.fetch=async(url,options)=>{if(!url.endsWith('/delete-file'))return original(url,options);const data=JSON.parse(options.body);calls.push(data);if(data.Key==='b')return{ok:false,json:async()=>({error:'Access denied'})};files=files.filter(f=>f.Key!==data.Key);return{ok:true,json:async()=>data.Key}};
 await h.get('delete').click();assert.equal(calls.length,0);assert.match(prompt,/bucket-a\/a/);assert.match(prompt,/bucket-a\/b/);
 confirmed=true;await h.get('delete').click();assert.equal(calls.length,2);assert.equal(calls[0].ETag,'a-tag');assert.match(h.get('message').textContent,/Deleted 1 file.*Failed: b: Access denied/);assert.equal(h.get('selection-count').textContent,'1 file selected');assert.match(h.get('details').textContent,/Selected: b/);
});
