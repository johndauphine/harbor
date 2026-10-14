const $=id=>document.getElementById(id);let session=location.hash.slice(1)||sessionStorage.getItem('harbor-session');if(session){sessionStorage.setItem('harbor-session',session);history.replaceState(null,'',location.pathname)}let profile='',bucket='',prefix='',next='',objects=[],selected=null,browseRequest=0;async function api(route,data={}){const r=await fetch('/api/'+route,{method:'POST',headers:{'Content-Type':'application/json','Authorization':'Bearer '+session},body:JSON.stringify(data)});let v=await r.json();if(!r.ok)throw Error(v.error||'Request failed');return v}function message(s){$('message').textContent=s}function action(fn){return async()=>{try{await fn()}catch(e){message(e.message)}}}function button(text,fn){const b=document.createElement('button');b.textContent=text;b.onclick=action(async()=>{if(b.parentElement?.closest("aside")){for(const peer of b.parentElement.children){peer.classList.remove("active");peer.setAttribute("aria-pressed","false")}b.classList.add("active");b.setAttribute("aria-pressed","true")}await fn()});return b}async function profiles(){const list=await api('profiles');$('profiles').replaceChildren(...list.map(p=>button(p.Name,async()=>{profile=p.ID;bucket='';prefix='';clearSelection();$('objects').replaceChildren();$('buckets').replaceChildren();message('Loading buckets…');const buckets=await api('buckets',{ProfileID:profile});$('buckets').replaceChildren(...buckets.map(b=>button('▱ '+b,async()=>{bucket=b;prefix='';await browse()})));message(buckets.length+' buckets available')})))}async function browse(append=false,options={}){const request=++browseRequest;const target={ProfileID:profile,Bucket:bucket,Prefix:prefix};const selectedKey=options.preserveSelection?selected?.Key:null;clearSelection();if(!options.quiet)message('Loading…');const page=await api('browse',{...target,Token:append?next:''});if(request!==browseRequest||profile!==target.ProfileID||bucket!==target.Bucket||prefix!==target.Prefix)return false;objects=append?objects.concat(page.Objects):page.Objects;next=page.Next;$('path').value=prefix;$('path').placeholder='Bucket root';$('location').textContent=bucket+'/'+prefix;$('more').hidden=!next;$('objects').replaceChildren(...objects.map(o=>{const row=document.createElement('tr');for(const value of [(o.Folder?'▸ ':'')+o.Key.slice(prefix.length),o.Folder?'—':o.Size.toLocaleString()+' B',o.Modified?new Date(o.Modified).toLocaleString():'—',o.StorageClass||'—']){const td=document.createElement('td');td.textContent=value;row.append(td)}row.tabIndex=0;row.setAttribute('aria-selected','false');row.onclick=action(async()=>{if(o.Folder){prefix=o.Key;await browse();return}clearSelection();selected=o;row.classList.add('selected');row.setAttribute('aria-selected','true');$('download').disabled=o.Folder;$('delete').disabled=o.Folder;$('details').textContent='Selected: '+o.Key+' · '+o.Size+' bytes · ETag '+o.ETag});row.onkeydown=e=>{if(e.key==='Enter'||e.key===' '){e.preventDefault();row.click()}};if(o.Folder)row.title='Open folder';return row}));if(selectedKey){const index=objects.findIndex(o=>!o.Folder&&o.Key===selectedKey);if(index>=0)$('objects').children[index].click()}if(!options.quiet)message(bucket+'/'+prefix+' · '+objects.length+' entries');return true}$('add').onclick=()=>$('connection').showModal();$('close').onclick=()=>$('connection').close();$('form').onsubmit=async e=>{e.preventDefault();try{const f=new FormData(e.target);await api('save',{Profile:{Name:f.get('Name'),Endpoint:f.get('Endpoint'),Region:f.get('Region'),PathStyle:f.has('PathStyle'),CAFile:f.get('CAFile')},Secret:{AccessKey:f.get('AccessKey'),SecretKey:f.get('SecretKey'),SessionToken:f.get('SessionToken')}});e.target.reset();$('connection').close();await profiles();message('Connection saved securely')}catch(e){message(e.message)}};$('refresh').onclick=action(()=>browse());$('go').onclick=action(async()=>{prefix=$('path').value;await browse()});$('up').onclick=action(async()=>{prefix=prefix.replace(/[^/]+\/$/,'');await browse()});$('test').onclick=action(async()=>message(await api('test',{ProfileID:profile})));$('more').onclick=action(()=>browse(true));action(profiles)();

