const $=id=>document.getElementById(id);let session=location.hash.slice(1)||sessionStorage.getItem('harbor-session');if(session){sessionStorage.setItem('harbor-session',session);history.replaceState(null,'',location.pathname)}let profile='',bucket='',prefix='',next='',objects=[],selected=null,browseRequest=0,objectFilter='',filterLocation='',sortKey='name',sortAscending=true;async function api(route,data={}){const r=await fetch('/api/'+route,{method:'POST',headers:{'Content-Type':'application/json','Authorization':'Bearer '+session},body:JSON.stringify(data)});let v=await r.json();if(!r.ok)throw Error(v.error||'Request failed');return v}function message(s){$('message').textContent=s;$('message').className=/failed|error|unable|could not|denied/i.test(s)?'status-error':/completed|saved|deleted|created/i.test(s)?'status-success':''}function action(fn){return async()=>{try{await fn()}catch(e){message(e.message)}}}function button(text,fn){const b=document.createElement('button');b.textContent=text;b.onclick=action(async()=>{if(b.parentElement?.closest("aside")){for(const peer of b.parentElement.children){peer.classList.remove("active");peer.setAttribute("aria-pressed","false")}b.classList.add("active");b.setAttribute("aria-pressed","true")}await fn()});return b}async function profiles(){const list=await api('profiles');$('profiles').replaceChildren(...list.map(p=>button(p.Name,async()=>{profile=p.ID;bucket='';prefix='';renderBreadcrumbs();clearSelection();objects=[];next='';renderObjects();$('buckets').replaceChildren();message('Loading buckets…');const buckets=await api('buckets',{ProfileID:profile});$('buckets').replaceChildren(...buckets.map(b=>button('▱ '+b,async()=>{bucket=b;prefix='';await browse()})));message(buckets.length+' buckets available')})))}async function browse(append=false,options={}){const scope=JSON.stringify([profile,bucket,prefix]);if(filterLocation!==scope){objectFilter='';$('object-search').value='';filterLocation=scope}const request=++browseRequest;const target={ProfileID:profile,Bucket:bucket,Prefix:prefix+objectFilter};const selectedKey=options.preserveSelection?selected?.Key:null;clearSelection();if(!options.quiet)message('Loading…');const page=await api('browse',{...target,Token:append?next:''});if(request!==browseRequest||profile!==target.ProfileID||bucket!==target.Bucket||prefix+objectFilter!==target.Prefix)return false;objects=append?objects.concat(page.Objects):page.Objects;next=page.Next;$('path').value=prefix;$('path').placeholder='Bucket root';renderBreadcrumbs();$('more').hidden=!next;renderObjects(selectedKey);if(!options.quiet)message(bucket+'/'+prefix+' · '+objects.length+' entries');return true}$('add').onclick=()=>$('connection').showModal();$('close').onclick=()=>$('connection').close();$('form').onsubmit=async e=>{e.preventDefault();try{const f=new FormData(e.target);await api('save',{Profile:{Name:f.get('Name'),Endpoint:f.get('Endpoint'),Region:f.get('Region'),PathStyle:f.has('PathStyle'),CAFile:f.get('CAFile')},Secret:{AccessKey:f.get('AccessKey'),SecretKey:f.get('SecretKey'),SessionToken:f.get('SessionToken')}});e.target.reset();$('connection').close();await profiles();message('Connection saved securely')}catch(e){message(e.message)}};$('refresh').onclick=action(()=>browse());$('go').onclick=action(async()=>{prefix=$('path').value;await browse()});$('up').onclick=action(async()=>{prefix=prefix.replace(/[^/]+\/$/,'');await browse()});$('test').onclick=action(async()=>message(await api('test',{ProfileID:profile})));$('more').onclick=action(()=>browse(true));action(profiles)();

