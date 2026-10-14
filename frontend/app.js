const $=id=>document.getElementById(id);let session=location.hash.slice(1)||sessionStorage.getItem('harbor-session');if(session){sessionStorage.setItem('harbor-session',session);history.replaceState(null,'',location.pathname)}let profile='',bucket='',prefix='',next='',objects=[],selected=null,selectedKeys=new Set(),deleting=false,browseRequest=0,connectionRequest=0,listingContext=null,listingState='empty',objectFilter='',filterLocation='',sortKey='name',sortAscending=true;async function api(route,data={}){const r=await fetch('/api/'+route,{method:'POST',headers:{'Content-Type':'application/json','Authorization':'Bearer '+session},body:JSON.stringify(data)});let v=await r.json();if(!r.ok)throw Error(v.error||'Request failed');return v}function message(s){$('message').textContent=s;$('message').className=/failed|error|unable|could not|denied/i.test(s)?'status-error':/completed|saved|deleted|created/i.test(s)?'status-success':''}function action(fn){return async()=>{try{await fn()}catch(e){message(e.message)}}}function button(text,fn){const b=document.createElement('button');b.type='button';b.textContent=text;b.onclick=action(async()=>{await fn()});return b}async function selectConnection(p){
 const request=++connectionRequest;browseRequest++;profile=p.ID;bucket='';prefix='';objectFilter='';filterLocation='';$('path').value='';$('object-search').value='';closePathSuggestions();renderBreadcrumbs();clearListing();$('buckets').replaceChildren();message('Loading buckets…');
 for(const row of $('profiles').children){const b=row.children[0];b.classList[b.dataset.profile===p.ID?'add':'remove']('active');b.setAttribute('aria-pressed',String(b.dataset.profile===p.ID))}
 let buckets;try{buckets=await api('buckets',{ProfileID:p.ID})}catch(e){if(request!==connectionRequest||profile!==p.ID)return;throw e}
 if(request!==connectionRequest||profile!==p.ID)return;renderBuckets(buckets,p.ID);message(buckets.length+' buckets available');
}
async function profiles(){
 const list=await api('profiles');connectionProfiles=list;
 $('profiles').replaceChildren(...list.map(p=>{
 const row=document.createElement('div');row.className='connection-row';
 const select=document.createElement('button');select.textContent=p.Name;select.dataset.profile=p.ID;select.className='connection-select';select.onclick=action(()=>selectConnection(p));if(profile===p.ID)select.classList.add('active');
 const menu=document.createElement('details');menu.className='connection-menu';const toggle=document.createElement('summary');toggle.textContent='⋯';toggle.setAttribute('aria-label','Actions for '+p.Name);toggle.title='Actions for '+p.Name;
 const choices=document.createElement('div');choices.className='connection-choices';
 choices.append(button('Edit',()=>{menu.open=false;editConnection(p)}));choices.append(button('Delete',async()=>{menu.open=false;await deleteConnection(p)}));menu.append(toggle);menu.append(choices);row.append(select);row.append(menu);return row;
 }));
}
async function deleteConnection(p){
 if(!confirm('Delete connection “'+p.Name+'”?\nThis removes its saved settings and credentials from this computer. Buckets and files will not be deleted.'))return;
 await api('delete-profile',{ProfileID:p.ID});
 if(profile===p.ID){profile='';bucket='';prefix='';objectFilter='';filterLocation='';connectionRequest++;browseRequest++;closePathSuggestions();$('path').value='';$('object-search').value='';$('buckets').replaceChildren();renderBreadcrumbs();clearListing()}
 await profiles();message('Deleted connection: '+p.Name);
}
// Keep each visible listing attached to the location that produced it.
function currentListing(context=listingContext){return !!context&&context.Connection===connectionRequest&&profile===context.ProfileID&&bucket===context.Bucket&&prefix===context.Prefix&&objectFilter===context.Filter}
function clearListing(state='empty'){listingContext=null;listingState=state;objects=[];next='';selectedKeys.clear();$('more').hidden=true;$('more').disabled=false;renderObjects([])}
async function browse(append=false,options={}){
 closePathSuggestions();const scope=JSON.stringify([profile,bucket,prefix]);if(filterLocation!==scope){objectFilter='';$('object-search').value='';filterLocation=scope}
 const previousContext=currentListing()?listingContext:null;
 if(append&&(!previousContext||!next||listingState==='loading'))return false;
 const previousObjects=append?objects.slice():[],token=append?next:'';
 const request=++browseRequest,target=Object.freeze({ProfileID:profile,Bucket:bucket,Prefix:prefix,Filter:objectFilter,Connection:connectionRequest});
 if(!previousContext)clearListing('loading');else listingState='loading';
 $('path').value=target.Prefix;$('path').placeholder='Bucket root';renderBreadcrumbs();$('more').disabled=true;
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
 const saved=await api('save',{Profile:{ID:editingConnection,Name:f.get('Name'),Endpoint:f.get('Endpoint'),Region:f.get('Region'),PathStyle:f.has('PathStyle'),CAFile:f.get('CAFile')},Secret:{AccessKey:credentialValue('AccessKey'),SecretKey:credentialValue('SecretKey'),SessionToken:credentialValue('SessionToken')}});
 e.target.reset();$('connection').close();await profiles();
 const index=connectionProfiles.findIndex(p=>p.ID===saved.ID);if(index>=0)await $('profiles').children[index].children[0].click();
 message('Connection saved securely');
 }catch(e){$('connection-error').textContent=e.message}
};$('refresh').onclick=action(()=>browse());$('go').onclick=action(openTypedPath);$('up').onclick=action(async()=>{prefix=prefix.replace(/[^/]+\/$/,'');await browse()});$('test').onclick=action(async()=>message(await api('test',{ProfileID:profile})));$('more').onclick=action(()=>browse(true));action(profiles)();

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


