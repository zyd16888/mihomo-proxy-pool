let routingTab = 'groups';
const expandedProxyGroups = new Set();
let editingGroupMembers = [];

function nodeDelayResult(id) {
  const measured=checks.results[id];
  if(measured)return measured;
  if(!checkReady()||!activeNode(id)?.enabled||!activeNode(id)?.available)return null;
  const history=routingView.proxies?.['node-'+id]?.history;
  const last=history?.[history.length-1];
  return last?{status:last.delay>0?'success':'timeout',delayMs:last.delay,checkedAt:last.time}:null;
}
function outboundName(name) {
  if(name?.startsWith('node-'))return activeNode(name.slice(5))?.name||'节点已移除';
  return {DIRECT:'直连',REJECT:'拦截'}[name]||routingView.proxyGroups?.find(g=>g.name===name)?.label||name||'—';
}
function memberDelay(name) {
  if(name?.startsWith('node-'))return delayCell(name.slice(5));
  const group=routingView.proxyGroups?.find(g=>g.name===name);
  const end=group?.chain?.[group.chain.length-1];
  return end?.startsWith('node-')?delayCell(end.slice(5)):'';
}
function renderListenerPicker() {
  const focus=document.activeElement?.closest('#listener-node-list button');
  const focusAttr=focus?.hasAttribute('data-pick-node')?'data-pick-node':'data-picker-check';
  const focusValue=focus?.getAttribute(focusAttr);
  const chosen=$('listener-node').value;
  const term=$('listener-node-search').value.trim().toLowerCase();
  let nodes=state.nodes.filter(n=>(n.enabled&&n.available)||n.id===chosen);
  nodes=nodes.filter(n=>[n.name,nodeSource(n)].some(v=>v.toLowerCase().includes(term)));
  if($('listener-node-success').checked)nodes=nodes.filter(n=>nodeDelayResult(n.id)?.status==='success'||n.id===chosen);
  if($('listener-node-sort').value==='delay')nodes.sort((a,b)=>{
    const delay=n=>nodeDelayResult(n.id)?.status==='success'?nodeDelayResult(n.id).delayMs:Infinity;
    return delay(a)-delay(b)||a.name.localeCompare(b.name,'zh-CN');
  });
  const selected=activeNode(chosen);
  $('listener-selected').textContent=selected?'已选：'+selected.name:'尚未选择节点';
  setHTML($('listener-node-list'),nodes.map(n=>{
    const result=nodeDelayResult(n.id),unavailable=!n.available||!n.enabled;
    return `<div class="picker-row${chosen===n.id?' selected':''}"><button type="button" class="picker-choice" data-pick-node="${n.id}" aria-pressed="${chosen===n.id}"${unavailable?' disabled':''}><span class="choice-dot" aria-hidden="true"></span><span class="picker-copy"><strong>${escapeHTML(n.name)}</strong><small>${escapeHTML(nodeSource(n))}${unavailable?' · 不可用':''}</small><small>${result?.checkedAt?escapeHTML(formatTime(result.checkedAt)):'尚无检测记录'}</small></span><span class="picker-latency">${delayCell(n.id)}</span></button><button type="button" class="quiet picker-check" data-picker-check="${n.id}"${!checkReady()||result?.inFlight||unavailable?' disabled':''}>检测</button></div>`;
  }).join('')||'<p class="picker-empty">没有符合条件的节点，试试取消筛选。</p>');
  if(focusValue)$('listener-node-list').querySelector(`[${focusAttr}="${CSS.escape(focusValue)}"]`)?.focus({preventScroll:true});
}
function proxyGroupTargets(group,seen=new Set()) {
  if(!group||seen.has(group.name))return [];
  seen.add(group.name);
  return [...new Set(group.members.flatMap(member=>member.startsWith('node-')?[member.slice(5)]:proxyGroupTargets(routingView.proxyGroups?.find(g=>g.name===member),seen)))].filter(id=>{const n=activeNode(id);return n?.enabled&&n?.available;});
}
function renderRouting(items) {
  $('create').textContent=routingTab==='sources'?'＋ 订阅分流方案':state.activeRoutingSource?'＋ 新增分类':'＋ 添加分类';
  const tabBar=`<div class="routing-tabs" role="tablist" aria-label="分流视图"><button id="routing-groups-tab" role="tab" aria-selected="${routingTab==='groups'}" aria-controls="routing-content" data-routing-tab="groups">代理组 <span>${routingView.proxyGroups?.length||0}</span></button><button id="routing-rules-tab" role="tab" aria-selected="${routingTab==='rules'}" aria-controls="routing-content" data-routing-tab="rules">规则集 <span>${state.ruleSets?.length||0}</span></button><button id="routing-sources-tab" role="tab" aria-selected="${routingTab==='sources'}" aria-controls="routing-content" data-routing-tab="sources">方案订阅 <span>${state.routingSources?.length||0}</span></button></div>`;
  let content;
  if(routingTab==='sources'){content=renderRoutingSources();}
  else if(routingTab==='rules') {
    content='<p class="routing-caption">从上到下首次命中即生效。排序值越小越优先；细分类应排在通用国内外规则之前。</p>'+renderRuleSets(items);
  } else {
    const term=$('search').value.trim().toLowerCase();
    const groups=(routingView.proxyGroups||[]).filter(g=>[g.label||g.name,...g.members.map(outboundName),...g.ruleSets].some(v=>v.toLowerCase().includes(term)));
    $('row-count').textContent=`${groups.length} / ${routingView.proxyGroups?.length||0}`;
    content=`<p class="routing-caption">规则命中分类后，使用对应代理组的出口。所有规则监听共享此配置。</p>${routingView.runtimeError?`<p class="error-text">${escapeHTML(routingView.runtimeError)}</p>`:''}<div class="proxy-groups">${groups.map(renderProxyGroup).join('')||'<p class="picker-empty">没有匹配的分组。添加规则分类，或新增代理组。</p>'}</div>`;
  }
  return routingSummary()+tabBar+`<div id="routing-content" role="tabpanel" aria-labelledby="routing-${routingTab}-tab">${content}</div>`;
}
function renderProxyGroup(group) {
  const expanded=expandedProxyGroups.has(group.name);
  const selected=group.live?group.now:group.selected;
  const chain=group.live?group.chain.map(outboundName).join(' → '):'待生效'+(selected?' · '+outboundName(selected):'');
  const kind={select:'手动选择','url-test':'自动选择',fallback:'故障转移','load-balance':'负载均衡'}[group.kind]||group.kind;
  const action=group.kind==='select'?'选择后保存并应用':'由内核自动选择';
  const matches=group.ruleSets.length?group.ruleSets.join('、'):group.customId?'尚未关联规则集，可在“自定义规则”中选择此分组':'基础策略或订阅分组';
  return `<section class="proxy-group${expanded?' expanded':''}">
    <div class="proxy-group-head"><button type="button" class="proxy-group-toggle" data-expand-proxy="${escapeHTML(group.name)}" aria-expanded="${expanded}"><span class="group-chevron" aria-hidden="true">${expanded?'⌄':'›'}</span><span class="proxy-group-heading"><strong>${escapeHTML(group.label||group.name)}</strong><span class="proxy-group-chain">${escapeHTML(chain||'等待内核选择')}</span></span></button><span class="group-kind">${escapeHTML(kind)}</span><span class="group-count">${group.members.length}</span><div class="actions"><button type="button" data-action="check-proxy-group" data-id="${escapeHTML(group.name)}"${busy||batchActive()||checkRequestBusy||!checkReady()||!proxyGroupTargets(group).length?' disabled':''}>检测本组</button>${button('编辑','edit-category',group.name)}${button('规则 '+group.ruleCount,'category-rules',group.name)}</div></div>
    ${group.warning?`<p class="group-warning">${escapeHTML(group.warning)}</p>`:''}
    ${expanded?`<div class="proxy-group-body"><div class="group-description"><span>${escapeHTML(matches)}</span><small>${action}</small></div><div class="proxy-member-grid">${group.members.map(member=>{
      const n=member.startsWith('node-')?activeNode(member.slice(5)):null;
      const result=n?nodeDelayResult(n.id):null;
      return `<div class="proxy-member${selected===member?' selected':''}"><button type="button" class="member-choice" data-select-group="${escapeHTML(group.name)}" data-member="${escapeHTML(member)}" aria-pressed="${selected===member}"${group.kind!=='select'||busy?' disabled':''}><span class="choice-dot" aria-hidden="true"></span><span class="member-copy"><strong>${escapeHTML(outboundName(member))}</strong><small>${escapeHTML(n?nodeSource(n):member==='DIRECT'?'直接连接目标':member==='REJECT'?'阻止连接':'代理组')}</small></span><span class="member-delay"${n?` data-check-result="${n.id}"`:''}>${memberDelay(member)}</span></button>${n?`<button type="button" class="member-check" data-member-check="${n.id}" aria-label="检测 ${escapeHTML(n.name)}"${!checkReady()||result?.inFlight?' disabled':''}>检测</button>`:''}</div>`;
    }).join('')}</div><div class="group-footer">${group.edited?button('恢复原始设置','restore-category',group.name):''}${button('删除整个规则组','delete-category',group.name,'remove')}</div></div>`:''}
  </section>`;
}
function editProxyGroup(id='') {const group=state.proxyGroups?.find(g=>g.id===id);openCategoryEditor(group?.name||'');}
function renderGroupEditor(){renderCategoryEditor();}
function openTemplates() {
  $('template-form').reset();
  $('template-list').innerHTML=(routingView.templates||[]).map(t=>{
    const installed=routingView.proxyGroups?.some(g=>g.name===t.name)||t.sets.some(set=>state.ruleSets?.some(s=>s.name===set.name));
    return `<label class="template-option${installed?' installed':''}"><input type="checkbox" name="template" value="${escapeHTML(t.id)}"${installed?' disabled':''}><span><strong>${escapeHTML(t.name)}${installed?' · 已添加':''}</strong><small>${escapeHTML(t.description)}</small></span><span class="muted">${t.sets.length} 个规则集</span></label>`;
  }).join('');
  openDialog('template-dialog');
}
function initRoutingUI() {
  // Scroll the body inside the rounded shell; headings/actions never leave view.
  document.querySelectorAll('dialog').forEach(dialog=>{
    const shell=dialog.querySelector('form')||dialog;
    let heading=shell.querySelector('.dialog-heading');
    if(!heading){heading=document.createElement('div');heading.className='dialog-heading';const h=shell.querySelector('h2');if(h)heading.append(h);}
    const actions=shell.querySelector('.dialog-actions'),error=shell.querySelector('.dialog-error');
    const body=document.createElement('div');body.className='dialog-body';
    const footer=document.createElement('div');footer.className='dialog-footer';
    for(const node of [...shell.childNodes])if(node!==heading&&node!==actions&&node!==error)body.append(node);
    if(error)footer.append(error);if(actions)footer.append(actions);
    shell.replaceChildren(heading,body,footer);shell.classList.add('dialog-layout');
  });
  ['listener-node-search','listener-node-sort','listener-node-success'].forEach(id=>$(id).addEventListener(id==='listener-node-search'?'input':'change',renderListenerPicker));
  $('listener-node-list').addEventListener('click',event=>{
    const pick=event.target.closest('[data-pick-node]');if(pick&&!busy){$('listener-node').value=pick.dataset.pickNode;renderListenerPicker();}
    const check=event.target.closest('[data-picker-check]');if(check&&!busy)requestCheck('/api/nodes/'+check.dataset.pickerCheck+'/check',{});
  });
  $('group-all').addEventListener('change',renderGroupEditor);
  $('group-node-search').addEventListener('input',renderGroupEditor);
  $('group-members').addEventListener('change',event=>{
    const id=event.target.dataset.groupNode;if(!id)return;
    if(event.target.checked)editingGroupMembers.push(id);else editingGroupMembers=editingGroupMembers.filter(n=>n!==id);
    renderGroupEditor();
  });
  $('group-order').addEventListener('click',event=>{
    const move=event.target.closest('[data-group-move]'),remove=event.target.closest('[data-group-remove]');
    if(move){const index=editingGroupMembers.indexOf(move.dataset.groupMove),next=index+Number(move.dataset.direction);if(next>=0&&next<editingGroupMembers.length)[editingGroupMembers[index],editingGroupMembers[next]]=[editingGroupMembers[next],editingGroupMembers[index]];}
    if(remove)editingGroupMembers=editingGroupMembers.filter(id=>id!==remove.dataset.groupRemove);
    renderGroupEditor();
  });
  $('group-form').addEventListener('submit',saveCategoryEditor);
  $('template-form').addEventListener('submit',event=>{
    event.preventDefault();const ids=[...$('template-list').querySelectorAll('input:checked')].map(input=>input.value);
    if(!ids.length){$('template-dialog').querySelector('.dialog-error').textContent='请至少选择一个分类';return;}
    run(()=>save('/api/rule-templates','POST',{ids},'分类已添加，请在代理组中选择出口',$('template-dialog')),$('template-dialog'));
  });
  $('table').addEventListener('click',event=>{
    if(busy)return;
    const tab=event.target.closest('[data-routing-tab]');if(tab){routingTab=tab.dataset.routingTab;renderTable();return;}
    const expand=event.target.closest('[data-expand-proxy]');if(expand){const name=expand.dataset.expandProxy;if(expandedProxyGroups.has(name))expandedProxyGroups.delete(name);else expandedProxyGroups.add(name);renderTable();return;}
    const choice=event.target.closest('[data-select-group]');if(choice){run(()=>save('/api/proxy-selection','PUT',{name:choice.dataset.selectGroup,member:choice.dataset.member},'分组出口已保存并应用'));return;}
    const check=event.target.closest('[data-member-check]');if(check){requestCheck('/api/nodes/'+check.dataset.memberCheck+'/check',{});return;}
    const action=event.target.closest('[data-action]');if(!action)return;
    if(action.dataset.action==='edit-proxy-group')editProxyGroup(action.dataset.id);
    if(action.dataset.action==='delete-proxy-group'){const group=state.proxyGroups.find(g=>g.id===action.dataset.id);confirmDelete(`删除“${group.name}”？请先解除规则集和兜底策略引用。`,'/api/proxy-groups/'+group.id);}
    if(action.dataset.action==='check-proxy-group'){const group=routingView.proxyGroups.find(g=>g.name===action.dataset.id);const ids=proxyGroupTargets(group);if(ids.length)requestCheck('/api/node-checks/batch',{ids});else notice('本组没有可检测的节点');}
  });
}