let uploading=false,uploadDirectory=null,uploadSelection=null,uploadTarget=null;
function localChild(directory,name){const sep=directory.includes('\\')?'\\':'/';return directory.replace(/[\\/]$/,'')+sep+name}
async function loadUploadFolder(path=''){
 const locations=await api('upload-locations',{Path:path});uploadDirectory=locations;uploadSelection=null;$('upload-confirm').disabled=true;$('upload-selection').textContent='Choose a file.';$('upload-path').value=locations.Path;
 const folders=locations.Folders.map(name=>button('▸ '+name,()=>loadUploadFolder(localChild(locations.Path,name))));
 const files=(locations.Files||[]).map(file=>{const row=button(file.Name+' · '+file.Size.toLocaleString()+' bytes',()=>{uploadSelection={Path:localChild(locations.Path,file.Name),Name:file.Name};for(const peer of $('upload-files').children)peer.classList.remove('active');row.classList.add('active');$('upload-selection').textContent='Selected: '+file.Name;$('upload-confirm').disabled=false});return row});
 $('upload-files').replaceChildren(...folders,...files);if(!folders.length&&!files.length)$('upload-files').textContent='This folder is empty.';$('upload-error').textContent='';
}
$('upload').onclick=action(async()=>{if(!bucket)throw Error('Choose a bucket first');uploadTarget={ProfileID:profile,Bucket:bucket,Prefix:prefix};$('upload-destination').textContent='Upload to '+bucket+'/'+prefix;$('upload-dialog').showModal();try{await loadUploadFolder()}catch(e){$('upload-error').textContent=e.message}});
$('upload-home').onclick=()=>loadUploadFolder(uploadDirectory?.Home||'').catch(e=>$('upload-error').textContent=e.message);
$('upload-up').onclick=()=>loadUploadFolder(uploadDirectory?.Parent||'').catch(e=>$('upload-error').textContent=e.message);
$('upload-go').onclick=()=>loadUploadFolder($('upload-path').value).catch(e=>$('upload-error').textContent=e.message);
$('upload-cancel').onclick=()=>{if(!uploading)$('upload-dialog').close()};
$('upload-dialog').oncancel=e=>{if(uploading)e.preventDefault()};
$('upload-form').onsubmit=async e=>{
 e.preventDefault();if(uploading||!uploadSelection||!uploadTarget)return;
 const request={ProfileID:uploadTarget.ProfileID,Bucket:uploadTarget.Bucket,Key:uploadTarget.Prefix+uploadSelection.Name,Path:uploadSelection.Path};
 if(!confirm('Upload to '+request.Bucket+'/'+request.Key+'? An existing object with this key will be replaced.'))return;
 uploading=true;$('upload-confirm').disabled=true;$('upload-cancel').disabled=true;$('upload-error').textContent='Preparing upload…';
 try{await api('upload-local',request);$('upload-dialog').close();message('Upload queued');await transfers()}catch(e){$('upload-error').textContent=e.message}finally{uploading=false;$('upload-confirm').disabled=!uploadSelection;$('upload-cancel').disabled=false}
};
$('download').onclick=action(async()=>{if(!selected||selected.Folder)throw Error('Select a file first');await saveDownload({ProfileID:profile,Bucket:bucket,Key:selected.Key})});
let savingDownload=false,saveTransfer=null,saveDirectory=null;
async function loadSaveFolder(path=''){
 const locations=await api('save-locations',{Path:path});saveDirectory=locations;$('save-path').value=locations.Path;
 $('save-folders').replaceChildren(...locations.Folders.map(name=>button('▸ '+name,async()=>{try{const separator=locations.Path.includes('\\')?'\\':'/';await loadSaveFolder(locations.Path.replace(/[\\/]$/,'')+separator+name)}catch(e){$('save-error').textContent=e.message}})));
 if(!locations.Folders.length)$('save-folders').textContent='No subfolders — you can save here.';
 $('save-error').textContent='';
}
async function saveDownload(t){
 if(savingDownload)return;saveTransfer=t;$('save-name').value=t.Key.split('/').pop();$('save-error').textContent='';$('save-dialog').showModal();
 try{await loadSaveFolder()}catch(e){$('save-error').textContent=e.message}
}
$('save-home').onclick=()=>loadSaveFolder(saveDirectory?.Home||'').catch(e=>$('save-error').textContent=e.message);
$('save-up').onclick=()=>loadSaveFolder(saveDirectory?.Parent||'').catch(e=>$('save-error').textContent=e.message);
$('save-go').onclick=()=>loadSaveFolder($('save-path').value).catch(e=>$('save-error').textContent=e.message);
$('save-cancel').onclick=()=>{if(!savingDownload)$('save-dialog').close()};
$('save-form').onsubmit=async e=>{
 e.preventDefault();if(savingDownload||!saveTransfer)return;savingDownload=true;$('save-confirm').disabled=true;$('save-cancel').disabled=true;
 try{const request={ProfileID:saveTransfer.ProfileID,Bucket:saveTransfer.Bucket,Key:saveTransfer.Key,Path:$('save-path').value,Name:$('save-name').value,Overwrite:false};let result=await api('download',request);
  if(result.NeedsOverwrite){if(!confirm('Replace the existing file?\n'+result.Path))return;result=await api('download',{...request,Overwrite:true})}
  $('save-dialog').close();message('Download queued to '+result.Path);await transfers();
 }catch(e){$('save-error').textContent=e.message;await transfers()}finally{savingDownload=false;$('save-confirm').disabled=false;$('save-cancel').disabled=false}
};