function selectedFiles(){return currentListing()?objects.filter(o=>!o.Folder&&selectedKeys.has(o.Key)):[]}
function updateSelection(){
 const files=selectedFiles();selected=files.length===1?files[0]:null;
 $('selection-count').textContent=files.length?files.length+' file'+(files.length===1?'':'s')+' selected':'No file selected';
 $('download').disabled=!files.length;$('delete').disabled=!files.length||deleting;
 $('details').textContent=selected?'Selected: '+selected.Key+' · '+selected.Size+' bytes · ETag '+selected.ETag:files.length?files.length+' files selected · '+files.reduce((n,o)=>n+o.Size,0).toLocaleString()+' bytes':'Select an object to inspect its details.';
 for(let i=0;i<$('objects').children.length;i++){const row=$('objects').children[i],chosen=!!objects[i]&&!objects[i].Folder&&selectedKeys.has(objects[i].Key);if(row.children[0]?.children[0]?.type==='checkbox')row.children[0].children[0].checked=chosen;row.classList[chosen?'add':'remove']('selected');row.setAttribute('aria-selected',String(chosen))}
 const selectable=currentListing()?objects.filter(o=>!o.Folder).length:0;$('select-all').checked=selectable>0&&files.length===selectable;$('select-all').indeterminate=files.length>0&&files.length<selectable;$('select-all').disabled=!selectable;
}
function clearSelection(){selectedKeys.clear();updateSelection()}clearSelection();
$('select-all').onclick=()=>{if(!currentListing())return;const files=objects.filter(o=>!o.Folder);if(files.length&&files.every(o=>selectedKeys.has(o.Key)))selectedKeys.clear();else for(const o of files)selectedKeys.add(o.Key);updateSelection()};

$('new-folder').onclick=action(async()=>{if(!bucket)throw Error('Choose a bucket first');const target={ProfileID:profile,Bucket:bucket,Prefix:prefix};const name=prompt('Create a folder in '+bucket+'/'+prefix+'\nFolder name:');if(name===null)return;const created=await api('create-folder',{...target,Key:name});if(profile===target.ProfileID&&bucket===target.Bucket&&prefix===target.Prefix)await browse();message('Created folder: '+created)});

$('delete').onclick=action(async()=>{
 if(deleting)return;const files=selectedFiles(),target=listingContext;if(!files.length)throw Error('Select files first');
 if(!confirm('Delete '+files.length+' selected file'+(files.length===1?'':'s')+'?\n\n'+files.map(o=>target.Bucket+'/'+o.Key).join('\n')+'\n\nThis may permanently remove them. Bucket versioning and retention rules apply.'))return;
 deleting=true;updateSelection();const failed=[];let deleted=0;
 try{for(const o of files){try{await api('delete-file',{ProfileID:target.ProfileID,Bucket:target.Bucket,Key:o.Key,ETag:o.ETag});deleted++}catch(e){failed.push({key:o.Key,error:e.message})}}
 if(currentListing(target)){const refreshed=await browse();if(refreshed&&currentListing(target)){selectedKeys=new Set(failed.map(f=>f.key));updateSelection()}}
 message('Deleted '+deleted+' file'+(deleted===1?'':'s')+(failed.length?' · Failed: '+failed.map(f=>f.key+': '+f.error).join(' · '):''));
 }finally{deleting=false;updateSelection()}
});

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
 $('empty-objects').hidden=objects.length>0;$('empty-objects').textContent=listingState==='loading'?'Loading objects…':listingState==='error'?'Unable to load objects. Refresh to try again.':!bucket?'Choose a bucket to see its objects.':objectFilter?'No names match this prefix. Clear the search to see all objects.':'No objects in this folder. Upload a file or create a folder to get started.';
 for(const key of ['name','size','modified','storage'])$('heading-'+key).setAttribute('aria-sort',key===sortKey?(sortAscending?'ascending':'descending'):'none');
