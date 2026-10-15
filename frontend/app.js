const $=id=>document.getElementById(id);let session=location.hash.slice(1)||sessionStorage.getItem('harbor-session');if(session){sessionStorage.setItem('harbor-session',session);history.replaceState(null,'',location.pathname)}let profile='',bucket='',prefix='',next='',objects=[],selected=null,selectedKeys=new Set(),deleting=false,browseRequest=0,connectionRequest=0,listingContext=null,listingState='empty',objectFilter='',filterLocation='',sortKey='name',sortAscending=true;async function api(route,data={}){const r=await fetch('/api/'+route,{method:'POST',headers:{'Content-Type':'application/json','Authorization':'Bearer '+session},body:JSON.stringify(data)});let v=await r.json();if(!r.ok)throw Error(v.error||'Request failed');return v}function message(s){$('message').textContent=s;$('message').className=/credential cleanup (?:is )?pending/i.test(s)?'status-warning':/failed|error|unable|could not|denied/i.test(s)?'status-error':/completed|saved|deleted|created/i.test(s)?'status-success':''}function action(fn){return async()=>{try{await fn()}catch(e){message(e.message)}}}function button(text,fn){const b=document.createElement('button');b.type='button';b.textContent=text;b.onclick=action(async()=>{await fn()});return b}function formatBytes(value){
 const bytes=Math.max(0,Number(value)||0),units=['B','KiB','MiB','GiB','TiB'];
 if(bytes<1024)return bytes.toLocaleString()+' B';
 const unit=Math.min(Math.floor(Math.log(bytes)/Math.log(1024)),units.length-1);
 return (bytes/Math.pow(1024,unit)).toLocaleString(undefined,{maximumFractionDigits:1})+' '+units[unit];
}
function updateWorkspace(){
 const owner=connectionProfiles?.find(p=>p.ID===profile),name=$('workspace-name'),description=$('workspace-description');
 if(name)name.textContent=owner?.Name||'Storage workspace';
 if(description)description.textContent=owner?(owner.Endpoint||'Amazon S3')+' · '+(owner.Region||'Default region'):'Choose a connection to browse your buckets';
 for(const id of ['upload','new-folder','refresh','go','path','object-search','find-objects','clear-search','up'])$(id).disabled=!bucket||(id==='up'&&!prefix);
 $('test').disabled=!profile;
}
async function selectConnection(p){
 const request=++connectionRequest;browseRequest++;profile=p.ID;bucket='';prefix='';objectFilter='';filterLocation='';$('path').value='';$('object-search').value='';closePathSuggestions();renderBreadcrumbs();clearListing();$('buckets').replaceChildren();message('Loading buckets…');
 for(const row of $('profiles').children){const b=row.children[0];b.classList[b.dataset.profile===p.ID?'add':'remove']('active');b.setAttribute('aria-pressed',String(b.dataset.profile===p.ID))}
 let buckets;try{buckets=await api('buckets',{ProfileID:p.ID})}catch(e){if(request!==connectionRequest||profile!==p.ID)return;throw e}
 if(request!==connectionRequest||profile!==p.ID)return;renderBuckets(buckets,p.ID);message(buckets.length+' buckets available');
}
async function profiles(){
 const list=await api('profiles');connectionProfiles=list;updateWorkspace();
 if(!profile){clearListing();message(list.length?'Choose a connection to browse your storage.':'Add your first connection to start browsing S3 storage.');}
 $('profiles').replaceChildren(...list.map(p=>{
 const row=document.createElement('div');row.className='connection-row';
 const select=document.createElement('button');select.textContent=p.Name;select.title=p.Name;select.setAttribute('aria-pressed',String(profile===p.ID));select.dataset.profile=p.ID;select.className='connection-select';select.onclick=action(()=>selectConnection(p));if(profile===p.ID)select.classList.add('active');
 const menu=document.createElement('details');menu.className='connection-menu';const toggle=document.createElement('summary');toggle.textContent='⋯';toggle.setAttribute('aria-label','Actions for '+p.Name);toggle.title='Actions for '+p.Name;
 const choices=document.createElement('div');choices.className='connection-choices';
 choices.append(button('Edit',()=>{menu.open=false;editConnection(p)}));choices.append(button('Delete',async()=>{menu.open=false;await deleteConnection(p)}));menu.append(toggle);menu.append(choices);row.append(select);row.append(menu);return row;
 }));
}
async function deleteConnection(p){
 if(!confirm('Delete connection “'+p.Name+'”?\nThis removes its saved settings and credentials from this computer. Buckets and files will not be deleted.'))return;
 const result=await api('delete-profile',{ProfileID:p.ID});
 if(profile===p.ID){profile='';bucket='';prefix='';objectFilter='';filterLocation='';connectionRequest++;browseRequest++;closePathSuggestions();$('path').value='';$('object-search').value='';$('buckets').replaceChildren();renderBreadcrumbs();clearListing()}
 await profiles();message(result?.Warning||'Deleted connection: '+p.Name);
}
// Keep each visible listing attached to the location that produced it.
function currentListing(context=listingContext){return !!context&&context.Connection===connectionRequest&&profile===context.ProfileID&&bucket===context.Bucket&&prefix===context.Prefix&&objectFilter===context.Filter}
function clearListing(state='empty'){updateWorkspace();listingContext=null;listingState=state;objects=[];next='';selectedKeys.clear();$('more').hidden=true;$('more').disabled=false;renderObjects([])}
async function browse(append=false,options={}){
 closePathSuggestions();const scope=JSON.stringify([profile,bucket,prefix]);if(filterLocation!==scope){objectFilter='';$('object-search').value='';filterLocation=scope}
 const previousContext=currentListing()?listingContext:null;
 if(append&&(!previousContext||!next||listingState==='loading'))return false;
 const previousObjects=append?objects.slice():[],token=append?next:'';
 const request=++browseRequest,target=Object.freeze({ProfileID:profile,Bucket:bucket,Prefix:prefix,Filter:objectFilter,Connection:connectionRequest});
 if(!previousContext)clearListing('loading');else listingState='loading';
 $('path').value=target.Prefix;$('path').placeholder='Bucket root';updateWorkspace();renderBreadcrumbs();$('more').disabled=true;
 if(!target.ProfileID||!target.Bucket){clearListing();if(!options.quiet)message('Choose a bucket first');return false}
 if(!options.quiet)message('Loading…');
 let page;try{page=await api('browse',{ProfileID:target.ProfileID,Bucket:target.Bucket,Prefix:target.Prefix+target.Filter,Token:token})}
 catch(e){if(request!==browseRequest||!currentListing(target))return false;if(previousContext===listingContext&&currentListing(previousContext)){listingState='ready';$('more').disabled=false}else clearListing('error');throw e}
 if(request!==browseRequest||!currentListing(target))return false;
 const selectedKey=previousContext===listingContext?[...selectedKeys]:[];
 objects=append?previousObjects.concat(page.Objects):page.Objects;next=page.Next;listingContext=target;listingState='ready';$('more').disabled=false;$('more').hidden=!next;renderObjects(selectedKey);
 if(!options.quiet)message(target.Bucket+'/'+target.Prefix+' · '+objects.length+' entries');return true;
}$('add').onclick=()=>editConnection();$('close').onclick=()=>$('connection').close();$('form').onsubmit=async e=>{
 e.preventDefault();$('connection-error').textContent='';
 try{
 const f=new FormData(e.target);
 const result=await api('save',{Profile:{ID:editingConnection,Name:f.get('Name'),Endpoint:f.get('Endpoint'),Region:f.get('Region'),PathStyle:f.has('PathStyle'),CAFile:f.get('CAFile')},Secret:{AccessKey:credentialValue('AccessKey'),SecretKey:credentialValue('SecretKey'),SessionToken:credentialValue('SessionToken')}});
 const saved=result?.Value||result;
 e.target.reset();$('connection').close();await profiles();
 const updated=connectionProfiles.find(p=>p.ID===saved.ID);
 let navigationError='';if(updated){try{await selectConnection(updated)}catch(e){navigationError='; bucket listing failed: '+e.message}}
 message((result?.Warning||'Connection saved securely')+navigationError);
 }catch(e){$('connection-error').textContent=e.message}
};$('refresh').onclick=action(()=>browse());$('go').onclick=action(openTypedPath);$('up').onclick=action(async()=>{prefix=prefix.replace(/[^/]+\/$/,'');await browse()});$('test').onclick=action(async()=>{if(!profile)throw Error('Choose a connection first');message(await api('test',{ProfileID:profile}))});$('more').onclick=action(()=>browse(true));action(profiles)();

