const test=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
class Element {
 constructor(){this.children=[];this.dataset={};this.value='';this.classList={add(){},remove(){}}}
 append(...v){this.children.push(...v)} replaceChildren(...v){this.children=v} setAttribute(){} close(){this.open=false} showModal(){this.open=true} reset(){} closest(){return false}
}
async function setup(){
 const elements=new Map(),get=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id)};
 let list=[{ID:'one',Name:'Fixture'}],bucketFail=false;const warning='Connection saved; unused credential cleanup is pending. Unlock the credential vault and restart Harbor to retry cleanup.';
 const context={document:{getElementById:get,createElement:()=>new Element()},location:{hash:'#test',pathname:'/'},sessionStorage:{getItem(){},setItem(){}},history:{replaceState(){}},setInterval(){},setTimeout,clearTimeout,confirm:()=>true,FormData:class {get(k){return k==='Name'?'Fixture':k==='Region'?'us-east-1':''}has(){return false}},fetch:async(url)=>{
 let value;if(url.endsWith('/profiles'))value=list;
 else if(url.endsWith('/save'))value={Value:list[0],Warning:warning};
 else if(url.endsWith('/buckets')){await new Promise(r=>setImmediate(r));if(bucketFail)return{ok:false,json:async()=>({error:'Endpoint unavailable'})};value=['test-bucket']}
 else if(url.endsWith('/delete-profile')){list=[];value={Value:'Connection deleted',Warning:warning.replace('saved','deleted')}}
 else throw Error('Unexpected route '+url);
 return{ok:true,json:async()=>value};
 }};
 vm.createContext(context);vm.runInContext(fs.readFileSync(path.join(__dirname,'../frontend/app.js'),'utf8'),context);await new Promise(r=>setImmediate(r));
 return {context,get,warning,failBuckets(){bucketFail=true}};
}
test('committed save closes dialog and retains cleanup warning after asynchronous bucket loading',async()=>{
 const h=await setup();h.context.editConnection({ID:'one',Name:'Fixture'});
 await h.get('form').onsubmit({preventDefault(){},target:h.get('form')});
 assert.equal(h.get('connection').open,false);assert.equal(h.get('connection-error').textContent,'');assert.equal(h.get('message').textContent,h.warning);assert.equal(h.get('message').className,'status-warning');assert.equal(h.get('buckets').children.length,1);
});
test('bucket failure after committed save does not invite resaving credentials or hide cleanup warning',async()=>{
 const h=await setup();h.failBuckets();h.context.editConnection({ID:'one',Name:'Fixture'});
 await h.get('form').onsubmit({preventDefault(){},target:h.get('form')});
 assert.equal(h.get('connection').open,false);assert.equal(h.get('connection-error').textContent,'');assert.match(h.get('message').textContent,/credential cleanup is pending/);assert.match(h.get('message').textContent,/bucket listing failed: Endpoint unavailable/);
});
test('committed deletion refreshes profiles while showing pending cleanup',async()=>{
 const h=await setup();await h.context.deleteConnection({ID:'one',Name:'Fixture'});
 assert.equal(h.get('profiles').children.length,0);assert.match(h.get('message').textContent,/Connection deleted; unused credential cleanup is pending/);assert.equal(h.get('message').className,'status-warning');
});
