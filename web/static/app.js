const $ = (id) => document.getElementById(id);
const escapeHTML = (value) => String(value ?? '').replace(/[&<>"']/g, (char) => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[char]));
const pages = {
  listeners: {title:'监听管理', create:'＋ 新增监听', search:'搜索名称、端口或节点…'},
  nodes: {title:'节点池', create:'＋ 导入节点', search:'搜索节点、协议或订阅…'},
  subscriptions: {title:'订阅管理', create:'＋ 添加订阅', search:'搜索订阅名称…'},
  routing: {title:'规则分流', create:'＋ 添加分类', search:'搜索分组、节点或规则集…'},
  observe: {title:'观测', create:'', search:'搜索目标、监听或规则…'},
};
let state = {nodes:[],listeners:[],subscriptions:[],ruleSets:[],routing:{}};
let page = 'listeners';
let busy = false;
let confirmation = null;
let checks = {results:{}, batch:null};
let checkRequestBusy = false;
let checksLoading = false;
let checkSerial = 0;
let appliedCheckSerial = 0;
const collapsedGroups = new Set();
let detailNodeId = null;
let routingView = {routing:null, ruleSets:[], policies:[], reports:[], groups:0, rules:0, providers:0};
let observeTab = 'connections';
let logs = {entries:[], nextSeq:0, level:'info', connected:false, error:'', dropped:0};
let connections = {connections:[], downloadTotal:0, uploadTotal:0};
let connectionsError = '';
let observeLoading = false;
let logsPaused = false;
const logWindow = [];
let exits = {nodes:{}, listeners:{}, batch:null, service:''};
let exitBusy = false;


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
async function load() {
  const serial=++checkSerial;state=await api('/api/state');acceptChecks(state.checks,serial);
  // The merge report and policy list are only computed on demand, so they are
  // fetched for the page that shows them.
  if(page==='routing'||page==='listeners')try{await loadRouting();}catch{}
  // Probe results survive a page reload because they live in the service, so
  // a fresh load shows what was already measured.
  try{exits=await api('/api/exit-ip');}catch{}
  render();
}
function pageItems() {
  if(page==='routing')return state.ruleSets||[];
  if(page==='observe')return connections.connections||[];
  return state[page]||[];
}
function searchable(item) {
  if(page==='routing')return [item.name,item.policy,item.url,item.behavior,item.format];
  if(page==='observe')return [item.listener,item.target,item.source,item.rule,item.port,...(item.chains||[])];
  return [item.name,item.port,item.server,item.protocol,page==='nodes'?nodeSource(item):item.url,activeNode(item.nodeId)?.name];
}
function filteredItems() {const term=$('search').value.trim().toLowerCase();return pageItems().filter(item=>searchable(item).some(v=>String(v??'').toLowerCase().includes(term)));}
function batchActive() {return ['running','stopping'].includes(checks.batch?.status);}
function checkReady() {return state.coreReady&&state.revision===state.appliedRevision&&!state.lastError;}
function checkTargets() {return page==='nodes'?filteredItems().filter(n=>n.enabled&&n.available).map(n=>n.id):[];}
function delayCell(id) {
  const result=nodeDelayResult(id);
  if(!result)return '<span class="muted">未检测</span>';
  const label={queued:'排队中',running:'检测中',timeout:'超时',failed:'失败',cancelled:'已取消',stale:'已过期',skipped:'已跳过'}[result.status]||'未检测';
  const value=result.status==='success'?`<span class="mono latency-value">${escapeHTML(result.delayMs)} ms</span>`:tag(label,['failed','timeout'].includes(result.status)?'error':['running','queued'].includes(result.status)?'pending':'');
  const detail=[result.error,result.checkedAt?formatTime(result.checkedAt):''].filter(Boolean).join(' · ');
  return `<span title="${escapeHTML(detail)}" class="latency-result">${value}</span>`;
}
function exitCell(scope,id) {
  const result=(scope==='nodes'?exits.nodes:exits.listeners)[id];
  if(!result)return '<span class="muted">未探测</span>';
  if(result.status==='success'){
    const place=[result.country,result.region,result.city].filter(Boolean).join(' ');
    const detail=[place,result.isp,result.asn,result.checkedAt?formatTime(result.checkedAt):''].filter(Boolean).join(' · ');
    return `<span class="exit-result" title="${escapeHTML(detail)}"><span class="mono exit-ip">${escapeHTML(result.ip)}</span><span class="cell-sub">${escapeHTML(place||result.isp||'')}</span></span>`;
  }
  const label={queued:'排队中',running:'探测中',failed:'失败',cancelled:'已取消'}[result.status]||result.status;
  const kind=result.status==='failed'?'error':['running','queued'].includes(result.status)?'pending':'';
  return `<span class="exit-result" title="${escapeHTML([result.error,result.checkedAt?formatTime(result.checkedAt):''].filter(Boolean).join(' · '))}">${tag(label,kind)}</span>`;
}
function exitBatchActive() {return ['running','stopping'].includes(exits.batch?.status);}
async function requestExit(path,body) {
  if(exitBusy)return;exitBusy=true;
  try{exits=await api(path,'POST',body);renderTable();}
  catch(error){notice(error.message,true);}
  finally{exitBusy=false;renderTable();}
}
async function pollExits() {
  if(document.hidden||$('workspace').hidden||exitBusy)return;
  const pending=exitBatchActive()||Object.values(exits.nodes).some(r=>r.inFlight)||Object.values(exits.listeners).some(r=>r.inFlight);
  if(!pending)return;
  try{exits=await api('/api/exit-ip');renderTable();}catch{}
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
  document.querySelectorAll('[data-action="check-group"]').forEach(button=>{
    const count=groupTargets(button.dataset.id).length;
    button.textContent=`检测本组 (${count})`;
    button.disabled=busy||checkRequestBusy||batchActive()||!checkReady()||count===0;
  });
  updateNodeDetails();
  if($('listener-dialog').open)renderListenerPicker();
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
  if(!['nodes','listeners','routing'].includes(page)&&!batchActive()&&!Object.values(checks.results).some(r=>r.inFlight))return;
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
  if (!state.coreReady) return tag('内核离线','error');
  if (state.revision!==state.appliedRevision || state.lastError) return tag('待应用','pending');
  if (!l.enabled) return tag('已停用');
  if (l.mode==='rule') return state.routing?.enabled?tag('已生效','ready'):tag('分流已关闭','error');
  const node=activeNode(l.nodeId);
  if (!node?.available || !node?.enabled) return tag('节点不可用','error');
  return tag('已生效','ready');
}
function render() {
  const current=pages[page];
  document.body.dataset.page=page;
  $('page-title').textContent=current.title;
  $('create').textContent=current.create;$('create').hidden=!current.create;
  $('search').placeholder=current.search;
  const counts={listeners:state.listeners.length,nodes:state.nodes.length,subscriptions:state.subscriptions.length,routing:(state.ruleSets||[]).filter(s=>s.enabled).length,observe:connections.connections.length};
  for (const name of Object.keys(pages)) $('nav-'+name).textContent=counts[name];
  document.querySelectorAll('[data-page]').forEach(a=>{const selected=a.dataset.page===page;a.classList.toggle('active',selected);if(selected)a.setAttribute('aria-current','page');else a.removeAttribute('aria-current');});
  $('core-dot').className='live-dot'+(state.coreReady?' ready':'');$('core-label').textContent=state.coreReady?`Mihomo ${state.coreVersion}`:'内核未连接';
  // The apply state is a chip, not a bar: the common case is "nothing to do",
  // and that case should not cost a row of the page.
  const applied=state.coreReady && state.revision===state.appliedRevision && !state.lastError;
  const chip=$('apply');
  $('apply-dot').className='status-dot '+(applied?'ready':'pending');
  $('apply-label').textContent=applied?`已应用 v${state.appliedRevision}`:(state.coreReady?'待应用':'内核未就绪');
  chip.classList.toggle('pending',!applied);
  chip.title=applied?`版本 ${state.appliedRevision} · ${formatTime(state.appliedAt)} · 点击重新应用`
    :`已保存版本 ${state.revision} · 上次应用 ${state.appliedRevision<0?'无':state.appliedRevision} · 点击重试`;
  $('apply-error').hidden=!state.lastError;$('apply-error').textContent=state.lastError;
  renderTable();
}
function bytesLabel(value) {
  if(typeof value!=='number'||!Number.isFinite(value)||value<0)return '未提供';
  const units=['B','KiB','MiB','GiB','TiB','PiB'];let size=value,index=0;
  while(size>=1024&&index<units.length-1){size/=1024;index++;}
  return `${size.toLocaleString('zh-CN',{maximumFractionDigits:index?1:0})} ${units[index]}`;
}
function expiryLabel(value) {return typeof value==='number'&&value>0?new Date(value*1000).toLocaleDateString('zh-CN'):'未提供';}
function usageHTML(sub,expanded=false) {
  const u=sub?.usage;
  if(!u)return '<span class="quota-muted">用量未获取 · 刷新订阅后显示</span>';
  if(!u.updatedAt){const message={missing:'订阅未提供流量信息',invalid:'流量信息格式异常',fetch_failed:'用量刷新失败'}[u.status]||'暂无有效用量快照';return `<span class="quota-muted">${message}</span>${u.checkedAt?`<div class="quota-meta">最近检查 ${escapeHTML(formatTime(u.checkedAt))}</div>`:''}`;}

  const positiveTotal=typeof u.totalBytes==='number'&&u.totalBytes>0;
  const used=bytesLabel(u.usedBytes),total=positiveTotal?bytesLabel(u.totalBytes):'总量未提供';
  const remaining=u.remainingBytes==null?'剩余未提供':`剩余 ${bytesLabel(u.remainingBytes)}`;
  const over=u.overageBytes>0?`<strong class="quota-danger">超额 ${bytesLabel(u.overageBytes)}</strong>`:'';
  const expired=u.expire>0&&u.expire*1000<Date.now();
  const warning={missing:'本次未提供用量',invalid:'本次用量格式异常',fetch_failed:'刷新失败'}[u.status];
  const updated=u.updatedAt?`更新于 ${formatTime(u.updatedAt)}`:'暂无有效用量快照';
  const summary=`<span>已用 <b>${used}</b> / ${total}</span><span>${remaining}</span><span class="${expired?'quota-danger':''}">${expired?'已到期':'到期'} ${expiryLabel(u.expire)}</span>${over}`;
  const progress=positiveTotal&&u.usedBytes!=null?`<progress class="quota-meter" max="100" value="${Math.min(100,Math.max(0,u.usedBytes/u.totalBytes*100))}" aria-label="订阅流量使用比例"></progress>`:'';
  const status=warning?`<span class="quota-warning">${warning}${u.updatedAt?'，展示上次数据':''}</span>`:'';
  return `<div class="quota-summary" title="${escapeHTML(updated)}">${summary}</div>${expanded?progress:''}<div class="quota-meta">${status}<span>${escapeHTML(updated)}</span>${expanded?`<span>上传 ${bytesLabel(u.uploadBytes)} · 下载 ${bytesLabel(u.downloadBytes)}</span>`:''}</div>`;
}
function groupTargets(sourceId) {return page==='nodes'?filteredItems().filter(n=>n.sourceId===sourceId&&n.enabled&&n.available).map(n=>n.id):[];}
function nodeCard(n) {
  const bindings=state.listeners.filter(l=>l.nodeId===n.id).length;
  const status=!n.available?tag('已移除','error'):!n.enabled?tag('已停用'):'';
  return `<article class="node-card${!n.enabled||!n.available?' is-disabled':''}">
    <div class="node-card-top"><button class="node-name" type="button" data-action="node-details" data-id="${n.id}" title="${escapeHTML(n.name)}">${escapeHTML(n.name)}</button><div class="card-latency" data-check-result="${n.id}">${delayCell(n.id)}</div></div>
    <div class="node-card-bottom"><div class="node-tags"><span data-exit-node="${n.id}">${exitCell('nodes',n.id)}</span>${tag(escapeHTML(n.protocol.toUpperCase()))}${bindings?tag(`${bindings} 个绑定`):''}${status}</div>
    <div class="node-card-actions">${button('检测','check',n.id)}<details class="node-menu"><summary aria-label="${escapeHTML(n.name)} 更多操作" title="更多操作">⋯</summary><div class="node-menu-items">${button('详情','node-details',n.id)}${button('探测出口 IP','exit-node',n.id)}${button(n.enabled?'停用':'启用','toggle-node',n.id)}${button('删除','delete-node',n.id,'remove')}</div></details></div></div>
  </article>`;
}
function renderNodeGroups(items) {
  if(!items.length&&$('search').value.trim())return '<div class="empty"><strong>没有匹配的节点</strong><p>试试节点名称、协议或订阅名称。</p></div>';
  const sources=new Map(state.subscriptions.map(sub=>[sub.id,sub]));
  const groups=new Map();
  if(items.some(n=>!n.sourceId))groups.set('',[]);
  for(const sub of state.subscriptions)if(!$('search').value.trim()||items.some(n=>n.sourceId===sub.id))groups.set(sub.id,[]);
  for(const node of items){if(!groups.has(node.sourceId))groups.set(node.sourceId,[]);groups.get(node.sourceId).push(node);}
  if(!groups.size)return '<div class="empty"><strong>节点池为空</strong><p>导入节点链接，或添加订阅获取节点。</p></div>';
  return [...groups].map(([id,nodes])=>{
    const sub=sources.get(id),name=sub?.name||(id?'未知来源':'手动节点'),collapsed=collapsedGroups.has(id);
    return `<section class="node-group"><header class="node-group-head"><div class="node-group-info"><button type="button" class="group-toggle" data-action="toggle-group" data-id="${escapeHTML(id)}" aria-expanded="${!collapsed}"><span aria-hidden="true">${collapsed?'▸':'▾'}</span><strong>${escapeHTML(name)}</strong><span class="group-count">${nodes.length}</span></button>${sub?usageHTML(sub):'<span class="quota-muted">独立节点 · 无订阅用量</span>'}</div><div class="group-actions">${sub?button('刷新用量','usage',id):''}${button(`检测本组 (${groupTargets(id).length})`,'check-group',id)}</div></header><div class="node-grid" ${collapsed?'hidden':''}>${nodes.length?nodes.map(nodeCard).join(''):`<div class="group-empty">尚未同步节点 ${sub?button('同步节点','sync',id):''}</div>`}</div></section>`;
  }).join('');
}
function renderSubscriptionCards(items) {
  if(!items.length)return '<div class="empty"><strong>没有订阅记录</strong><p>添加订阅地址，随后同步节点。</p></div>';
  return `<div class="subscription-grid">${items.map(sub=>`<article class="subscription-card"><header><h2>${escapeHTML(sub.name)}</h2><span class="group-count">${state.nodes.filter(n=>n.sourceId===sub.id&&n.available).length} 个节点</span></header>${usageHTML(sub,true)}<div class="quota-meta">节点更新：${escapeHTML(formatTime(sub.updatedAt))}</div><div class="subscription-card-actions">${button('刷新用量','usage',sub.id)}${button('同步节点','sync',sub.id)}${button('编辑','edit-subscription',sub.id)}${button('删除','delete-subscription',sub.id,'remove')}</div></article>`).join('')}</div>`;
}
function updateNodeDetails() {
  if(!detailNodeId||!$('node-details').open)return;
  const node=activeNode(detailNodeId);if(!node){$('node-detail-title').textContent='节点已移除';$('node-detail-body').textContent='请刷新节点列表。';return;}
  $('node-detail-title').textContent=node.name;
  const result=checks.results[node.id];
  const fields=[['协议',node.protocol.toUpperCase()],['来源',nodeSource(node)],['远程地址',`${node.server}:${node.port}`],['状态',!node.available?'订阅已移除':node.enabled?'已启用':'已停用'],['绑定监听',state.listeners.filter(l=>l.nodeId===node.id).map(l=>`${l.name} :${l.port}`).join('、')||'未绑定'],['检测时间',result?.checkedAt?formatTime(result.checkedAt):'未检测']];
  $('node-detail-body').innerHTML=`<dl class="node-detail-fields">${fields.map(([label,value])=>`<dt>${label}</dt><dd>${escapeHTML(value)}</dd>`).join('')}</dl><div class="node-detail-result">${delayCell(node.id)}${result?.error?`<p class="error-text">${escapeHTML(result.error)}</p>`:''}</div>`;
}
function bytesShort(value) {
  if(typeof value!=='number'||!Number.isFinite(value))return '0 B';
  const units=['B','KiB','MiB','GiB','TiB'];let size=value,index=0;
  while(size>=1024&&index<units.length-1){size/=1024;index++;}
  return `${size.toLocaleString('zh-CN',{maximumFractionDigits:index?1:0})} ${units[index]}`;
}
function durationLabel(seconds) {
  if(!Number.isFinite(seconds)||seconds<0)return '—';
  if(seconds<60)return `${seconds} 秒`;
  if(seconds<3600)return `${Math.floor(seconds/60)} 分 ${seconds%60} 秒`;
  return `${Math.floor(seconds/3600)} 时 ${Math.floor(seconds%3600/60)} 分`;
}
// Live views re-render on a timer. Rewriting identical markup would drop text
// selection and swap the node out from under a click, so skip unchanged writes.
function setHTML(element,html) {
  if(element.dataset.rendered===html)return;
  element.dataset.rendered=html;element.innerHTML=html;
}
function renderPageTools() {
  const tools=$('page-tools');
  if(page==='observe'){
    const tab=(id,label,count)=>`<button type="button" class="seg${observeTab===id?' active':''}" data-action="observe-tab" data-id="${id}">${label}${count!==undefined?` (${count})`:''}</button>`;
    const levels=['debug','info','warning','error','silent'].map(l=>`<option value="${l}"${logs.level===l?' selected':''}>${l}</option>`).join('');
    setHTML(tools,`<span class="segmented">${tab('connections','连接',connections.connections.length)}${tab('logs','日志')}</span>`+
      (observeTab==='logs'
        ?`<label class="inline-field"><span class="sr-only">日志级别</span><select id="log-level">${levels}</select></label>`+
         `<button type="button" class="quiet" data-action="toggle-log-pause">${logsPaused?'继续':'暂停'}</button><button type="button" class="quiet" data-action="clear-logs">清空</button>`
        :`<button type="button" class="quiet" data-action="close-connections">关闭全部连接</button>`));
    return;
  }
  if(page==='routing'){
    setHTML(tools,`<button type="button" class="quiet" data-action="new-group">新增代理组</button><button type="button" class="quiet" data-action="new-ruleset">自定义规则</button><button type="button" class="quiet" data-action="routing-settings">分流设置</button><button type="button" class="quiet" data-action="refresh-rulesets">立即更新规则集</button>`);
    return;
  }
  if(page==='nodes'){
    const targets=checkTargets();
    const label=exitBatchActive()?`探测中 ${exits.batch.completed}/${exits.batch.total}`:`探测出口 IP (${targets.length})`;
    setHTML(tools,`<button type="button" class="quiet" data-action="exit-batch"${targets.length&&!exitBatchActive()?'':' disabled'}>${label}</button>`+
      (exitBatchActive()?`<button type="button" class="quiet" data-action="exit-stop">停止</button>`:''));
    return;
  }
  setHTML(tools,'');
}
function routingSummary() {
  const routing=state.routing||{};
  const view=routingView;
  const ruleListeners=state.listeners.filter(l=>l.mode==='rule');
  const activePorts=ruleListeners.filter(l=>l.enabled).map(l=>l.port);
  const status=!routing.enabled?tag('已关闭','error')
    :activePorts.length?tag('已启用','ready'):tag('无规则监听','pending');
  const hint=!routing.enabled
    ?'规则分流已关闭，规则监听端口不会开放。'
    :activePorts.length
      ?`浏览器或下载器使用 http://本机IP:${activePorts.join(' 、 ')} 即可获得国内直连、国外代理和广告拦截。`
      :'尚未创建规则监听。到“监听管理”新增一个出口方式为“规则分流”的端口后生效。';
  const reports=(view.reports||[]).filter(r=>r.groups||r.rules||r.providers||r.droppedGeo||r.droppedRules||r.droppedGroups||r.duplicates);
  // Merge results are folded away: they matter when adding a subscription,
  // not every time the page is opened.
  const reportHTML=reports.length?reports.map(r=>{
    const kept=[r.rules?`规则 ${r.rules}`:'',r.groups?`策略组 ${r.groups}`:'',r.providers?`规则集 ${r.providers}`:''].filter(Boolean).join(' · ')||'无可用内容';
    const dropped=[r.droppedGeo?`GEO 规则 ${r.droppedGeo}`:'',r.droppedRules?`无法解析 ${r.droppedRules}`:'',r.droppedGroups?`空策略组 ${r.droppedGroups}`:'',r.duplicates?`重复 ${r.duplicates}`:''].filter(Boolean).join(' · ');
    const renamed=(r.renamed||[]).length?`<div class="merge-renamed">重命名：${r.renamed.map(escapeHTML).join('、')}</div>`:'';
    return `<div class="merge-report"><strong>${escapeHTML(r.subscription)}</strong><span>合并 ${kept}</span>${dropped?`<span class="quota-warning">丢弃 ${dropped}</span>`:''}${renamed}</div>`;
  }).join(''):'<div class="merge-report muted">订阅没有自带规则，或未开启合并。当前只使用下方规则集。</div>';
  const figures=`<span class="routing-figures">策略组 <b>${view.groups||0}</b> · 规则 <b>${view.rules||0}</b> · 规则集 <b>${view.providers||0}</b> · 兜底 <b>${escapeHTML(routing.defaultPolicy||'—')}</b> · DNS ${routing.dnsEnabled?'已配置':'未配置'}</span>`;
  return `<section class="routing-summary">
    <div class="routing-status">${status}<span>${escapeHTML(hint)}</span>${figures}</div>
    <details class="routing-detail"${reports.length?'':' hidden'}>
      <summary>订阅规则合并 (${reports.length})</summary>
      <div class="merge-reports">${reportHTML}</div>
    </details>
  </section>`;
}
function renderRuleSets(items) {
  const rows=items.map(s=>`<tr${s.enabled?'':' class="is-off"'}>
    <td><span class="mono">${s.position}</span></td>
    <td class="primary-cell"><span class="ruleset-name" title="${escapeHTML(s.url)}">${escapeHTML(s.name)}</span>${s.builtin?tag('内置'):''}</td>
    <td>${escapeHTML(s.policy)}${routingView.policies?.includes(s.policy)?'':'<span class="error-text">出口已失效 · 当前拦截</span>'}</td>
    <td class="muted" title="每 ${Math.round(s.interval/3600)} 小时更新">${escapeHTML(s.behavior)} · ${escapeHTML(s.format)}${s.noResolve?' · no-resolve':''}</td>
    <td>${s.enabled?tag('启用中','ready'):tag('已停用')}</td>
    <td><div class="actions">${button('编辑','edit-ruleset',s.id)}${button(s.enabled?'停用':'启用','toggle-ruleset',s.id)}${button('删除','delete-ruleset',s.id,'remove')}</div></td></tr>`);
  const table=items.length
    ?`<table><thead><tr>${['排序','名称','命中策略','类型','状态','操作'].map(h=>`<th scope="col">${h}</th>`).join('')}</tr></thead><tbody>${rows.join('')}</tbody></table>`
    :'<div class="empty"><strong>没有规则集</strong><p>新增一个规则集，或恢复默认的国内、国外与广告列表。</p></div>';
  return table;
}
function renderConnections(items) {
  if(connectionsError)return `<div class="empty"><strong>${escapeHTML(connectionsError)}</strong><p>内核就绪后会自动恢复。</p></div>`;
  if(!items.length)return '<div class="empty"><strong>当前没有连接</strong><p>通过任一监听端口发起请求后，连接会出现在这里。</p></div>';
  const rows=items.map(c=>`<tr>
    <td class="primary-cell">${escapeHTML(c.target||'—')}<span class="cell-sub">${escapeHTML(c.source)} · ${escapeHTML(c.network||'')} ${escapeHTML(c.kind||'')}</span></td>
    <td>${escapeHTML(c.listener||'—')}<span class="cell-sub">:${escapeHTML(c.port||'')}</span></td>
    <td>${escapeHTML((c.chains||[]).join(' → ')||'—')}</td>
    <td><span class="mono">${escapeHTML(c.rule||'—')}</span></td>
    <td><span class="mono">↑${bytesShort(c.upload)} ↓${bytesShort(c.download)}</span></td>
    <td>${durationLabel(c.elapsedSeconds)}</td>
    <td><div class="actions">${button('关闭','close-connection',c.id,'remove')}</div></td></tr>`);
  return `<div class="observe-totals">累计上行 ${bytesShort(connections.uploadTotal)} · 累计下行 ${bytesShort(connections.downloadTotal)}</div>
    <table><thead><tr>${['目标','入口监听','出口链路','命中规则','流量','时长','操作'].map(h=>`<th scope="col">${h}</th>`).join('')}</tr></thead><tbody>${rows.join('')}</tbody></table>`;
}
function renderLogs() {
  const term=$('search').value.trim().toLowerCase();
  const shown=logWindow.filter(e=>!term||e.payload.toLowerCase().includes(term)||e.level.toLowerCase().includes(term));
  const statusBits=[logs.connected?'已连接内核':'未连接内核',`级别 ${logs.level}`,logsPaused?'已暂停':'实时',`保留 ${logWindow.length} 条`];
  if(logs.dropped)statusBits.push(`已滚动丢弃 ${logs.dropped} 条`);
  const status=`<div class="observe-totals">${statusBits.join(' · ')}${logs.error?`<span class="quota-warning"> · ${escapeHTML(logs.error)}</span>`:''}</div>`;
  if(!shown.length)return status+`<div class="empty"><strong>${term?'没有匹配的日志':'暂无日志'}</strong><p>${term?'换个关键词试试。':'级别为 silent 时不采集；请求经过监听端口后会产生日志。'}</p></div>`;
  return status+`<div class="log-stream">${shown.map(e=>`<div class="log-line log-${escapeHTML(e.level)}"><span class="log-time">${escapeHTML(new Date(e.time).toLocaleTimeString('zh-CN',{hour12:false}))}</span><span class="log-level">${escapeHTML(e.level)}</span><span class="log-payload">${escapeHTML(e.payload)}</span></div>`).join('')}</div>`;
}
function renderTable() {
  const term=$('search').value.trim();const items=filteredItems();
  // The sidebar already carries the total, so the count only earns its place
  // while a filter is hiding something.
  $('row-count').hidden=!term;
  $('row-count').textContent=`${items.length} / ${pageItems().length}`;
  $('table').classList.toggle('card-content',page!=='listeners'&&page!=='routing'&&page!=='observe');
  renderPageTools();
  if(page==='nodes'){setHTML($('table'),renderNodeGroups(items));renderChecks();return;}
  if(page==='subscriptions'){setHTML($('table'),renderSubscriptionCards(items));renderChecks();return;}
  if(page==='routing'){setHTML($('table'),renderRouting(items));renderChecks();return;}
  if(page==='observe'){setHTML($('table'),observeTab==='logs'?renderLogs():renderConnections(items));renderChecks();return;}
  if(!items.length){setHTML($('table'),'<div class="empty"><strong>没有监听记录</strong><p>先导入节点创建固定监听，或直接新增一个规则分流端口。</p></div>');renderChecks();return;}
  const rows=items.map(l=>{
    const n=activeNode(l.nodeId);
    // One column for the outbound: the mode tag and the node name were saying
    // the same thing twice.
    const outbound=l.mode==='rule'
      ? tag('规则分流','ready')
      : `${escapeHTML(n?.name||'节点不存在')}<span class="cell-sub">${escapeHTML(n?nodeSource(n):'')}</span>`;
    return `<tr><td class="primary-cell">${escapeHTML(l.name)}</td><td><span class="mono port">${l.port}</span></td><td>${outbound}</td><td>${l.mode==='rule'?'<a href="#routing">按目标分流</a>':`<span data-check-result="${l.nodeId}">${delayCell(l.nodeId)}</span>`}</td><td data-exit-listener="${l.id}">${exitCell('listeners',l.id)}</td><td>${listenerStatus(l)}</td><td><div class="actions">${button('探测出口','exit-listener',l.id)}${button('复制','copy',l.id)}${button('编辑','edit-listener',l.id)}${button(l.enabled?'停用':'启用','toggle-listener',l.id)}${button('删除','delete-listener',l.id,'remove')}</div></td></tr>`;});
  setHTML($('table'),`<table><thead><tr>${['名称','端口','出口','节点延迟','出口 IP','状态','操作'].map(h=>`<th scope="col">${h}</th>`).join('')}</tr></thead><tbody>${rows.join('')}</tbody></table>`);renderChecks();
}
function openDialog(id) {const d=$(id);d.querySelector('.dialog-error').textContent='';d.showModal();}
function syncListenerMode() {
  const rule=$('listener-mode').value==='rule';
  $('listener-node-field').hidden=rule;
  $('listener-node').required=!rule;
  $('listener-mode-hint').textContent=rule
    ?'该端口按域名和 IP 规则选择出口，适合浏览器和下载器。需要先在“规则分流”中保持启用。'
    :'该端口始终走同一个节点，出口 IP 稳定，适合爬虫和过盾。';
}
function editListener(id='') {
  const l=state.listeners.find(item=>item.id===id);$('listener-form').reset();$('listener-id').value=id;$('listener-title').textContent=l?'编辑监听':'新增监听';
  $('listener-name').value=l?.name||'';let port=17891;while(state.listeners.some(i=>i.port===port))port++;
  $('listener-port').value=l?.port||port;
  const nodes=state.nodes.filter(n=>(n.available&&n.enabled)||n.id===l?.nodeId);
  $('listener-node').value=l?.nodeId||'';$('listener-mode').value=l?.mode||(nodes.length?'node':'rule');
  $('listener-node-search').value='';$('listener-node-success').checked=false;renderListenerPicker();
  $('listener-enabled').checked=l?.enabled??true;syncListenerMode();openDialog('listener-dialog');
}
function editRuleSet(id='') {
  const s=(state.ruleSets||[]).find(item=>item.id===id);$('ruleset-form').reset();$('ruleset-id').value=id;
  $('ruleset-title').textContent=s?'编辑规则集':'新增规则集';
  const policies=routingView.policies?.length?routingView.policies:['🌍 国外代理','🎯 国内直连','🛑 广告拦截','DIRECT','REJECT'];
  $('ruleset-policy').innerHTML=policies.map(p=>`<option value="${escapeHTML(p)}">${escapeHTML(outboundName(p))}</option>`).join('');
  $('ruleset-name').value=s?.name||'';$('ruleset-url').value=s?.url||'';
  $('ruleset-policy').value=s?.policy||policies[0];
  $('ruleset-behavior').value=s?.behavior||'domain';$('ruleset-format').value=s?.format||'mrs';
  let position=s?.position;
  if(position===undefined)position=300;
  $('ruleset-position').value=position;
  $('ruleset-interval').value=s?.interval||86400;
  $('ruleset-noresolve').checked=s?.noResolve??false;$('ruleset-enabled').checked=s?.enabled??true;
  openDialog('ruleset-dialog');
}
function openRoutingSettings() {
  const routing=state.routing||{};
  const policies=routingView.policies?.length?routingView.policies:['🌍 国外代理','🎯 国内直连','DIRECT'];
  $('routing-default').innerHTML=policies.filter(p=>p!=='🐟 漏网之鱼').map(p=>`<option value="${escapeHTML(p)}">${escapeHTML(outboundName(p))}</option>`).join('');
  $('routing-enabled').checked=!!routing.enabled;
  $('routing-default').value=routing.defaultPolicy||policies[0];
  $('routing-subpos').value=routing.subRulePosition??500;
  $('routing-merge').checked=!!routing.mergeSubRules;
  $('routing-geo').checked=!!routing.allowGeoRules;
  $('routing-setproxy').value=routing.ruleSetProxy||'DIRECT';
  $('routing-dns').checked=!!routing.dnsEnabled;
  $('routing-dns-cn').value=(routing.dnsDomestic||[]).join('\n');
  $('routing-dns-out').value=(routing.dnsForeign||[]).join('\n');
  openDialog('routing-dialog');
}
function editSubscription(id='') {const s=state.subscriptions.find(item=>item.id===id);$('subscription-form').reset();$('subscription-id').value=id;$('subscription-title').textContent=s?'编辑订阅':'添加订阅';$('subscription-name').value=s?.name||'';$('subscription-url').value=s?.url||'';openDialog('subscription-dialog');}
async function run(task,dialog) {
  if(busy)return;busy=true;document.querySelectorAll('button').forEach(b=>b.disabled=true);
  if(dialog)dialog.querySelector('.dialog-error').textContent='';
  try {await task();} catch(error) {if(page==='subscriptions'||page==='nodes'){try{await load();}catch{}}if(dialog?.open)dialog.querySelector('.dialog-error').textContent=error.message;notice(error.message,true);}
  finally {busy=false;document.querySelectorAll('button').forEach(b=>b.disabled=false);renderTable();}
}
async function save(path,method,body,message,dialog) {const data=await api(path,method,body);dialog?.close();await load();notice(data.apply?.applied?message:`已保存，尚未生效：${data.apply?.error||'请重新应用配置'}`,!data.apply?.applied);}
function confirmDelete(description,path) {confirmation=path;$('confirm-description').textContent=description;openDialog('confirm-dialog');}

async function loadRouting() {
  routingView=await api('/api/routing');
  if(routingView.routing)state.routing=routingView.routing;
  if(routingView.ruleSets)state.ruleSets=routingView.ruleSets;
}
function appendLogs(entries) {
  for(const entry of entries){
    logWindow.push(entry);
    if(entry.seq>logs.nextSeq)logs.nextSeq=entry.seq;
  }
  // The buffer is bounded server side; bound it here too so a long session
  // does not grow the page without limit.
  if(logWindow.length>1500)logWindow.splice(0,logWindow.length-1500);
}
async function pollObserve() {
  if(page!=='observe'||observeLoading||busy||document.hidden||$('workspace').hidden)return;
  observeLoading=true;
  try{
    if(observeTab==='logs'){
      if(logsPaused)return;
      const data=await api('/api/logs?since='+encodeURIComponent(logs.nextSeq));
      const {entries,...rest}=data;logs={...logs,...rest};
      appendLogs(entries||[]);
    }else{
      connections=await api('/api/connections');connectionsError='';
    }
    renderTable();
  }catch(error){
    if(observeTab==='connections'){connectionsError=error.message;renderTable();}
  }finally{observeLoading=false;}
}
$('login-form').addEventListener('submit',async e=>{e.preventDefault();const b=e.currentTarget.querySelector('button');b.disabled=true;$('login-error').textContent='';try{await api('/api/login','POST',{key:$('admin-key').value});$('admin-key').value='';showWorkspace();await load();}catch(error){$('login-error').textContent=error.message;}finally{b.disabled=false;}});
$('logout').addEventListener('click',()=>run(async()=>{await api('/api/logout','POST',{});showLogin();}));
$('refresh').addEventListener('click',()=>run(async()=>{await load();notice('状态已更新');}));
$('apply').addEventListener('click',()=>run(async()=>{const data=await api('/api/apply','POST',{});await load();notice(data.applied?'配置已应用':data.error,!data.applied);}));
$('create').addEventListener('click',()=>{
  if(page==='listeners'){editListener();return;}
  if(page==='nodes'){$('import-form').reset();openDialog('import-dialog');return;}
  if(page==='routing'){openTemplates();return;}
  if(page==='subscriptions')editSubscription();
});
$('search').addEventListener('input',renderTable);
$('listener-mode').addEventListener('change',syncListenerMode);
$('page-tools').addEventListener('click',event=>{
  const target=event.target.closest('[data-action]');if(!target||busy)return;
  const {action,id}=target.dataset;
  if(action==='new-group'){editProxyGroup();return;}
  if(action==='new-ruleset'){editRuleSet();return;}
  if(action==='observe-tab'){observeTab=id;renderTable();pollObserve();return;}
  if(action==='toggle-log-pause'){logsPaused=!logsPaused;renderTable();return;}
  if(action==='clear-logs'){run(async()=>{const data=await api('/api/logs','DELETE');logWindow.length=0;logs={...logs,...data,entries:undefined};logs.nextSeq=data.nextSeq;renderTable();});return;}
  if(action==='close-connections'){run(async()=>{await api('/api/connections','DELETE');await pollObserve();notice('已请求关闭全部连接');});return;}
  if(action==='exit-batch'){requestExit('/api/exit-ip/nodes',{ids:checkTargets()});return;}
  if(action==='exit-stop'){if(exits.batch)requestExit('/api/exit-ip/batch/'+exits.batch.id+'/stop',{});return;}
  if(action==='routing-settings'){openRoutingSettings();return;}
  if(action==='refresh-rulesets'){run(async()=>{
    const result=await api('/api/rule-sets/refresh','POST',{});
    notice(result.failed?.length?`已更新 ${result.refreshed} 个规则集，${result.failed.join('、')} 更新失败`:`已更新 ${result.refreshed} 个规则集`,!!result.failed?.length);
  });return;}
});
$('page-tools').addEventListener('change',event=>{
  if(event.target.id!=='log-level')return;
  const level=event.target.value;
  run(async()=>{const data=await api('/api/logs/level','POST',{level});logs={...logs,level:data.level,connected:data.connected};renderTable();});
});
$('check-batch').addEventListener('click',()=>requestCheck('/api/node-checks/batch',{ids:checkTargets()}));
$('check-stop').addEventListener('click',()=>{if(checks.batch)requestCheck('/api/node-checks/batch/'+checks.batch.id+'/stop',{});});
document.querySelectorAll('[data-close]').forEach(b=>b.addEventListener('click',()=>{if(!busy)b.closest('dialog').close();}));
document.querySelectorAll('dialog').forEach(d=>d.addEventListener('cancel',e=>{if(busy)e.preventDefault();}));
$('listener-form').addEventListener('submit',e=>{e.preventDefault();const id=$('listener-id').value;const mode=$('listener-mode').value;
  if(mode==='node'&&!$('listener-node').value){$('listener-dialog').querySelector('.dialog-error').textContent='请选择绑定节点';return;}
  run(()=>save('/api/listeners'+(id?'/'+id:''),id?'PUT':'POST',{name:$('listener-name').value,port:Number($('listener-port').value),mode,nodeId:mode==='rule'?'':$('listener-node').value,enabled:$('listener-enabled').checked},'监听已保存并应用',$('listener-dialog')),$('listener-dialog'));});
$('routing-form').addEventListener('submit',e=>{e.preventDefault();
  run(()=>save('/api/routing','PUT',{enabled:$('routing-enabled').checked,defaultPolicy:$('routing-default').value,mergeSubRules:$('routing-merge').checked,
    subRulePosition:Number($('routing-subpos').value),allowGeoRules:$('routing-geo').checked,ruleSetProxy:$('routing-setproxy').value,
    dnsEnabled:$('routing-dns').checked,dnsDomestic:$('routing-dns-cn').value.split('\n').map(v=>v.trim()).filter(Boolean),
    dnsForeign:$('routing-dns-out').value.split('\n').map(v=>v.trim()).filter(Boolean)},'分流设置已保存并应用',$('routing-dialog')),$('routing-dialog'));});
$('ruleset-form').addEventListener('submit',e=>{e.preventDefault();const id=$('ruleset-id').value;
  run(()=>save('/api/rule-sets'+(id?'/'+id:''),id?'PUT':'POST',{name:$('ruleset-name').value,url:$('ruleset-url').value,policy:$('ruleset-policy').value,
    behavior:$('ruleset-behavior').value,format:$('ruleset-format').value,position:Number($('ruleset-position').value),
    interval:Number($('ruleset-interval').value),noResolve:$('ruleset-noresolve').checked,enabled:$('ruleset-enabled').checked},'规则集已保存并应用',$('ruleset-dialog')),$('ruleset-dialog'));});
$('subscription-form').addEventListener('submit',e=>{e.preventDefault();const id=$('subscription-id').value;run(()=>save('/api/subscriptions'+(id?'/'+id:''),id?'PUT':'POST',{name:$('subscription-name').value,url:$('subscription-url').value},'订阅已保存，可点击“同步节点”获取节点',$('subscription-dialog')),$('subscription-dialog'));});
$('import-form').addEventListener('submit',e=>{e.preventDefault();run(()=>save('/api/import','POST',{raw:$('import-raw').value},'节点已导入，可以创建监听了',$('import-dialog')),$('import-dialog'));});
$('confirm-form').addEventListener('submit',e=>{e.preventDefault();run(()=>save(confirmation,'DELETE',undefined,'已删除并应用',$('confirm-dialog')),$('confirm-dialog'));});
$('table').addEventListener('click',e=>{
  const target=e.target.closest('[data-action]');if(!target||busy)return;const {action,id}=target.dataset;
  if(['check-proxy-group','edit-proxy-group','delete-proxy-group'].includes(action))return;
  if(action==='toggle-group'){if(collapsedGroups.has(id))collapsedGroups.delete(id);else collapsedGroups.add(id);renderTable();return;}
  if(action==='check-group'){requestCheck('/api/node-checks/batch',{ids:groupTargets(id)});return;}
  if(action==='node-details'){detailNodeId=id;openDialog('node-details');updateNodeDetails();return;}
  if(action==='edit-listener'){editListener(id);return;}if(action==='edit-subscription'){editSubscription(id);return;}
  if(action==='edit-ruleset'){editRuleSet(id);return;}
  if(action==='delete-ruleset'){const s=state.ruleSets.find(i=>i.id===id);confirmDelete(`删除规则集“${s.name}”？命中该规则集的流量将改由后续规则决定。`,'/api/rule-sets/'+id);return;}
  if(action==='close-connection'){run(async()=>{await api('/api/connections/'+encodeURIComponent(id),'DELETE');await pollObserve();});return;}
  if(action==='delete-listener'){const l=state.listeners.find(i=>i.id===id);confirmDelete(`删除“${l.name}”并关闭端口 ${l.port}？绑定节点会保留。`,'/api/listeners/'+id);return;}
  if(action==='delete-node'){const n=activeNode(id);confirmDelete(`删除节点“${n.name}”？如果仍有监听引用，需要先更换绑定。`,'/api/nodes/'+id);return;}
  if(action==='delete-subscription'){const s=state.subscriptions.find(i=>i.id===id);confirmDelete(`删除订阅“${s.name}”及其节点？存在监听引用时无法删除。`,'/api/subscriptions/'+id);return;}
  if(action==='check'){requestCheck('/api/nodes/'+id+'/check',{});return;}
  if(action==='exit-node'){requestExit('/api/exit-ip/nodes',{ids:[id]});return;}
  if(action==='exit-listener'){requestExit('/api/exit-ip/listeners/'+id,{});return;}
  run(async()=>{
    if(action==='toggle-listener'){const l=state.listeners.find(i=>i.id===id);await save('/api/listeners/'+id,'PUT',{...l,enabled:!l.enabled},'监听状态已更新');}
    if(action==='toggle-node'){const n=activeNode(id);await save('/api/nodes/'+id,'PATCH',{enabled:!n.enabled},'节点状态已更新');}
    if(action==='toggle-ruleset'){const s=state.ruleSets.find(i=>i.id===id);await save('/api/rule-sets/'+id,'PUT',{...s,enabled:!s.enabled},'规则集状态已更新');}
    if(action==='usage'){const result=await api('/api/subscriptions/'+id+'/usage','POST',{});await load();const messages={current:'订阅用量已更新',missing:'订阅未提供流量信息',invalid:'返回的流量信息格式异常',fetch_failed:'用量刷新失败'};notice(messages[result.usage?.status]||'暂无用量信息',result.usage?.status==='invalid');}
    if(action==='sync')await save('/api/subscriptions/'+id+'/sync','POST',{},'订阅节点和用量已刷新，监听绑定已保留');
    if(action==='copy'){const l=state.listeners.find(i=>i.id===id);const address=`http://${location.hostname}:${l.port}`;if(navigator.clipboard&&window.isSecureContext){await navigator.clipboard.writeText(address);notice('代理地址已复制：'+address);}else{notice('代理地址：'+address+'（可选中复制）');}}
  });
});
function navigate(){
  const name=location.hash.slice(1);page=pages[name]?name:'listeners';$('search').value='';render();
  if(page==='observe')pollObserve();
  pollExits();
  if(page==='routing')loadRouting().then(render).catch(error=>notice(error.message,true));
}
window.addEventListener('hashchange',navigate);
document.addEventListener('click',event=>{document.querySelectorAll('.node-menu[open]').forEach(menu=>{if(!menu.contains(event.target))menu.removeAttribute('open');});});
async function start(){navigate();try{const auth=await api('/api/auth');if(auth.authenticated){showWorkspace();await load();}else showLogin();}catch(error){showLogin();$('login-error').textContent=error.message;}}
setInterval(()=>{if(!$('workspace').hidden&&!busy&&!document.hidden)load().catch(error=>notice(error.message,true));},15000);
setInterval(pollChecks,1000);
setInterval(pollObserve,2000);
setInterval(pollExits,1000);
initRoutingUI();
start();
