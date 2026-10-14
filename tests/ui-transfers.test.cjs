const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
class Element {
 constructor(){this.children=[];this.classList={add(){},remove(){}};this.value='';}
 append(e){e.parentElement=this;this.children.push(e);}
 replaceChildren(...elements){this.children=[];elements.forEach(e=>this.append(e));}
 setAttribute(){}
 closest(){return false;}
 click(){return this.onclick?.();}
}
test('terminal transfers refresh once, preserve selection, and report failures and retries', async()=>{
 const elements=new Map();const get=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id);};
 let transfers=[],browseCalls=0;
 const context={document:{getElementById:get,createElement:()=>new Element()},location:{hash:'#fixture',pathname:'/'},sessionStorage:{getItem(){},setItem(){}},history:{replaceState(){}},setInterval(){},setTimeout,URL,URLSearchParams,
 fetch:async(url,options)=>{
  let result;
  if(url.endsWith('/profiles'))result=[{ID:'profile',Name:'Fixture'}];
  else if(url.endsWith('/buckets'))result=['bucket'];
  else if(url.endsWith('/transfers'))result=transfers.map(t=>({...t}));
  else if(url.endsWith('/browse')){browseCalls++;result={Objects:[{Key:'fixture.txt',Size:3,Folder:false,ETag:'etag'}],Next:''};}
  else throw Error('Unexpected endpoint '+url);
  return {ok:true,json:async()=>result};
 }};
 vm.createContext(context);vm.runInContext(fs.readFileSync(path.join(__dirname,'../frontend/app.js'),'utf8'),context);
 await new Promise(r=>setImmediate(r));
 await get('profiles').children[0].click();await get('buckets').children[0].click();await get('objects').children[0].click();
 const transfer={ID:'upload',Direction:'upload',ProfileID:'profile',Bucket:'bucket',Key:'fixture.txt',Done:0,Total:3,Status:'running',Error:''};transfers=[transfer];
 await context.transfers();assert.equal(browseCalls,1);
 transfer.Status='completed';transfer.Done=3;await context.transfers();assert.equal(browseCalls,2);assert.match(get('message').textContent,/Upload completed/);assert.match(get('details').textContent,/Selected: fixture.txt/);
 await context.transfers();assert.equal(browseCalls,2,'completed history must not repeatedly refresh');
 transfers=[{...transfer,ID:'retry',Status:'failed',Error:'Access denied'}];await context.transfers();assert.equal(browseCalls,2);assert.match(get('message').textContent,/Upload failed.*Access denied/);
 transfers[0].Status='running';await context.transfers();transfers[0].Status='completed';await context.transfers();assert.equal(browseCalls,3,'successful retry refreshes');
 transfers=[{...transfer,ID:'download',Direction:'download'}];await context.transfers();assert.equal(browseCalls,4);assert.match(get('message').textContent,/Download ready to save/);
 transfers=[{...transfer,ID:'elsewhere',Bucket:'other-bucket'}];await context.transfers();assert.equal(browseCalls,4,'unrelated bucket must not replace current view');
});