let uploading=false,uploadTarget=null,uploadItems=[],uploadStop=false,uploadCollecting=false,uploadGeneration=0;
function localChild(directory,name){const sep=directory.includes('\\')?'\\':'/';return directory.replace(/[\\/]$/,'')+sep+name}
function renderUpload(){
 const busy=uploading||uploadCollecting;
 $('upload-dropzone').setAttribute('aria-disabled',String(busy));
 $('upload-confirm').disabled=busy||!uploadItems.length;
 $('upload-clear').disabled=busy||!uploadItems.length;
 $('upload-add-files').disabled=busy;$('upload-add-folder').disabled=busy;
 $('upload-cancel').textContent=uploading?(uploadStop?'Stopping after this file…':'Stop after this file'):'Cancel';
 $('upload-empty').hidden=uploadItems.length>0;$('upload-table').hidden=!uploadItems.length;
 $('upload-selection').textContent=uploadItems.length+' '+(uploadItems.length===1?'file':'files')+' · '+formatBytes(uploadItems.reduce((sum,item)=>sum+item.file.size,0));
 $('upload-rows').replaceChildren(...uploadItems.map(item=>{
  const row=document.createElement('tr');
  for(const value of [item.path,item.file.name.includes('.')?item.file.name.split('.').pop().toUpperCase():'File',formatBytes(item.file.size),item.status||'Ready']){const cell=document.createElement('td');cell.textContent=value;cell.title=value;row.append(cell)}
  const cell=document.createElement('td'),remove=button('Remove',()=>{if(busy)return;const index=uploadItems.indexOf(item);uploadItems=uploadItems.filter(peer=>peer!==item);renderUpload();const nextRow=$('upload-rows').children[Math.min(index,uploadItems.length-1)];(nextRow?.children[4].children[0]||$('upload-add-files')).focus?.()});remove.disabled=busy;remove.setAttribute('aria-label','Remove '+item.path);cell.append(remove);row.append(cell);return row;
 }));
}
function addUploadFiles(files){
 if(uploading||uploadCollecting)return;
 const errors=[];
 for(const entry of files){
  const file=entry.file||entry,path=entry.path||file.webkitRelativePath||file.name;
  if(!path||path.startsWith('/')||path.split('/').some(part=>!part||part==='.'||part==='..')||path.includes('\0')){errors.push('Invalid file path: '+path);continue}
  if(file.size>5*1024**3){errors.push(path+' exceeds the 5 GiB file limit.');continue}
  if(uploadItems.some(item=>item.path===path)){errors.push(path+' is already in the list. Remove it before choosing a replacement.');continue}
  if(uploadItems.length>=1000){errors.push('Select up to 1,000 files per upload.');break}
  uploadItems.push({file,path,status:'Ready'});
 }
 $('upload-error').textContent=errors.join('\n');renderUpload();
}
$('upload').onclick=action(async()=>{
 if(!bucket)throw Error('Choose a bucket first');uploadGeneration++;uploadItems=[];uploadStop=false;uploadCollecting=false;
 uploadTarget={ProfileID:profile,Bucket:bucket,Prefix:prefix};
 $('upload-destination').textContent='s3://'+bucket+'/'+prefix;
 $('upload-connection').textContent=connectionProfiles.find(p=>p.ID===profile)?.Name||profile;
 $('upload-error').textContent='';$('upload-file-input').value='';$('upload-folder-input').value='';renderUpload();$('upload-dialog').showModal();
});
$('upload-add-files').onclick=()=>{if(!uploading&&!uploadCollecting)$('upload-file-input').click()};
$('upload-add-folder').onclick=()=>{if(!uploading&&!uploadCollecting)$('upload-folder-input').click()};
for(const id of ['upload-file-input','upload-folder-input'])$(id).onchange=()=>{addUploadFiles(Array.from($(id).files||[]));$(id).value=''};
$('upload-clear').onclick=()=>{if(!uploading&&!uploadCollecting){uploadItems=[];$('upload-error').textContent='';renderUpload()}};
function closeUpload(){uploadGeneration++;uploadCollecting=false;$('upload-dialog').close();uploadItems=[];uploadTarget=null}
$('upload-cancel').onclick=()=>{if(uploading){uploadStop=true;renderUpload()}else closeUpload()};
$('upload-dialog').oncancel=e=>{if(uploading)e.preventDefault();else{uploadGeneration++;uploadCollecting=false;uploadItems=[];uploadTarget=null}};
const uploadDrop=$('upload-dropzone');
uploadDrop.onclick=()=>$('upload-add-files').onclick();
uploadDrop.onkeydown=e=>{if(e.target===uploadDrop&&(e.key==='Enter'||e.key===' ')){e.preventDefault();$('upload-add-files').onclick()}};
uploadDrop.ondragover=e=>{e.preventDefault();if(!uploading&&!uploadCollecting)uploadDrop.classList.add('drag-over')};
uploadDrop.ondragleave=()=>uploadDrop.classList.remove('drag-over');
async function droppedUploadFiles(data){
 const result=[];
 async function visit(entry,parent=''){
  const path=parent+entry.name;
  if(entry.isFile){const file=await new Promise((resolve,reject)=>entry.file(resolve,reject));result.push({file,path});if(result.length>1000)throw Error('Select up to 1,000 files per upload.');return}
  if(entry.isDirectory){const reader=entry.createReader();for(;;){const batch=await new Promise((resolve,reject)=>reader.readEntries(resolve,reject));if(!batch.length)break;for(const child of batch)await visit(child,path+'/')}}
 }
 const entries=Array.from(data.items||[]).filter(item=>item.kind==='file').map(item=>item.webkitGetAsEntry?.()||item.getAsEntry?.());
 if(entries.length&&entries.every(Boolean)){for(const entry of entries)await visit(entry);return result}
 return Array.from(data.files||[]);
}
uploadDrop.ondrop=async e=>{
 e.preventDefault();uploadDrop.classList.remove('drag-over');if(uploading||uploadCollecting)return;
 const generation=uploadGeneration;uploadCollecting=true;renderUpload();$('upload-error').textContent='Reading dropped files…';
 try{const files=await droppedUploadFiles(e.dataTransfer);if(generation!==uploadGeneration)return;uploadCollecting=false;addUploadFiles(files);if(!files.length)$('upload-error').textContent='No files found. Use Add files or Add folder to choose files. Empty folders are not uploaded.'}
 catch(error){if(generation===uploadGeneration)$('upload-error').textContent='Could not add dropped files: '+error.message}
 finally{if(generation===uploadGeneration){uploadCollecting=false;renderUpload()}}
};
$('upload-form').onsubmit=async e=>{
 e.preventDefault();if(uploading||uploadCollecting||!uploadItems.length||!uploadTarget)return;
 uploading=true;uploadStop=false;let queued=0;const errors=[];
 const target={...uploadTarget},items=uploadItems.slice();$('upload-error').textContent='';renderUpload();
 for(const item of items){
  if(uploadStop)break;
  item.status='Preparing…';renderUpload();
  try{
   const query=new URLSearchParams({profile:target.ProfileID,bucket:target.Bucket,key:target.Prefix+item.path});
   const response=await fetch('/api/upload?'+query,{method:'POST',headers:{Authorization:'Bearer '+session,'Content-Type':'application/octet-stream'},body:item.file});
   if(!response.ok)throw Error((await response.text()).trim()||'Upload could not be prepared');
   await response.json();item.status='Queued';queued++;
  }catch(error){item.status='Failed';errors.push(item.path+': '+error.message)}
  renderUpload();
 }
 uploading=false;uploadItems=uploadItems.filter(item=>item.status!=='Queued');renderUpload();
 if(!uploadItems.length)closeUpload();
 else $('upload-error').textContent=(uploadStop?'Stopped adding uploads. Already queued transfers continue in Transfers.\n':'')+errors.join('\n');
 // Finalize this draft before awaiting status refresh: the user can now open a
 // different draft, which this completed operation must never close or modify.
 const finishedGeneration=uploadGeneration;
 if(queued){message(queued+' '+(queued===1?'upload':'uploads')+' queued');try{await transfers()}catch(error){if(uploadGeneration===finishedGeneration)message('Uploads were queued, but transfer status could not be refreshed: '+error.message)}}
};
$('download').onclick=action(async()=>{const files=selectedFiles(),target=listingContext;if(!files.length)throw Error('Select files first');await saveDownload(files.map(o=>({ProfileID:target.ProfileID,Bucket:target.Bucket,Key:o.Key}))) });
let savingDownload=false,saveTransfer=null,saveDirectory=null;
async function loadSaveFolder(path=''){
 const locations=await api('save-locations',{Path:path});saveDirectory=locations;$('save-path').value=locations.Path;
 $('save-folders').replaceChildren(...locations.Folders.map(name=>button('▸ '+name,async()=>{try{const separator=locations.Path.includes('\\')?'\\':'/';await loadSaveFolder(locations.Path.replace(/[\\/]$/,'')+separator+name)}catch(e){$('save-error').textContent=e.message}})));
 if(!locations.Folders.length)$('save-folders').textContent='No subfolders — you can save here.';
 $('save-error').textContent='';
}
async function saveDownload(t){
 if(savingDownload)return;saveTransfer=Array.isArray(t)?t:[t];$('save-name').value=saveTransfer[0].Key.split('/').pop();$('save-name-label').hidden=saveTransfer.length>1;$('save-name').required=saveTransfer.length===1;$('save-summary').textContent=saveTransfer.length>1?'Save '+saveTransfer.length+' files to the chosen folder, keeping their filenames: '+saveTransfer.map(t=>t.Key).join(', '):'';$('save-error').textContent='';$('save-dialog').showModal();
 try{await loadSaveFolder()}catch(e){$('save-error').textContent=e.message}
}
$('save-home').onclick=()=>loadSaveFolder(saveDirectory?.Home||'').catch(e=>$('save-error').textContent=e.message);
$('save-up').onclick=()=>loadSaveFolder(saveDirectory?.Parent||'').catch(e=>$('save-error').textContent=e.message);
$('save-go').onclick=()=>loadSaveFolder($('save-path').value).catch(e=>$('save-error').textContent=e.message);
$('save-cancel').onclick=()=>{if(!savingDownload)$('save-dialog').close()};
$('save-dialog').oncancel=e=>{if(savingDownload)e.preventDefault()};
$('save-form').onsubmit=async e=>{
 e.preventDefault();if(savingDownload||!saveTransfer?.length)return;
 const batch=saveTransfer.length>1,directory=$('save-path').value;
 const names=saveTransfer.map(t=>batch?t.Key.split('/').pop():$('save-name').value);
 if(new Set(names).size!==names.length){$('save-error').textContent='Some selected files have the same filename. Download those separately to choose different names.';return}
 savingDownload=true;$('save-confirm').disabled=true;$('save-cancel').disabled=true;
 const remaining=[],errors=[];let queued=0;
 try{for(let i=0;i<saveTransfer.length;i++){
  const t=saveTransfer[i],request={ProfileID:t.ProfileID,Bucket:t.Bucket,Key:t.Key,Path:directory,Name:names[i],Overwrite:false};
  try{let result=await api('download',request);if(result.NeedsOverwrite){if(!confirm('Replace the existing file?\n'+result.Path)){remaining.push(t);continue}await api('download',{...request,Overwrite:true})}queued++}
  catch(e){remaining.push(t);errors.push(t.Key+': '+e.message)}
 }
 saveTransfer=remaining;
 if(!remaining.length)$('save-dialog').close();
 else{$('save-error').textContent=errors.length?errors.join(' · '):'Existing files were skipped. Cancel or choose another destination.';$('save-name-label').hidden=remaining.length>1;$('save-name').required=remaining.length===1;if(remaining.length===1)$('save-name').value=remaining[0].Key.split('/').pop();$('save-summary').textContent=remaining.length+' files remaining: '+remaining.map(t=>t.Key).join(', ')}
 message(queued+' download'+(queued===1?'':'s')+' queued'+(errors.length?' · Failed to queue: '+errors.join(' · '):''));await transfers();
 }finally{savingDownload=false;$('save-confirm').disabled=false;$('save-cancel').disabled=false}
};