$('objects').replaceChildren(...objects.map(o=>{const row=document.createElement('tr');const cell=document.createElement('td');cell.className='selection-column';if(!o.Folder){const control=document.createElement('input');control.type='checkbox';control.setAttribute('aria-label','Select '+o.Key);control.onclick=e=>{e.stopPropagation();row.click()};cell.append(control)}row.append(cell);for(const value of [(o.Folder?'▸ ':'')+o.Key.slice(prefix.length),o.Folder?'—':o.Size.toLocaleString()+' B',o.Modified?new Date(o.Modified).toLocaleString():'—',o.StorageClass||'—']){const td=document.createElement('td');td.textContent=value;row.append(td)}row.tabIndex=0;row.setAttribute('aria-selected','false');row.onclick=action(async()=>{if(listingContext!==context||!currentListing(context))return;if(o.Folder){prefix=o.Key;await browse();return}if(selectedKeys.has(o.Key))selectedKeys.delete(o.Key);else selectedKeys.add(o.Key);updateSelection()});row.onkeydown=e=>{if(e.key==='Enter'||e.key===' '){e.preventDefault();row.click()}};if(o.Folder)row.title='Open folder';return row}));selectedKeys=new Set((selectedKey||[]).filter(key=>objects.some(o=>!o.Folder&&o.Key===key)));updateSelection();
}
for(const key of ['name','size','modified','storage'])$('sort-'+key).onclick=()=>{const selectedKey=[...selectedKeys];sortAscending=sortKey===key?!sortAscending:true;sortKey=key;renderObjects(selectedKey)};
$('object-search-form').onsubmit=async e=>{e.preventDefault();if(!bucket){message('Choose a bucket first');return}objectFilter=$('object-search').value;try{await browse()}catch(e){message(e.message)}};
$('clear-search').onclick=action(async()=>{objectFilter='';$('object-search').value='';if(bucket)await browse()});

// Query storage, rather than only the loaded table, for path completion.
var pathTimer, pathRequest=0, pathMatches=[], pathActive=-1;
function closePathSuggestions(){clearTimeout(pathTimer);pathRequest++;pathMatches=[];pathActive=-1;$('path-suggestions').hidden=true;$('path').setAttribute('aria-expanded','false');$('path').setAttribute('aria-activedescendant','')}
async function suggestPaths(token='',append=false){
 const input=$('path').value,target={ProfileID:profile,Bucket:bucket,Prefix:input},request=++pathRequest;
 if(!bucket){closePathSuggestions();return}
 try{
  const page=await api('browse',{...target,Token:token});
  if(request!==pathRequest||input!==$('path').value||target.ProfileID!==profile||target.Bucket!==bucket)return;
  pathMatches=(append?pathMatches:[]).concat(page.Objects.filter(o=>o.Key.startsWith(input)));pathActive=-1;
  const rows=pathMatches.map((o,i)=>{const row=button(o.Key,()=>{if(request!==pathRequest||input!==$('path').value||target.ProfileID!==profile||target.Bucket!==bucket)return;return choosePath(o,true)});row.id='path-option-'+i;row.setAttribute('role','option');row.setAttribute('aria-selected','false');row.onmousedown=e=>e.preventDefault();return row});
  $('path-suggestions').replaceChildren(...rows);
  if(page.Next){const more=button('Load more matches',()=>{if(request!==pathRequest||input!==$('path').value||target.ProfileID!==profile||target.Bucket!==bucket)return;return suggestPaths(page.Next,true)});more.onmousedown=e=>e.preventDefault();$('path-suggestions').append(more)}
  if(!rows.length)$('path-suggestions').textContent='No matching folders or files.';
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
  if(pathActive>=0||pathMatches.length===1){choosePath(pathMatches[pathActive>=0?pathActive:0]);return}
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
 const select=document.createElement('button');select.textContent='▱ '+name;select.className='connection-select';select.onclick=action(async()=>{if(profile!==owner)return;bucket=name;prefix='';for(const peer of $('buckets').children)peer.children[0].classList.remove('active');select.classList.add('active');await browse()});if(bucket===name)select.classList.add('active');
 const menu=document.createElement('details');menu.className='connection-menu';const toggle=document.createElement('summary');toggle.textContent='⋯';toggle.setAttribute('aria-label','Actions for bucket '+name);
 const choices=document.createElement('div');choices.className='connection-choices';
 choices.append(button('Delete',async()=>{menu.open=false;await deleteBucket(owner,name)}));menu.append(toggle);menu.append(choices);row.append(select);row.append(menu);return row;
 }));
}
async function deleteBucket(owner,name){
 const connection=connectionProfiles.find(p=>p.ID===owner)?.Name||'Selected connection';
 const confirmation=prompt('Delete empty bucket “'+name+'” from '+connection+'?\nThe bucket must contain no objects, versions, or delete markers. This cannot be undone, and its name may become available to others.\n\nType the exact bucket name to confirm:');
 if(confirmation===null)return;
 if(confirmation!==name){message('Bucket not deleted: the confirmation name did not match.');return}
 await api('delete-bucket',{ProfileID:owner,Bucket:name,Confirmation:confirmation});
 if(profile===owner){
 if(bucket===name){bucket='';prefix='';objectFilter='';filterLocation='';browseRequest++;closePathSuggestions();$('path').value='';$('object-search').value='';renderBreadcrumbs();clearListing()}
 const request=connectionRequest,names=await api('buckets',{ProfileID:owner});if(profile===owner&&request===connectionRequest)renderBuckets(names,owner);
 }
 message('Deleted bucket: '+name);
}