$('upload').onclick=()=>{if(!bucket){message('Choose a bucket first');return}$('file').click()};
$('file').onchange=action(async()=>{const file=$('file').files[0];if(!file)return;const key=prefix+file.name;if(!confirm('Upload to '+bucket+'/'+key+'? An existing object with this key will be replaced.'))return;message('Staging upload…');const r=await fetch('/api/upload?'+new URLSearchParams({profile,bucket,key}),{method:'POST',headers:{Authorization:'Bearer '+session},body:file});if(!r.ok)throw Error(await r.text());message('Upload queued');$('file').value=''});
$('download').onclick=action(async()=>{if(!selected||selected.Folder)throw Error('Select a file first');await api('download',{ProfileID:profile,Bucket:bucket,Key:selected.Key});message('Download queued')});
async function saveDownload(t){const r=await fetch('/api/file',{method:'POST',headers:{Authorization:'Bearer '+session,'Content-Type':'application/json'},body:JSON.stringify({ID:t.ID})});if(!r.ok)throw Error(await r.text());const blob=await r.blob(),url=URL.createObjectURL(blob),link=document.createElement('a');link.href=url;link.download=t.Key.split('/').pop();link.click();setTimeout(()=>URL.revokeObjectURL(url),60000)}

const transferStates=new Map(),transferRows=new Map();let pollingTransfers=false;
async function transfers(){
 if(pollingTransfers)return;
 pollingTransfers=true;
 try{
  const list=await api('transfers');
  const present=new Set(list.map(t=>t.ID));
  for(const [id,entry] of transferRows){if(!present.has(id)){entry.row.remove();transferRows.delete(id)}}
  for(const t of list){
   let entry=transferRows.get(t.ID);
   if(!entry){const row=document.createElement('div'),label=document.createElement('span'),controls=document.createElement('span');row.append(label);row.append(controls);entry={row,label,controls,status:null};transferRows.set(t.ID,entry);$('transfers').append(row)}
   entry.label.textContent=t.Direction+' · '+t.Key+' · '+t.Status+' · '+t.Done.toLocaleString()+' / '+t.Total.toLocaleString()+' bytes '+(t.Error||'');
   if(entry.status!==t.Status){
    entry.controls.replaceChildren();
    if(t.Status==='running'||t.Status==='queued')entry.controls.append(button('Cancel',()=>api('cancel',{ID:t.ID})));
    if(t.Status==='failed'||t.Status==='cancelled')entry.controls.append(button('Retry',()=>api('retry',{ID:t.ID})));
    if(t.Direction==='download'&&t.Status==='completed')entry.controls.append(button('Save file',()=>saveDownload(t)));
    entry.status=t.Status;
   }
  }
  const changed=list.filter(t=>['completed','failed','cancelled'].includes(t.Status)&&transferStates.get(t.ID)!==t.Status);
  for(const t of list)transferStates.set(t.ID,t.Status);
  let refreshError='';
  if(changed.some(t=>t.Status==='completed'&&t.ProfileID===profile&&t.Bucket===bucket&&t.Key.startsWith(prefix))){
   try{await browse(false,{quiet:true,preserveSelection:true})}catch(e){refreshError='Listing refresh failed: '+e.message}
  }
  if(changed.length){
   const summaries=changed.map(t=>{const verb=t.Direction==='upload'?'Upload':'Download';return verb+' '+(t.Status==='completed'?(t.Direction==='download'?'ready to save':'completed'):t.Status)+': '+t.Key+(t.Status==='failed'?' — '+(t.Error||'Unknown error'):'')});
   if(refreshError)summaries.push(refreshError);
   message(summaries.join(' · '));
  }
 }catch(e){message('Unable to update transfer status: '+e.message)}finally{pollingTransfers=false}
}
setInterval(transfers,1000);


function clearSelection(){selected=null;for(const row of $('objects').children){row.classList.remove('selected');row.setAttribute('aria-selected','false')}$('download').disabled=true;$('delete').disabled=true;$('details').textContent='Select an object to inspect its details.'}clearSelection();

$('new-folder').onclick=action(async()=>{if(!bucket)throw Error('Choose a bucket first');const target={ProfileID:profile,Bucket:bucket,Prefix:prefix};const name=prompt('Create a folder in '+bucket+'/'+prefix+'\nFolder name:');if(name===null)return;const created=await api('create-folder',{...target,Key:name});if(profile===target.ProfileID&&bucket===target.Bucket&&prefix===target.Prefix)await browse();message('Created folder: '+created)});

$('delete').onclick=action(async()=>{if(!selected||selected.Folder)throw Error('Select a file first');const target={ProfileID:profile,Bucket:bucket,Key:selected.Key,ETag:selected.ETag};if(!confirm('Delete this file?\n\n'+target.Bucket+'/'+target.Key+'\n\nThis may permanently remove it. Bucket versioning and retention rules apply.'))return;$('delete').disabled=true;try{await api('delete-file',target);if(profile===target.ProfileID&&bucket===target.Bucket)await browse();message('Deleted: '+target.Key)}finally{$('delete').disabled=!selected||selected.Folder}});