const transferStates=new Map(),transferRows=new Map(),transferActivity=new Map();let pollingTransfers=false,transferOrder='';
async function transfers(){
 if(pollingTransfers)return;
 pollingTransfers=true;
 try{
  const list=await api('transfers');
  if(transferRows.size===0)$('transfers').replaceChildren();
  const present=new Set(list.map(t=>t.ID));
  for(const [id,entry] of transferRows){if(!present.has(id)){entry.row.remove();transferRows.delete(id)}}
  let revealTransfers=false;
  for(const t of list){
   const previousActivity=transferActivity.get(t.ID),active=['running','queued'].includes(t.Status);
   const newAttention=(t.Status==='failed'&&(previousActivity?.Status!=='failed'||previousActivity?.Error!==t.Error))||(t.SaveError&&previousActivity?.SaveError!==t.SaveError)||(t.CleanupError&&previousActivity?.CleanupError!==t.CleanupError);
   if((!previousActivity&&active)||newAttention)revealTransfers=true;
   transferActivity.set(t.ID,{Status:t.Status,Error:t.Error,SaveError:t.SaveError,CleanupError:t.CleanupError});
   let entry=transferRows.get(t.ID);
   if(!entry){
    const row=document.createElement('div'),label=document.createElement('div'),controls=document.createElement('div'),notice=document.createElement('div'),timestamp=document.createElement('time');
    row.className='transfer-row';label.className='transfer-label';controls.className='transfer-controls';notice.setAttribute('role','status');timestamp.className='transfer-time';
    const parts={};for(const [key,tag] of [['name','strong'],['status','span'],['context','span'],['progress','progress'],['metrics','span'],['error','span']]){parts[key]=document.createElement(tag);parts[key].className='transfer-'+key;label.append(parts[key])}
    row.append(label);row.append(controls);row.append(notice);row.append(timestamp);entry={row,label,controls,notice,timestamp,...parts,statusValue:null};transferRows.set(t.ID,entry);$('transfers').append(row);
   }
   const direction=t.Direction==='upload'?'Upload':'Download',owner=connectionProfiles.find(p=>p.ID===t.ProfileID),total=Math.max(0,Number(t.Total)||0),done=Math.max(0,Math.min(Number(t.Done)||0,total||Number(t.Done)||0));
   entry.row.dataset.status=t.Status;
   entry.name.textContent=t.Key.split('/').pop()||t.Key;entry.name.title=t.Key;
   const awaiting=t.Status==='running'&&total>0&&done>=total;
   entry.status.textContent=awaiting?'Awaiting confirmation':({running:'In progress',queued:'Queued',completed:'Completed',failed:'Failed',cancelled:'Cancelled'})[t.Status]||t.Status;
   entry.context.textContent=direction+' · '+(owner?owner.Name+' · ':'')+t.Bucket+'/'+t.Key;
   entry.progress.max=total||1;entry.progress.setAttribute('aria-label',direction+' '+t.Key);
   // Unknown totals stay indeterminate; retries can reset byte counts.
   if(total||t.Status==='completed')entry.progress.value=total?done:1;
   else if(entry.progress.removeAttribute)entry.progress.removeAttribute('value');
   entry.progress.hidden=t.Status==='completed';
   entry.metrics.textContent=awaiting?(t.Direction==='upload'?formatBytes(done)+' read · awaiting storage confirmation':formatBytes(done)+' received · awaiting completion'):total?formatBytes(done)+' of '+formatBytes(total)+' · '+Math.floor(done/total*100)+'%':formatBytes(done)+' transferred';
   entry.progress.setAttribute('aria-valuetext',awaiting?(t.Direction==='upload'?formatBytes(done)+' source bytes read; upload still in progress':formatBytes(done)+' received; download still in progress'):entry.metrics.textContent);
   entry.error.textContent=t.Error||'';entry.error.hidden=!t.Error;
   entry.timestamp.textContent=t.CreatedAt&&!t.CreatedAt.startsWith('0001-')?'Queued '+new Date(t.CreatedAt).toLocaleString():'';
   const saved=t.SavedPath?'Saved to '+t.SavedPath+(t.SavedAt&&!t.SavedAt.startsWith('0001-')?' · '+new Date(t.SavedAt).toLocaleString():''):'';
   const noticeText=[saved,t.SaveError?'Save failed: '+t.SaveError:'',t.CleanupError||'',['failed','cancelled'].includes(t.Status)?'Retry restarts this transfer from the beginning.':''].filter(Boolean).join(' — ');
   if(entry.notice.textContent!==noticeText)entry.notice.textContent=noticeText;
   entry.notice.className='transfer-notice'+(t.SaveError||t.CleanupError?' transfer-save-error':'');
   entry.notice.hidden=!entry.notice.textContent;
   if(entry.statusValue!==t.Status){
    entry.controls.replaceChildren();
    if(t.Status==='running'||t.Status==='queued'){const cancel=button('Cancel',()=>api('cancel',{ID:t.ID}));cancel.setAttribute('aria-label','Cancel '+direction.toLowerCase()+' of '+t.Key);entry.controls.append(cancel)}
    if(t.Status==='failed'||t.Status==='cancelled'){const retry=button('Retry',()=>api('retry',{ID:t.ID}));retry.setAttribute('aria-label','Retry '+direction.toLowerCase()+' of '+t.Key);entry.controls.append(retry)}
    entry.statusValue=t.Status;
   }
  }
  if(revealTransfers&&$('transfer-panel'))$('transfer-panel').open=true;
  // Keep recovery and live work above history, moving existing nodes only when order changes.
  const needsAttention=t=>t.Status==='failed'||t.SaveError||t.CleanupError;
  const rank=t=>needsAttention(t)?0:['running','queued'].includes(t.Status)?1:t.Status==='cancelled'?2:3;
  const ordered=list.slice().sort((a,b)=>rank(a)-rank(b)),order=JSON.stringify(ordered.map(t=>t.ID));
  if(order!==transferOrder){
   const focused=document.activeElement?.closest?.('.transfer-row')?document.activeElement:null;
   for(const t of ordered)$('transfers').append(transferRows.get(t.ID).row);
   focused?.focus?.({preventScroll:true});transferOrder=order;
  }
  const summary=$('transfer-summary'),active=list.filter(t=>['running','queued'].includes(t.Status)).length,attention=list.filter(needsAttention).length,cancelled=list.filter(t=>t.Status==='cancelled').length;
  if(summary)summary.textContent=list.length?[active+' active',attention+' need attention',...(cancelled?[cancelled+' cancelled']:[]),list.filter(t=>t.Status==='completed').length+' completed'].join(' · '):'No transfers yet';
  if(!list.length)$('transfers').textContent='Upload or download a file to see progress here.';
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


function selectedFiles(){return currentListing()?objects.filter(o=>!o.Folder&&selectedKeys.has(o.Key)):[]}
let selectionInspector=null,selectionSummary=null,selectionMetadata=null;
function updateSelection(){
 const files=selectedFiles();selected=files.length===1?files[0]:null;
 $('selection-count').textContent=files.length?files.length+' file'+(files.length===1?'':'s')+' selected':'No file selected';
 $('download').disabled=!files.length;$('delete').disabled=!files.length||deleting;
 if(selected){
  if(!selectionInspector){selectionInspector=document.createElement('details');selectionInspector.className='selection-inspector';selectionSummary=document.createElement('summary');selectionSummary.className='selection-summary';selectionMetadata=document.createElement('dl');selectionMetadata.className='selection-metadata';selectionInspector.append(selectionSummary);selectionInspector.append(selectionMetadata)}
  selectionSummary.textContent='Selected: '+selected.Key+' · '+(selected.Size<1024?selected.Size+' bytes':formatBytes(selected.Size));selectionSummary.title=selected.Key;
  const metadata=[['Object key',selected.Key],['Bucket',listingContext.Bucket],['Size',selected.Size.toLocaleString()+' bytes'],['ETag',selected.ETag||'Not provided'],['Storage class',selected.StorageClass||'Not provided']];
  selectionMetadata.replaceChildren();for(const [label,value] of metadata){const term=document.createElement('dt'),description=document.createElement('dd');term.textContent=label;description.textContent=value;selectionMetadata.append(term);selectionMetadata.append(description)}
  if($('details').children[0]!==selectionInspector)$('details').replaceChildren(selectionInspector);
 }else $('details').textContent=files.length?files.length+' files selected · '+files.reduce((n,o)=>n+o.Size,0).toLocaleString()+' bytes':'Select an object to inspect its details.';
 for(let i=0;i<$('objects').children.length;i++){const row=$('objects').children[i],chosen=!!objects[i]&&!objects[i].Folder&&selectedKeys.has(objects[i].Key);if(row.children[0]?.children[0]?.type==='checkbox')row.children[0].children[0].checked=chosen;row.classList[chosen?'add':'remove']('selected');row.setAttribute('aria-selected',String(chosen))}
 const selectable=currentListing()?objects.filter(o=>!o.Folder).length:0;$('select-all').checked=selectable>0&&files.length===selectable;$('select-all').indeterminate=files.length>0&&files.length<selectable;$('select-all').disabled=!selectable;
}
function clearSelection(){selectedKeys.clear();updateSelection()}clearSelection();
$('select-all').onclick=()=>{if(!currentListing())return;const files=objects.filter(o=>!o.Folder);if(files.length&&files.every(o=>selectedKeys.has(o.Key)))selectedKeys.clear();else for(const o of files)selectedKeys.add(o.Key);updateSelection()};

$('new-folder').onclick=action(async()=>{if(!bucket)throw Error('Choose a bucket first');const target={ProfileID:profile,Bucket:bucket,Prefix:prefix};const name=prompt('Create a folder in '+bucket+'/'+prefix+'\nFolder name:');if(name===null)return;const created=await api('create-folder',{...target,Key:name});if(profile===target.ProfileID&&bucket===target.Bucket&&prefix===target.Prefix)await browse();message('Created folder: '+created)});

let deleteState=null,deleteRequest=0,deleteExecuting=false;
function deletionCount(value,noun){return value+' '+noun+(value===1?'':'s')}
function updateDeleteConfirmation(){
 const expected=deleteState?.confirmation||'';
 $('delete-confirm').disabled=!deleteState?.ready||deleteExecuting||(expected&&$('delete-confirmation').value!==expected);
 $('delete-confirmation').disabled=deleteExecuting;$('delete-cancel').disabled=deleteExecuting;$('delete-review').disabled=deleteExecuting;
}
function closeDeleteDialog(){if(deleteExecuting)return;deleteRequest++;deleteState=null;$('delete-dialog').close()}
function prepareDeleteDialog(state){
 deleteRequest++;deleteState=state;$('delete-title').textContent=state.kind==='files'?'Delete selected files':state.kind==='folder'?'Delete folder and contents':'Delete bucket and contents';
 $('delete-context').textContent=state.connection+' · '+state.target.Bucket+(state.kind==='folder'?'/'+state.target.Prefix:'');
 $('delete-description').textContent=state.kind==='files'?'Review the exact files below. Bucket versioning and retention rules apply.':state.kind==='folder'?'Permanently delete this exact folder prefix, including its objects, all versions, delete markers and unfinished multipart uploads.':'Permanently delete this bucket and its contents, including all versions, delete markers and unfinished multipart uploads. The bucket name may become available to others.';
 $('delete-targets').textContent=state.kind==='files'?state.files.map(o=>state.target.Bucket+'/'+o.Key).join('\n'):state.target.Bucket+'/'+(state.target.Prefix||'');
 $('delete-summary').textContent=state.kind==='files'?state.files.length+' selected file'+(state.files.length===1?'':'s'):'Preparing a deletion preview…';
 $('delete-error').textContent='';$('delete-review').hidden=true;$('delete-confirmation').value='';$('delete-confirm-label').hidden=!state.confirmation;
 $('delete-confirm-prompt').textContent=state.confirmation?'Type '+state.confirmation+' to confirm':'';
 $('delete-confirmation').required=!!state.confirmation;updateDeleteConfirmation();
 if(!$('delete-dialog').open)$('delete-dialog').showModal();$('delete-cancel').focus?.();
}
$('delete-confirmation').oninput=updateDeleteConfirmation;$('delete-cancel').onclick=closeDeleteDialog;
$('delete-dialog').oncancel=e=>{if(deleteExecuting)e.preventDefault();else{deleteRequest++;deleteState=null}};
$('delete').onclick=action(async()=>{
 if(deleting||deleteExecuting)return;const files=selectedFiles();if(!files.length)throw Error('Select files first');
 const target=listingContext;prepareDeleteDialog({kind:'files',target,files:files.map(o=>Object.freeze({...o})),connection:connectionProfiles.find(p=>p.ID===target.ProfileID)?.Name||'Selected connection',confirmation:files.length>1?'DELETE':'',ready:true});
});
async function previewDelete(kind,owner,name,folder=''){
 if(deleteExecuting)return;const target=Object.freeze({ProfileID:owner,Bucket:name,Prefix:folder,Kind:kind}),state={kind,target,connection:connectionProfiles.find(p=>p.ID===owner)?.Name||'Selected connection',confirmation:kind==='folder'?folder:name,ready:false};
 prepareDeleteDialog(state);const request=deleteRequest;
 try{
  const plan=await api('preview-delete',target);if(request!==deleteRequest||deleteState!==state)return;
  state.plan=plan;state.confirmation=plan.Confirmation;state.ready=true;
  $('delete-summary').textContent=[deletionCount(plan.Objects,'object'),deletionCount(plan.Versions,'version'),deletionCount(plan.DeleteMarkers,'delete marker'),deletionCount(plan.MultipartUploads,'unfinished upload'),formatBytes(plan.TotalBytes)].join(' · ');
  $('delete-targets').textContent=target.Bucket+'/'+target.Prefix+'\n\nPreview sample (up to 8 keys; counts cover the entire scope):\n'+(plan.Keys||[]).join('\n');
  $('delete-confirm-prompt').textContent='Type '+state.confirmation+' to confirm';updateDeleteConfirmation();
 }catch(e){if(request!==deleteRequest||deleteState!==state)return;$('delete-error').textContent=e.message;$('delete-summary').textContent='Nothing has been deleted.';$('delete-review').hidden=false;updateDeleteConfirmation()}
}
$('delete-review').onclick=action(async()=>{const state=deleteState;if(state&&state.kind!=='files')await previewDelete(state.kind,state.target.ProfileID,state.target.Bucket,state.target.Prefix)});
async function deleteSelectedFiles(state){
 const failed=[];let deleted=0,refreshWarning='';
 for(const o of state.files){try{await api('delete-file',{ProfileID:state.target.ProfileID,Bucket:state.target.Bucket,Key:o.Key,ETag:o.ETag});deleted++}catch(e){failed.push({key:o.Key,error:e.message})}}
 if(currentListing(state.target)){
  selectedKeys=new Set(failed.map(f=>f.key));updateSelection();
  try{await browse()}catch(e){refreshWarning='Listing refresh failed: '+e.message}
  if(currentListing(state.target)){selectedKeys=new Set(failed.map(f=>f.key));updateSelection()}
 }
 const summary='Deleted '+deleted+' file'+(deleted===1?'':'s')+(failed.length?' · Failed: '+failed.map(f=>f.key+': '+f.error).join(' · '):'')+(refreshWarning?' · '+refreshWarning:'');
 $('delete-dialog').close();deleteState=null;message(summary);
}
async function refreshDeletedScope(state,result){
 if(profile!==state.target.ProfileID)return '';
 try{
  if(state.kind==='bucket'){
   if(result.BucketDeleted&&bucket===state.target.Bucket){bucket='';prefix='';objectFilter='';filterLocation='';browseRequest++;closePathSuggestions();$('path').value='';$('object-search').value='';renderBreadcrumbs();clearListing()}
   const request=connectionRequest,names=await api('buckets',{ProfileID:state.target.ProfileID});if(profile===state.target.ProfileID&&request===connectionRequest)renderBuckets(names,state.target.ProfileID);
  }else if(bucket===state.target.Bucket)await browse(false,{quiet:true});
 }catch(e){return 'Listing refresh failed: '+e.message}
 return '';
}
$('delete-form').onsubmit=async e=>{
 e.preventDefault();const state=deleteState;if(!state?.ready||deleteExecuting||(state.confirmation&&$('delete-confirmation').value!==state.confirmation))return;
 deleteExecuting=true;deleting=true;state.ready=false;updateDeleteConfirmation();updateSelection();$('delete-error').textContent='';
 try{
  if(state.kind==='files')await deleteSelectedFiles(state);
  else{
   const result=await api('execute-delete',{PlanID:state.plan.PlanID,Confirmation:$('delete-confirmation').value});
   const warning=await refreshDeletedScope(state,result),failures=(result.Failures||[]).map(f=>f.Key+': '+f.Error);
   const summary='Deleted '+result.Deleted+(result.Deleted===1?' object or version':' objects or versions')+' · Aborted '+deletionCount(result.Aborted,'unfinished upload')+(result.BucketDeleted?' · Bucket deleted':'')+(warning?' · '+warning:'');
   message(summary+(failures.length?' · Some items could not be deleted':'')+(result.Remaining?' · '+result.Remaining:''));
   if(!result.Completed&&!failures.length)$('message').className='status-warning';
   if(result.Completed){$('delete-dialog').close();deleteState=null}
   else{$('delete-summary').textContent=summary;$('delete-error').textContent=[...failures,result.Remaining||'Review a fresh preview before deleting any remaining items.'].filter(Boolean).join('\n');$('delete-review').hidden=false;$('delete-confirmation').value=''}
  }
 }catch(error){$('delete-error').textContent=state.kind==='files'?error.message:'Could not confirm the deletion result: '+error.message+'. Some items may already have been deleted. Review remaining to prepare a fresh preview.';if(state.kind!=='files')$('delete-review').hidden=false;else state.ready=true}
 finally{deleteExecuting=false;deleting=false;updateDeleteConfirmation();updateSelection()}
};

function renderBreadcrumbs(){
 const container=$('location');container.replaceChildren();
 if(!bucket){container.textContent='Choose a bucket';return}
 const owner={ProfileID:profile,Bucket:bucket,Connection:connectionRequest},crumbs=[{label:bucket,path:''}];let path='';
 for(const segment of prefix.match(/[^/]*\/|[^/]+$/g)||[]){path+=segment;crumbs.push({label:segment.endsWith('/')?segment.slice(0,-1)||'(empty)':segment,path})}
 crumbs.forEach((crumb,index)=>{
  if(index){const separator=document.createElement('span');separator.textContent='›';separator.className='breadcrumb-separator';separator.setAttribute('aria-hidden','true');container.append(separator)}
  const link=button(crumb.label,async()=>{if(profile!==owner.ProfileID||bucket!==owner.Bucket||connectionRequest!==owner.Connection)return;prefix=crumb.path;await browse()});link.className='breadcrumb';link.title=crumb.path?bucket+'/'+crumb.path:'Bucket root: '+bucket;
  if(index===crumbs.length-1){link.setAttribute('aria-current','location');link.classList.add('current')}
  container.append(link);
 });
}

function renderObjects(selectedKey=[...selectedKeys]){
 const context=listingContext;
 const field={name:'Key',size:'Size',modified:'Modified',storage:'StorageClass'}[sortKey];
 objects.sort((a,b)=>{const av=a[field]??'',bv=b[field]??'';const compared=sortKey==='size'?Number(av)-Number(bv):String(av).localeCompare(String(bv));return (compared||a.Key.localeCompare(b.Key))*(sortAscending?1:-1)});
 $('object-count').textContent='('+objects.length+(next?'+':'')+')';
 $('sort-scope').textContent=next?'More objects available · sorting loaded entries only':'Sorting all listed entries';
 $('empty-objects').hidden=objects.length>0;$('empty-objects').textContent=listingState==='loading'?'Loading objects…':listingState==='error'?'Unable to load objects. Refresh to try again.':!profile?(connectionProfiles.length?'Choose a connection, then a bucket to see its objects.':'Add your first connection to start browsing S3 storage.'):!bucket?'Choose a bucket to see its objects.':objectFilter?'No names match this prefix. Clear the search to see all objects.':'No objects in this folder. Upload a file or create a folder to get started.';
 for(const key of ['name','size','modified','storage'])$('heading-'+key).setAttribute('aria-sort',key===sortKey?(sortAscending?'ascending':'descending'):'none');
$('objects').replaceChildren(...objects.map(o=>{const row=document.createElement('tr');const cell=document.createElement('td');cell.className='selection-column';if(!o.Folder){const control=document.createElement('input');control.type='checkbox';control.setAttribute('aria-label','Select '+o.Key);control.onclick=e=>{e.stopPropagation();row.click()};cell.append(control)}row.append(cell);for(const value of [(o.Folder?'▸ ':'')+o.Key.slice(prefix.length),o.Folder?'—':formatBytes(o.Size),o.Modified?new Date(o.Modified).toLocaleString():'—',o.StorageClass||'—']){const td=document.createElement('td');td.textContent=value;td.title=value;row.append(td)}row.children[1].title=o.Key;row.children[2].title=o.Folder?'Folder':o.Size.toLocaleString()+' bytes';row.className=o.Folder?'object-folder':'object-file';row.tabIndex=-1;row.setAttribute('aria-selected','false');row.onclick=action(async()=>{if(listingContext!==context||!currentListing(context))return;if(o.Folder){prefix=o.Key;await browse();return}if(selectedKeys.has(o.Key))selectedKeys.delete(o.Key);else selectedKeys.add(o.Key);updateSelection()});row.onkeydown=e=>{if(e.target&&e.target!==row)return;if(e.key==='Enter'||e.key===' '){e.preventDefault();row.click()}};if(o.Folder){const open=button('▸ '+o.Key.slice(prefix.length),()=>row.click());open.className='folder-link';open.setAttribute('aria-label','Open folder '+o.Key);open.onclick=e=>{e?.stopPropagation();return row.click()};row.children[1].replaceChildren(open);const remove=button('Delete folder…',()=>{});remove.className='delete-folder-action';remove.setAttribute('aria-label','Delete folder '+o.Key);remove.onclick=action(async()=>{if(listingContext!==context||!currentListing(context))return;await previewDelete('folder',context.ProfileID,context.Bucket,o.Key)});const removeClick=remove.onclick;remove.onclick=e=>{e?.stopPropagation();return removeClick()};row.children[1].append(remove);row.title='Open folder '+o.Key}return row}));selectedKeys=new Set((selectedKey||[]).filter(key=>objects.some(o=>!o.Folder&&o.Key===key)));updateSelection();
}
for(const key of ['name','size','modified','storage'])$('sort-'+key).onclick=()=>{const selectedKey=[...selectedKeys];sortAscending=sortKey===key?!sortAscending:true;sortKey=key;renderObjects(selectedKey)};
$('object-search-form').onsubmit=async e=>{e.preventDefault();if(!bucket){message('Choose a bucket first');return}objectFilter=$('object-search').value;try{await browse()}catch(e){message(e.message)}};
$('clear-search').onclick=action(async()=>{objectFilter='';$('object-search').value='';if(bucket)await browse()});

// Query storage, rather than only the loaded table, for path completion.
var pathTimer, pathRequest=0, pathMatches=[], pathActive=-1, pathHasMore=false;
function closePathSuggestions(){clearTimeout(pathTimer);pathRequest++;pathMatches=[];pathActive=-1;pathHasMore=false;$('path-suggestions').hidden=true;$('path').setAttribute('aria-expanded','false');$('path').setAttribute('aria-activedescendant','')}
async function suggestPaths(token='',append=false){
 const input=$('path').value,target={ProfileID:profile,Bucket:bucket,Prefix:input},request=++pathRequest;
 if(!bucket){closePathSuggestions();return}
 try{
  const page=await api('browse',{...target,Token:token});
  if(request!==pathRequest||input!==$('path').value||target.ProfileID!==profile||target.Bucket!==bucket)return;
  pathMatches=(append?pathMatches:[]).concat(page.Objects.filter(o=>o.Key.startsWith(input)));pathActive=-1;pathHasMore=!!page.Next;
  const rows=pathMatches.map((o,i)=>{const row=button(o.Key,()=>{if(request!==pathRequest||input!==$('path').value||target.ProfileID!==profile||target.Bucket!==bucket)return;return choosePath(o,true)});row.id='path-option-'+i;row.setAttribute('role','option');row.setAttribute('aria-selected','false');row.onmousedown=e=>e.preventDefault();return row});
  $('path-suggestions').replaceChildren(...rows);
  if(page.Next){const more=button('Load more matches',()=>{if(request!==pathRequest||input!==$('path').value||target.ProfileID!==profile||target.Bucket!==bucket)return;return suggestPaths(page.Next,true)});more.onmousedown=e=>e.preventDefault();$('path-suggestions').append(more)}
  if(!rows.length&&!page.Next)$('path-suggestions').textContent='No matching folders or files.';
  $('path-suggestions').hidden=false;$('path').setAttribute('aria-expanded','true');$('path').setAttribute('aria-activedescendant','');
 }catch(e){if(request===pathRequest){closePathSuggestions();message('Unable to suggest paths: '+e.message)}}
}
async function choosePath(o,navigate=false){
 closePathSuggestions();$('path').value=o.Key;
 if(!navigate){if(o.Folder)await suggestPaths();return}
 if(o.Folder){prefix=o.Key;await browse();return}
 prefix=o.Key.slice(0,o.Key.lastIndexOf('/')+1);filterLocation=JSON.stringify([profile,bucket,prefix]);objectFilter=o.Key.slice(prefix.length);$('object-search').value=objectFilter;
 if(await browse()){selectedKeys=new Set([o.Key]);updateSelection();$('path').value=o.Key;message('Selected file: '+o.Key)}
}
$('path').oninput=()=>{closePathSuggestions();pathTimer=setTimeout(suggestPaths,180)};
$('path').onfocus=()=>{closePathSuggestions();pathTimer=setTimeout(suggestPaths,180)};
$('path').onblur=()=>{closePathSuggestions()};
$('path').onkeydown=e=>{
 if(e.key==='Escape'){e.preventDefault();closePathSuggestions();return}
 if((e.key==='ArrowDown'||e.key==='ArrowUp')&&pathMatches.length){
  e.preventDefault();pathActive=(pathActive+(e.key==='ArrowDown'?1:-1)+pathMatches.length)%pathMatches.length;
  for(let i=0;i<$('path-suggestions').children.length;i++){$('path-suggestions').children[i].setAttribute('aria-selected',String(i===pathActive))}
  $('path').setAttribute('aria-activedescendant','path-option-'+pathActive);return;
 }
 if(e.key==='Tab'&&pathMatches.length){
  e.preventDefault();
  if(pathActive>=0){choosePath(pathMatches[pathActive]);return}
  if(pathHasMore){message('More matching names are available. Type more or load more matches before completing.');return}
  if(pathMatches.length===1){choosePath(pathMatches[0]);return}
  let shared=pathMatches[0].Key;
  for(const match of pathMatches.slice(1)){while(!match.Key.startsWith(shared))shared=shared.slice(0,-1)}
  if(shared.length>$('path').value.length){$('path').value=shared;suggestPaths()}
  return;
 }
 if(e.key==='Enter'){e.preventDefault();if(pathActive>=0)action(()=>choosePath(pathMatches[pathActive],true))();else $('go').click()}
};

async function openTypedPath(){
 const typed=$('path').value,targetProfile=profile,targetBucket=bucket;
 if(!bucket)throw Error('Choose a bucket first');
 if(typed&&!typed.endsWith('/')){
  const request=++browseRequest;closePathSuggestions();clearListing('loading');$('more').disabled=true;message('Loading…');
  let page;try{page=await api('browse',{ProfileID:targetProfile,Bucket:targetBucket,Prefix:typed})}catch(e){if(request!==browseRequest||profile!==targetProfile||bucket!==targetBucket)return;clearListing('error');throw e}
  if(request!==browseRequest||profile!==targetProfile||bucket!==targetBucket)return;
  if($('path').value!==typed){clearListing();return}
  const file=page.Objects.find(o=>!o.Folder&&o.Key===typed);
  if(file){await choosePath(file,true);return}
 }
 prefix=typed;await browse();
}

var connectionProfiles=[],editingConnection='',credentialDirty={};
function credentialValue(name){return editingConnection&&!credentialDirty[name]?'':$('connection-'+name).value}
function credentialFields(){
 for(const name of ['AccessKey','SecretKey','SessionToken']){
 const field=$('connection-'+name);field.disabled=false;field.required=!editingConnection&&name!=='SessionToken';field.placeholder=editingConnection?(name==='SessionToken'?'New session token, if needed':'Saved securely — leave blank to keep'):'';
 }
 $('credential-help').textContent=editingConnection?'Saved keys are masked. Select a field and type to replace it; untouched or blank key fields keep the saved key. Include a session token if the replacement credentials require one.':'Credentials are stored in your operating system’s secure vault.';
}
function editConnection(p=null){
 editingConnection=p?.ID||'';$('connection-title').textContent=p?'Edit connection':'New connection';$('connection-error').textContent='';
 for(const name of ['Name','Endpoint','Region','CAFile'])$('connection-'+name).value=p?.[name]||(name==='Region'?'us-east-1':'');
 $('connection-PathStyle').checked=!!p?.PathStyle;
 credentialDirty={};
 for(const name of ['AccessKey','SecretKey','SessionToken']){const field=$('connection-'+name);field.value=p&&name!=='SessionToken'?'••••••••••••':'';field.oninput=()=>{credentialDirty[name]=true};field.onfocus=()=>{if(editingConnection&&!credentialDirty[name]&&name!=='SessionToken')field.select()}};
 credentialFields();$('connection').showModal();
}



function renderBuckets(names,owner){
 $('buckets').replaceChildren(...names.map(name=>{
 const row=document.createElement('div');row.className='connection-row';
 const select=document.createElement('button');select.textContent='▱ '+name;select.title=name;select.setAttribute('aria-pressed',String(bucket===name));select.className='connection-select';select.onclick=action(async()=>{if(profile!==owner)return;bucket=name;prefix='';for(const peer of $('buckets').children){peer.children[0].classList.remove('active');peer.children[0].setAttribute('aria-pressed','false')}select.classList.add('active');select.setAttribute('aria-pressed','true');await browse()});if(bucket===name)select.classList.add('active');
 const menu=document.createElement('details');menu.className='connection-menu';const toggle=document.createElement('summary');toggle.textContent='⋯';toggle.setAttribute('aria-label','Actions for bucket '+name);
 const choices=document.createElement('div');choices.className='connection-choices';
 choices.append(button('Delete',async()=>{menu.open=false;await deleteBucket(owner,name)}));menu.append(toggle);menu.append(choices);row.append(select);row.append(menu);return row;
 }));
}
async function deleteBucket(owner,name){await previewDelete('bucket',owner,name)}

updateWorkspace();
