const test=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
class Element{
 constructor(tag='div'){this.tag=tag;this.children=[];this.attributes={};this.value='';this.classes=new Set();this.classList={add:v=>this.classes.add(v),remove:v=>this.classes.delete(v)};}
 set textContent(v){this.text=v;this.children=[]}get textContent(){return this.children.length?this.children.map(e=>e.textContent).join(''):this.text||''}
 append(e){e.parentElement=this;this.children.push(e)}replaceChildren(...items){this.text='';this.children=[];items.forEach(e=>this.append(e))}
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