const transferStates=new Map(),transferRows=new Map();let pollingTransfers=false;
async function transfers(){
 if(pollingTransfers)return;
 pollingTransfers=true;
 try{
  const list=await api('transfers');
  if(transferRows.size===0)$('transfers').replaceChildren();
  const present=new Set(list.map(t=>t.ID));
  for(const [id,entry] of transferRows){if(!present.has(id)){entry.row.remove();transferRows.delete(id)}}
  for(const t of list){
   let entry=transferRows.get(t.ID);
   if(!entry){const row=document.createElement('div'),label=document.createElement('span'),controls=document.createElement('span'),notice=document.createElement('div'),timestamp=document.createElement('time');row.className='transfer-row';notice.setAttribute('role','status');timestamp.className='transfer-time';row.append(label);row.append(controls);row.append(notice);row.append(timestamp);entry={row,label,controls,notice,timestamp,status:null};transferRows.set(t.ID,entry);$('transfers').append(row)}
   entry.label.textContent=t.Direction+' · '+t.Key+' · '+t.Status+' · '+t.Done.toLocaleString()+' / '+t.Total.toLocaleString()+' bytes '+(t.Error||'');
   entry.timestamp.textContent=t.CreatedAt&&!t.CreatedAt.startsWith('0001-')?'Queued '+new Date(t.CreatedAt).toLocaleString():'';
   const saved=t.SavedPath?'✓ Saved to '+t.SavedPath+(t.SavedAt&&!t.SavedAt.startsWith('0001-')?' · '+new Date(t.SavedAt).toLocaleString():''):'';
   entry.notice.textContent=[saved,t.SaveError?'Save failed: '+t.SaveError:''].filter(Boolean).join(' — ');
   entry.notice.className='transfer-notice'+(t.SaveError?' transfer-save-error':'');
   entry.notice.hidden=!entry.notice.textContent;
   if(entry.status!==t.Status){
    entry.controls.replaceChildren();
    if(t.Status==='running'||t.Status==='queued')entry.controls.append(button('Cancel',()=>api('cancel',{ID:t.ID})));
    if(t.Status==='failed'||t.Status==='cancelled')entry.controls.append(button('Retry',()=>api('retry',{ID:t.ID})));
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
   const summaries=changed.map(t=>{const verb=t.Direction==='upload'?'Upload':'Download';return verb+' '+(t.Status==='completed'?(t.Direction==='download'?'saved':'completed'):t.Status)+': '+t.Key+(t.Status==='failed'?' — '+(t.Error||'Unknown error'):'')});
   if(refreshError)summaries.push(refreshError);
   message(summaries.join(' · '));
  }
 }catch(e){message('Unable to update transfer status: '+e.message)}finally{pollingTransfers=false}
}
setInterval(transfers,1000);


function clearSelection(){selected=null;$('selection-count').textContent='No file selected';for(const row of $('objects').children){if(row.children[0]?.children[0]?.type==='radio')row.children[0].children[0].checked=false;row.classList.remove('selected');row.setAttribute('aria-selected','false')}$('download').disabled=true;$('delete').disabled=true;$('details').textContent='Select an object to inspect its details.'}clearSelection();

$('new-folder').onclick=action(async()=>{if(!bucket)throw Error('Choose a bucket first');const target={ProfileID:profile,Bucket:bucket,Prefix:prefix};const name=prompt('Create a folder in '+bucket+'/'+prefix+'\nFolder name:');if(name===null)return;const created=await api('create-folder',{...target,Key:name});if(profile===target.ProfileID&&bucket===target.Bucket&&prefix===target.Prefix)await browse();message('Created folder: '+created)});

$('delete').onclick=action(async()=>{if(!selected||selected.Folder)throw Error('Select a file first');const target={ProfileID:profile,Bucket:bucket,Key:selected.Key,ETag:selected.ETag};if(!confirm('Delete this file?\n\n'+target.Bucket+'/'+target.Key+'\n\nThis may permanently remove it. Bucket versioning and retention rules apply.'))return;$('delete').disabled=true;try{await api('delete-file',target);if(profile===target.ProfileID&&bucket===target.Bucket)await browse();message('Deleted: '+target.Key)}finally{$('delete').disabled=!selected||selected.Folder}});

function renderBreadcrumbs(){
 const container=$('location');container.replaceChildren();
 if(!bucket){container.textContent='Choose a bucket';return}
 const crumbs=[{label:bucket,path:''}];let path='';
 for(const segment of prefix.match(/[^/]*\/|[^/]+$/g)||[]){path+=segment;crumbs.push({label:segment.endsWith('/')?segment.slice(0,-1)||'(empty)':segment,path})}
 crumbs.forEach((crumb,index)=>{
  if(index){const separator=document.createElement('span');separator.textContent='›';separator.className='breadcrumb-separator';separator.setAttribute('aria-hidden','true');container.append(separator)}
  const link=button(crumb.label,async()=>{prefix=crumb.path;await browse()});link.className='breadcrumb';link.title=crumb.path?bucket+'/'+crumb.path:'Bucket root: '+bucket;
  if(index===crumbs.length-1){link.setAttribute('aria-current','location');link.classList.add('current')}
  container.append(link);
 });
}

function renderObjects(selectedKey=null){
 const field={name:'Key',size:'Size',modified:'Modified',storage:'StorageClass'}[sortKey];
 objects.sort((a,b)=>{const av=a[field]??'',bv=b[field]??'';const compared=sortKey==='size'?Number(av)-Number(bv):String(av).localeCompare(String(bv));return (compared||a.Key.localeCompare(b.Key))*(sortAscending?1:-1)});
 $('object-count').textContent='('+objects.length+(next?'+':'')+')';
 $('sort-scope').textContent=next?'More objects available · sorting loaded entries only':'Sorting all listed entries';
 $('empty-objects').hidden=objects.length>0;$('empty-objects').textContent=objectFilter?'No names match this prefix. Clear the search to see all objects.':'No objects in this folder. Upload a file or create a folder to get started.';
 for(const key of ['name','size','modified','storage'])$('heading-'+key).setAttribute('aria-sort',key===sortKey?(sortAscending?'ascending':'descending'):'none');
$('objects').replaceChildren(...objects.map(o=>{const row=document.createElement('tr');const cell=document.createElement('td');cell.className='selection-column';if(!o.Folder){const control=document.createElement('input');control.type='radio';control.name='selected-object';control.setAttribute('aria-label','Select '+o.Key);control.onclick=e=>{e.stopPropagation();row.click()};cell.append(control)}row.append(cell);for(const value of [(o.Folder?'▸ ':'')+o.Key.slice(prefix.length),o.Folder?'—':o.Size.toLocaleString()+' B',o.Modified?new Date(o.Modified).toLocaleString():'—',o.StorageClass||'—']){const td=document.createElement('td');td.textContent=value;row.append(td)}row.tabIndex=0;row.setAttribute('aria-selected','false');row.onclick=action(async()=>{if(o.Folder){prefix=o.Key;await browse();return}clearSelection();selected=o;row.children[0].children[0].checked=true;$('selection-count').textContent='1 file selected';row.classList.add('selected');row.setAttribute('aria-selected','true');$('download').disabled=o.Folder;$('delete').disabled=o.Folder;$('details').textContent='Selected: '+o.Key+' · '+o.Size+' bytes · ETag '+o.ETag});row.onkeydown=e=>{if(e.key==='Enter'||e.key===' '){e.preventDefault();row.click()}};if(o.Folder)row.title='Open folder';return row}));if(selectedKey){const index=objects.findIndex(o=>!o.Folder&&o.Key===selectedKey);if(index>=0)$('objects').children[index].click()}
}
for(const key of ['name','size','modified','storage'])$('sort-'+key).onclick=()=>{const selectedKey=selected?.Key;sortAscending=sortKey===key?!sortAscending:true;sortKey=key;renderObjects(selectedKey)};
$('object-search-form').onsubmit=async e=>{e.preventDefault();if(!bucket){message('Choose a bucket first');return}objectFilter=$('object-search').value;try{await browse()}catch(e){message(e.message)}};
$('clear-search').onclick=action(async()=>{objectFilter='';$('object-search').value='';if(bucket)await browse()});
