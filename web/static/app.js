const $ = (id) => document.getElementById(id);
const escapeHTML = (value) => String(value ?? '').replace(/[&<>"']/g, (char) => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[char]));
const pages = {
  listeners: {title:'监听管理', kicker:'LISTENERS', description:'为固定端口绑定一条节点线路，保存后自动应用。', create:'＋ 新增监听', table:'固定监听', note:'HTTP / SOCKS5 · mixed', foot:'删除监听不会删除节点。更换绑定会影响该端口的新连接。'},
  nodes: {title:'节点池', kicker:'NODE LIBRARY', description:'管理可选节点，按需绑定到监听端口。', create:'＋ 导入节点', table:'全部节点', note:'节点本身不开放本地端口', foot:'延迟为访问 gstatic 测试地址的耗时。刷新页面可继续查看，重启服务后清空检测结果。'},
  subscriptions: {title:'订阅管理', kicker:'SUBSCRIPTIONS', description:'更新节点来源，保留已有监听与节点的绑定关系。', create:'＋ 添加订阅', table:'订阅来源', note:'手动同步 · 固定端口', foot:'订阅中消失的节点会标记为不可用，关联监听关闭，不会自动切换到其他节点。'},
};
let state = {nodes:[],listeners:[],subscriptions:[]};
let page = 'listeners';
let busy = false;
let confirmation = null;
let checks = {results:{}, batch:null};
let checkRequestBusy = false;
let checksLoading = false;
let checkSerial = 0;
let appliedCheckSerial = 0;


async function api(path, method='GET', body) {
  const response = await fetch(path, {method, credentials:'same-origin', headers:{'Content-Type':'application/json'}, body:body===undefined?undefined:JSON.stringify(body)});
  const data = await response.json();
  if (response.status===401 && path!=='/api/login') showLogin();
  if (!response.ok) throw new Error(data.error || `请求失败 (${response.status})`);
  return data;
}
function notice(message,error=false) {$('notice').hidden=false;$('notice').textContent=message;$('notice').className=`notice${error?' error':''}`;}
function showLogin() {$('workspace').hidden=true;$('login').hidden=false;document.querySelectorAll('dialog[open]').forEach(d=>d.close());}
function showWorkspace() {$('login').hidden=true;$('workspace').hidden=false;}
function acceptChecks(data, serial) {if(serial>=appliedCheckSerial){checks=data||{results:{},batch:null};appliedCheckSerial=serial;}}
async function load() {const serial=++checkSerial;state=await api('/api/state');acceptChecks(state.checks,serial);render();}
function filteredItems() {const term=$('search').value.trim().toLowerCase();return state[page].filter(item=>[item.name,item.port,item.server,activeNode(item.nodeId)?.name].some(v=>String(v??'').toLowerCase().includes(term)));}
function batchActive() {return ['running','stopping'].includes(checks.batch?.status);}
function checkReady() {return state.coreReady&&state.revision===state.appliedRevision&&!state.lastError;}
function checkTargets() {return page==='nodes'?filteredItems().filter(n=>n.enabled&&n.available).map(n=>n.id):[];}
function delayCell(id) {
  const result=checks.results[id];
  if(!result)return '<span class="muted">未检测</span>';
  const label={queued:'排队中',running:'检测中',timeout:'超时',failed:'失败',cancelled:'已取消',stale:'已过期',skipped:'已跳过'}[result.status]||'未检测';
  const value=result.status==='success'?`<span class="mono latency-value">${escapeHTML(result.delayMs)} ms</span>`:tag(label,['failed','timeout'].includes(result.status)?'error':['running','queued'].includes(result.status)?'pending':'');
  const time=result.checkedAt?new Date(result.checkedAt).toLocaleTimeString('zh-CN',{hour12:false}):'';
  return `<span title="${escapeHTML(result.error||'')}" class="latency-result">${value}</span>${time?`<span class="cell-sub" title="${escapeHTML(formatTime(result.checkedAt))}">${escapeHTML(time)}</span>`:''}`;
}
function renderChecks() {
  const count=checkTargets().length;
  $('check-batch').hidden=page!=='nodes';$('check-batch').textContent=`批量检测 (${count})`;
  $('check-batch').disabled=busy||checkRequestBusy||batchActive()||!checkReady()||count===0;
  $('check-batch').title=!checkReady()?'请先启动内核并成功应用配置':'检测当前筛选结果中的已启用节点';
  const batch=checks.batch;$('check-progress').hidden=page!=='nodes'||!batch;
  if(batch){
    const label={running:'检测中',stopping:'正在停止',completed:'检测完成',stopped:'已停止'}[batch.status]||batch.status;
    $('check-summary').textContent=`${label} · ${batch.completed}/${batch.total} · 成功 ${batch.succeeded} · 失败 ${batch.failed}${batch.skipped?` · 跳过 ${batch.skipped}`:''}${batch.cancelled?` · 取消 ${batch.cancelled}`:''}`;
    $('check-meter').max=Math.max(1,batch.total);$('check-meter').value=batch.completed;
    $('check-stop').hidden=!batchActive();$('check-stop').disabled=checkRequestBusy||batch.status==='stopping';
  }
  document.querySelectorAll('[data-check-result]').forEach(cell=>{cell.innerHTML=delayCell(cell.dataset.checkResult);});
  document.querySelectorAll('[data-action="check"]').forEach(button=>{
    const result=checks.results[button.dataset.id],node=activeNode(button.dataset.id);
    button.disabled=checkRequestBusy||busy||!checkReady()||!node?.enabled||!node?.available||!!result?.inFlight;
    button.textContent=result?.inFlight?(result.status==='queued'?'排队中':'检测中'):'检测';
  });
}
async function requestCheck(path,body) {
  if(checkRequestBusy)return;checkRequestBusy=true;renderChecks();
  const serial=++checkSerial;
  try{acceptChecks(await api(path,'POST',body),serial);renderChecks();}
  catch(error){notice(error.message,true);}
  finally{checkRequestBusy=false;renderChecks();}
}
async function pollChecks() {
  if(checksLoading||checkRequestBusy||document.hidden||$('workspace').hidden)return;
  if(page!=='nodes'&&!batchActive()&&!Object.values(checks.results).some(r=>r.inFlight))return;
  checksLoading=true;const serial=++checkSerial;
  try{acceptChecks(await api('/api/node-checks'),serial);renderChecks();}
  catch(error){notice(error.message,true);}
  finally{checksLoading=false;}
}

function formatTime(value) {return value?new Date(value).toLocaleString('zh-CN',{hour12:false}):'尚未同步';}
function activeNode(id) {return state.nodes.find(n=>n.id===id);}
function nodeSource(node) {return state.subscriptions.find(s=>s.id===node.sourceId)?.name || (node.sourceId?'未知订阅':'手动导入');}
function button(label,action,id,kind='') {return `<button type="button" class="${kind}" data-action="${action}" data-id="${escapeHTML(id)}" ${busy?'disabled':''}>${label}</button>`;}
function tag(text,kind='') {return `<span class="tag ${kind}">${text}</span>`;}
function listenerStatus(l) {
  const node=activeNode(l.nodeId);
  if (!state.coreReady) return tag('内核离线','error');
  if (state.revision!==state.appliedRevision || state.lastError) return tag('待应用','pending');
  if (!l.enabled) return tag('已停用');
  if (!node?.available || !node?.enabled) return tag('节点不可用','error');
  return tag('已生效','ready');
}
function render() {
  const current=pages[page];
  $('page-title').textContent=current.title;$('breadcrumb').textContent=current.title;$('page-kicker').textContent=current.kicker;
  $('page-description').textContent=current.description;$('create').textContent=current.create;$('table-title').textContent=current.table;$('table-note').textContent=current.note;$('page-footnote').textContent=current.foot;
  for (const name of Object.keys(pages)) $('nav-'+name).textContent=state[name].length;
  document.querySelectorAll('[data-page]').forEach(a=>{const selected=a.dataset.page===page;a.classList.toggle('active',selected);if(selected)a.setAttribute('aria-current','page');else a.removeAttribute('aria-current');});
  $('core-dot').className='live-dot'+(state.coreReady?' ready':'');$('core-label').textContent=state.coreReady?`Mihomo ${state.coreVersion}`:'内核未连接';
  const applied=state.coreReady && state.revision===state.appliedRevision && !state.lastError;
  $('apply-dot').className='status-dot '+(applied?'ready':'pending');$('apply-title').textContent=applied?'所有变更已应用':(state.coreReady?'有配置等待应用':'内核未就绪');
  $('apply-detail').textContent=applied?`版本 ${state.appliedRevision} · ${formatTime(state.appliedAt)}`:`已保存版本 ${state.revision} · 上次应用 ${state.appliedRevision<0?'无':state.appliedRevision}`;
  $('apply-error').hidden=!state.lastError;$('apply-error').textContent=state.lastError;
  renderTable();
}
function renderTable() {
  const term=$('search').value.trim().toLowerCase();
  const items=filteredItems();
  $('row-count').textContent=`${items.length} 条记录${term?` / 共 ${state[page].length} 条`:''}`;
  if (!items.length) {const empty=term?'没有匹配的记录':{listeners:'还没有监听端口',nodes:'节点池为空',subscriptions:'还没有订阅'}[page];const hint=term?'试试其他关键词。':{listeners:'先导入节点，再创建你的第一条固定线路。',nodes:'导入节点链接，或添加订阅获取节点。',subscriptions:'添加订阅地址，随后同步节点。'}[page];$('table').innerHTML=`<div class="empty"><strong>${empty}</strong><p>${hint}</p></div>`;renderChecks();return;}
  let headers,rows;
  if (page==='listeners') {
    headers=['监听名称','端口','绑定节点','状态','操作'];
    rows=items.map(l=>{const n=activeNode(l.nodeId);return `<tr><td class="primary-cell">${escapeHTML(l.name)}<span class="cell-sub">0.0.0.0 · mixed</span></td><td><span class="mono port">${l.port}</span></td><td>${escapeHTML(n?.name||'节点不存在')}<span class="cell-sub">${escapeHTML(n?nodeSource(n):'')}</span></td><td>${listenerStatus(l)}</td><td><div class="actions">${button('复制地址','copy',l.id)}${button('编辑','edit-listener',l.id)}${button(l.enabled?'停用':'启用','toggle-listener',l.id)}${button('删除','delete-listener',l.id,'remove')}</div></td></tr>`;});
  } else if (page==='nodes') {
    headers=['节点名称','远程地址','来源','延迟','状态','操作'];
    rows=items.map(n=>`<tr><td class="primary-cell">${escapeHTML(n.name)}<span class="cell-sub">${escapeHTML(n.protocol.toUpperCase())} · ${state.listeners.filter(l=>l.nodeId===n.id).length} 个绑定</span></td><td class="mono">${escapeHTML(n.server)}:${n.port}</td><td>${escapeHTML(nodeSource(n))}</td><td class="latency-cell" data-check-result="${n.id}">${delayCell(n.id)}</td><td>${!n.available?tag('订阅已移除','error'):n.enabled?tag('可绑定','ready'):tag('已停用')}</td><td><div class="actions">${button('检测','check',n.id)}${button(n.enabled?'停用':'启用','toggle-node',n.id)}${button('删除','delete-node',n.id,'remove')}</div></td></tr>`);
  } else {
    headers=['订阅名称','节点数量','上次同步','操作'];
    rows=items.map(s=>`<tr><td class="primary-cell">${escapeHTML(s.name)}<span class="cell-sub">${escapeHTML(new URL(s.url).hostname)}</span></td><td>${state.nodes.filter(n=>n.sourceId===s.id&&n.available).length} 个有效节点</td><td>${escapeHTML(formatTime(s.updatedAt))}</td><td><div class="actions">${button('同步节点','sync',s.id)}${button('编辑','edit-subscription',s.id)}${button('删除','delete-subscription',s.id,'remove')}</div></td></tr>`);
  }
  $('table').innerHTML=`<table><thead><tr>${headers.map(h=>`<th scope="col">${h}</th>`).join('')}</tr></thead><tbody>${rows.join('')}</tbody></table>`;
  renderChecks();
}
function openDialog(id) {const d=$(id);d.querySelector('.dialog-error').textContent='';d.showModal();}
function editListener(id='') {
  const l=state.listeners.find(item=>item.id===id);$('listener-form').reset();$('listener-id').value=id;$('listener-title').textContent=l?'编辑监听':'新增监听';
  $('listener-name').value=l?.name||'';let port=17891;while(state.listeners.some(i=>i.port===port))port++;
  $('listener-port').value=l?.port||port;
  const nodes=state.nodes.filter(n=>(n.available&&n.enabled)||n.id===l?.nodeId);
  $('listener-node').innerHTML='<option value="">选择一个节点</option>'+nodes.map(n=>`<option value="${n.id}">${escapeHTML(n.name)} · ${escapeHTML(nodeSource(n))}${!n.available||!n.enabled?'（不可用）':''}</option>`).join('');
  $('listener-node').value=l?.nodeId||'';$('listener-enabled').checked=l?.enabled??true;openDialog('listener-dialog');
}
function editSubscription(id='') {const s=state.subscriptions.find(item=>item.id===id);$('subscription-form').reset();$('subscription-id').value=id;$('subscription-title').textContent=s?'编辑订阅':'添加订阅';$('subscription-name').value=s?.name||'';$('subscription-url').value=s?.url||'';openDialog('subscription-dialog');}
async function run(task,dialog) {
  if(busy)return;busy=true;document.querySelectorAll('button').forEach(b=>b.disabled=true);
  if(dialog)dialog.querySelector('.dialog-error').textContent='';
  try {await task();} catch(error) {if(dialog?.open)dialog.querySelector('.dialog-error').textContent=error.message;notice(error.message,true);}
  finally {busy=false;document.querySelectorAll('button').forEach(b=>b.disabled=false);renderTable();}
}
async function save(path,method,body,message,dialog) {const data=await api(path,method,body);dialog?.close();await load();notice(data.apply?.applied?message:`已保存，尚未生效：${data.apply?.error||'请重新应用配置'}`,!data.apply?.applied);}
function confirmDelete(description,path) {confirmation=path;$('confirm-description').textContent=description;openDialog('confirm-dialog');}

$('login-form').addEventListener('submit',async e=>{e.preventDefault();const b=e.currentTarget.querySelector('button');b.disabled=true;$('login-error').textContent='';try{await api('/api/login','POST',{key:$('admin-key').value});$('admin-key').value='';showWorkspace();await load();}catch(error){$('login-error').textContent=error.message;}finally{b.disabled=false;}});
$('logout').addEventListener('click',()=>run(async()=>{await api('/api/logout','POST',{});showLogin();}));
$('refresh').addEventListener('click',()=>run(async()=>{await load();notice('状态已更新');}));
$('apply').addEventListener('click',()=>run(async()=>{const data=await api('/api/apply','POST',{});await load();notice(data.applied?'配置已应用':data.error,!data.applied);}));
$('create').addEventListener('click',()=>{if(page==='listeners'){if(!state.nodes.some(n=>n.available&&n.enabled)){notice('请先在节点池导入节点，或添加订阅并同步。',true);return;}editListener();}else if(page==='nodes'){$('import-form').reset();openDialog('import-dialog');}else editSubscription();});
$('search').addEventListener('input',renderTable);
$('check-batch').addEventListener('click',()=>requestCheck('/api/node-checks/batch',{ids:checkTargets()}));
$('check-stop').addEventListener('click',()=>{if(checks.batch)requestCheck('/api/node-checks/batch/'+checks.batch.id+'/stop',{});});
document.querySelectorAll('[data-close]').forEach(b=>b.addEventListener('click',()=>{if(!busy)b.closest('dialog').close();}));
document.querySelectorAll('dialog').forEach(d=>d.addEventListener('cancel',e=>{if(busy)e.preventDefault();}));
$('listener-form').addEventListener('submit',e=>{e.preventDefault();const id=$('listener-id').value;run(()=>save('/api/listeners'+(id?'/'+id:''),id?'PUT':'POST',{name:$('listener-name').value,port:Number($('listener-port').value),nodeId:$('listener-node').value,enabled:$('listener-enabled').checked},'监听已保存并应用',$('listener-dialog')),$('listener-dialog'));});
$('subscription-form').addEventListener('submit',e=>{e.preventDefault();const id=$('subscription-id').value;run(()=>save('/api/subscriptions'+(id?'/'+id:''),id?'PUT':'POST',{name:$('subscription-name').value,url:$('subscription-url').value},'订阅已保存，可点击“同步节点”获取节点',$('subscription-dialog')),$('subscription-dialog'));});
$('import-form').addEventListener('submit',e=>{e.preventDefault();run(()=>save('/api/import','POST',{raw:$('import-raw').value},'节点已导入，可以创建监听了',$('import-dialog')),$('import-dialog'));});
$('confirm-form').addEventListener('submit',e=>{e.preventDefault();run(()=>save(confirmation,'DELETE',undefined,'已删除并应用',$('confirm-dialog')),$('confirm-dialog'));});
$('table').addEventListener('click',e=>{
  const target=e.target.closest('[data-action]');if(!target||busy)return;const {action,id}=target.dataset;
  if(action==='edit-listener'){editListener(id);return;}if(action==='edit-subscription'){editSubscription(id);return;}
  if(action==='delete-listener'){const l=state.listeners.find(i=>i.id===id);confirmDelete(`删除“${l.name}”并关闭端口 ${l.port}？绑定节点会保留。`,'/api/listeners/'+id);return;}
  if(action==='delete-node'){const n=activeNode(id);confirmDelete(`删除节点“${n.name}”？如果仍有监听引用，需要先更换绑定。`,'/api/nodes/'+id);return;}
  if(action==='delete-subscription'){const s=state.subscriptions.find(i=>i.id===id);confirmDelete(`删除订阅“${s.name}”及其节点？存在监听引用时无法删除。`,'/api/subscriptions/'+id);return;}
  if(action==='check'){requestCheck('/api/nodes/'+id+'/check',{});return;}
  run(async()=>{
    if(action==='toggle-listener'){const l=state.listeners.find(i=>i.id===id);await save('/api/listeners/'+id,'PUT',{...l,enabled:!l.enabled},'监听状态已更新');}
    if(action==='toggle-node'){const n=activeNode(id);await save('/api/nodes/'+id,'PATCH',{enabled:!n.enabled},'节点状态已更新');}
    if(action==='sync')await save('/api/subscriptions/'+id+'/sync','POST',{},'订阅节点已同步，监听绑定已保留');
    if(action==='copy'){const l=state.listeners.find(i=>i.id===id);const address=`http://${location.hostname}:${l.port}`;if(navigator.clipboard&&window.isSecureContext){await navigator.clipboard.writeText(address);notice('代理地址已复制：'+address);}else{notice('代理地址：'+address+'（可选中复制）');}}
  });
});
function navigate(){const name=location.hash.slice(1);page=pages[name]?name:'listeners';$('search').value='';render();}
window.addEventListener('hashchange',navigate);
async function start(){navigate();try{const auth=await api('/api/auth');if(auth.authenticated){showWorkspace();await load();}else showLogin();}catch(error){showLogin();$('login-error').textContent=error.message;}}
setInterval(()=>{if(!$('workspace').hidden&&!busy&&!document.hidden)load().catch(error=>notice(error.message,true));},15000);
setInterval(pollChecks,1000);
start();
