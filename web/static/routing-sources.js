let sourcePreviewToken='';
let sourceBindings={};
let categoryEditScope='';
let categoryRuleScope='';
let entriesView={policy:'',scope:'',entries:[],total:0,offset:0};
let entriesSerial=0;

function sourceLabel(source){try{const u=new URL(source.url);return u.host+u.pathname+(u.search?'?…':'');}catch{return '分流订阅';}}
function renderRoutingSources(){
  const term=$('search').value.trim().toLowerCase();
  const sources=(state.routingSources||[]).filter(s=>[s.name,sourceLabel(s)].some(v=>v.toLowerCase().includes(term)));
  $('row-count').textContent=`${sources.length} / ${state.routingSources?.length||0}`;
  const cards=sources.map(source=>{
    const current=state.activeRoutingSource===source.id;
    return `<article class="source-card${current?' current':''}"><div class="source-card-copy"><strong>${escapeHTML(source.name)} ${current?tag('使用中','ready'):''}</strong><p>${escapeHTML(sourceLabel(source))}</p><small>${source.groups} 个分组 · ${source.rules} 条规则 · ${source.providers} 个规则集 · ${source.autoUpdate?`每 ${Math.round(source.interval/3600*100)/100} 小时检查`:'手动更新'}</small><small>检查：${escapeHTML(formatTime(source.checkedAt))} · 内容更新：${escapeHTML(formatTime(source.updatedAt))}</small>${source.lastError?`<p class="error-text">${escapeHTML(source.lastError)} · 保留上次成功内容</p>`:''}</div><div class="actions"><button type="button" data-edit-source="${source.id}">编辑</button><button type="button" data-refresh-source="${source.id}">立即更新</button>${current?'<button type="button" data-activate-source="">停用</button>':`<button type="button" data-activate-source="${source.id}">启用</button><button type="button" class="remove" data-delete-source="${source.id}">删除</button>`}</div></article>`;
  }).join('');
  const deleted=(state.categoryEdits||[]).filter(edit=>edit.deleted);
  return `<div class="source-workspace"><p class="routing-caption">保存多套分流方案，每次启用一套。分类的本地修改、删除记录和节点选择分别保留。</p>${cards||'<div class="empty"><strong>尚未订阅分流方案</strong><p>添加 Mihomo YAML 订阅，预览校验后启用。</p></div>'}${deleted.length?`<details class="advanced"><summary>已删除的分类 (${deleted.length})</summary>${deleted.map(edit=>`<div class="deleted-category"><span>${escapeHTML(edit.label||edit.name)}</span><button type="button" data-restore-category="${escapeHTML(edit.name)}">恢复分类</button></div>`).join('')}</details>`:''}</div>`;
}
function openRoutingSource(id=''){
  const source=state.routingSources?.find(s=>s.id===id);
  $('source-form').reset();$('source-id').value=id;$('source-version').value=source?.version||0;
  $('source-title').textContent=source?'编辑分流订阅':'订阅分流方案';$('source-name').value=source?.name||'';$('source-url').value=source?.url||'';
  $('source-interval').value=(source?.interval||86400)/3600;$('source-auto').checked=source?.autoUpdate??true;$('source-dns').checked=!!source?.importDns;
  sourceBindings={...(source?.bindings||{})};sourcePreviewToken='';$('source-preview').innerHTML='';$('source-save').disabled=true;
  const options=[{id:'',name:'手动导入节点'},...state.subscriptions];
  $('source-node-sources').innerHTML=options.map(s=>`<label class="checkbox compact"><input type="checkbox" data-source-pool="${escapeHTML(s.id)}"${source?.sourceIds?.includes(s.id)?' checked':''}>${escapeHTML(s.name)}</label>`).join('');
  openDialog('source-dialog');
}
function sourceFormData(){return {id:$('source-id').value,version:Number($('source-version').value),name:$('source-name').value,url:$('source-url').value,interval:Math.round(Number($('source-interval').value)*3600),autoUpdate:$('source-auto').checked,sourceIds:[...$('source-node-sources').querySelectorAll('input:checked')].map(i=>i.dataset.sourcePool),bindings:sourceBindings,importDns:$('source-dns').checked};}
function renderSourcePreview(report){
  const errors=(report.errors||[]).map(e=>`<p class="error-text">${escapeHTML(e)}</p>`).join('');
  const options='<option value="">请选择对应节点</option>'+state.nodes.map(n=>`<option value="${n.id}">${escapeHTML(n.name)} · ${escapeHTML(nodeSource(n))}</option>`).join('');
  const unresolved=(report.unresolved||[]).map(name=>`<label class="mapping-row"><span>${escapeHTML(name)}</span><select aria-label="${escapeHTML(name)}" data-source-binding="${escapeHTML(name)}">${options}</select></label>`).join('');
  const mappings=Object.entries(report.mappings||{});
  $('source-preview').innerHTML=errors+unresolved+(report.token?`<div class="preview-success"><strong>校验通过${report.changed?'':' · 规则内容未变化'}</strong><p>${report.source.groups} 个分组 · ${report.source.rules} 条规则 · ${report.source.providers} 个规则集</p><p>${report.groupNames.map(escapeHTML).join('、')}</p></div>`:'')+(mappings.length?`<details><summary>已匹配 ${mappings.length} 个节点</summary>${mappings.map(([name,id])=>`<p class="field-hint">${escapeHTML(name)} → ${escapeHTML(activeNode(id)?.name||id)}</p>`).join('')}</details>`:'')+(report.ignored?.length?`<details><summary>保留管理器设置的字段与忽略项</summary><p class="field-hint">${report.ignored.map(escapeHTML).join('、')}</p></details>`:'');
  sourcePreviewToken=report.token||'';
}
function openCategoryEditor(name=''){
  if(!state.activeRoutingSource){openRoutingSource();return;}
  const group=routingView.proxyGroups?.find(g=>g.name===name);const edit=state.categoryEdits?.find(e=>e.name===name);
  categoryEditScope=state.activeRoutingSource||'';$('group-form').reset();$('group-id').value=name;
  $('group-title').textContent=group?'编辑分流分类':'新增分流分类';$('group-name').value=group?.label||'';$('group-name').readOnly=false;
  $('group-kind').value=group?.kind||'select';$('group-all').checked=group?!!group.allNodes:true;
  editingGroupMembers=[...(group?.configuredMembers||group?.members||[])];
  $('group-inherit-label').hidden=!group?.fromSource;$('group-inherit').checked=!!group?.fromSource&&(!edit||!edit.kind);
  $('group-initial-rules').hidden=!!group;$('group-default').dataset.saved=group?.selected||'';
  renderCategoryEditor();openDialog('group-dialog');
}
function renderCategoryEditor(){
  const inherit=$('group-inherit').checked,all=$('group-all').checked;
  $('group-kind').disabled=inherit;$('group-all').disabled=inherit;$('group-member-field').hidden=inherit;
  const term=$('group-node-search').value.trim().toLowerCase(),name=$('group-id').value;
  let candidates=[{value:'DIRECT',name:'直连',source:'连接方式'},{value:'REJECT',name:'拦截',source:'连接方式'},...(routingView.proxyGroups||[]).filter(g=>g.name!==name).map(g=>({value:g.name,name:g.label||g.name,source:'分流分类'}))];
  if(!all)candidates.push(...state.nodes.map(n=>({value:'node-'+n.id,name:n.name,source:nodeSource(n)})));
  candidates=candidates.filter(c=>[c.name,c.source].some(v=>v.toLowerCase().includes(term)));
  $('group-members').innerHTML=candidates.map(c=>`<label class="group-node-option"><input type="checkbox" data-group-node="${escapeHTML(c.value)}"${editingGroupMembers.includes(c.value)?' checked':''}><span>${escapeHTML(c.name)}<small>${escapeHTML(c.source)}</small></span>${c.value.startsWith('node-')?delayCell(c.value.slice(5)):''}</label>`).join('');
  const members=editingGroupMembers.filter(m=>!all||!m.startsWith('node-'));
  $('group-order').innerHTML=members.map(value=>{const index=editingGroupMembers.indexOf(value);return `<div class="group-order-row"><span>${escapeHTML(outboundName(value))}</span><button type="button" data-group-move="${escapeHTML(value)}" data-direction="-1"${index===0?' disabled':''} aria-label="上移 ${escapeHTML(outboundName(value))}">↑</button><button type="button" data-group-move="${escapeHTML(value)}" data-direction="1"${index===editingGroupMembers.length-1?' disabled':''} aria-label="下移 ${escapeHTML(outboundName(value))}">↓</button><button type="button" data-group-remove="${escapeHTML(value)}" aria-label="移除 ${escapeHTML(outboundName(value))}">×</button></div>`;}).join('')+(all?'<p class="field-hint">已启用节点会自动追加为候选出口。</p>':'');
  const previous=$('group-default').value||$('group-default').dataset.saved;
  let defaults=inherit?(routingView.proxyGroups?.find(g=>g.name===name)?.members||[]):members;
  if(all&&!inherit){const source=state.routingSources?.find(s=>s.id===state.activeRoutingSource);defaults=[...defaults,...state.nodes.filter(n=>n.enabled&&n.available&&(!source?.sourceIds?.length||source.sourceIds.includes(n.sourceId))).map(n=>'node-'+n.id)];}
  $('group-default-field').hidden=$('group-kind').value!=='select';
  $('group-default').innerHTML='<option value="">使用首个可用候选出口</option>'+[...new Set(defaults)].map(value=>`<option value="${escapeHTML(value)}">${escapeHTML(outboundName(value))}</option>`).join('');
  $('group-default').value=defaults.includes(previous)?previous:'';

}
function saveCategoryEditor(event){
  event.preventDefault();const inherit=$('group-inherit').checked;
  const edit={name:$('group-id').value,label:$('group-name').value,kind:inherit?'':$('group-kind').value,members:inherit?[]:editingGroupMembers.filter(m=>!$('group-all').checked||!m.startsWith('node-')),allNodes:!inherit&&$('group-all').checked,deleted:false};
  const rules=edit.name?[]:[...$('group-domains').value.split(/\r?\n/).map(value=>({kind:'DOMAIN-SUFFIX',value})),...$('group-ips').value.split(/\r?\n/).map(value=>({kind:'IP',value}))].filter(r=>r.value.trim()).map((r,i)=>({...r,position:100+i}));
  const selected=$('group-kind').value==='select'?$('group-default').value:'';
  run(()=>save('/api/categories','PUT',{scope:categoryEditScope,edit,rules,selected},'规则组已保存，订阅更新会保留本地设置',$('group-dialog')),$('group-dialog'));
}
async function refreshCategoryEntries(){
  const serial=++entriesSerial,scope=entriesView.scope;
  if((state.activeRoutingSource||'')!==scope){$('category-entries-dialog').querySelector('.dialog-error').textContent='当前方案已切换，请重新打开规则列表';return;}
  const params=new URLSearchParams({policy:entriesView.policy,q:$('category-entry-search').value,offset:String(entriesView.offset)});
  try{const data=await api('/api/routing-entries?'+params);if(serial!==entriesSerial)return;if(data.scope!==scope)throw new Error('当前方案已切换，请重新打开规则列表');entriesView={...entriesView,...data};renderCategoryEntries();}
  catch(error){$('category-entries-dialog').querySelector('.dialog-error').textContent=error.message;}
}
function openCategoryEntries(policy=''){
  entriesView={policy,scope:state.activeRoutingSource||'',entries:[],total:0,offset:0};$('category-entry-search').value='';
  $('category-entries-title').textContent=policy?outboundName(policy)+' · 组内规则':'全部规则条目';$('category-entry-list').innerHTML='<p class="picker-empty">正在读取规则…</p>';
  openDialog('category-entries-dialog');refreshCategoryEntries();
}
function renderCategoryEntries(){
  $('entry-page-status').textContent=entriesView.total?`${entriesView.offset+1}–${Math.min(entriesView.offset+100,entriesView.total)} / ${entriesView.total}`:'没有规则';
  $('entry-previous').disabled=entriesView.offset===0;$('entry-next').disabled=entriesView.offset+100>=entriesView.total;
  $('category-entry-list').innerHTML=entriesView.entries.map((entry,index)=>{
    const editable=['DOMAIN','DOMAIN-SUFFIX','IP-CIDR','IP-CIDR6'].includes(entry.kind);
    const kind={DOMAIN:'域名','DOMAIN-SUFFIX':'域名及子域名','IP-CIDR':'IP 网段','IP-CIDR6':'IPv6 网段','RULE-SET':'远程规则集',MATCH:'兜底'}[entry.kind]||entry.kind;
    return `<article class="category-entry${entry.enabled?'':' is-off'}"><div><strong>${escapeHTML(entry.value||outboundName(entry.policy))}</strong><small>${escapeHTML(kind)} · ${escapeHTML(outboundName(entry.policy))} · ${entry.local?'本地补充':'方案规则'}${entry.enabled?'':' · 已停用'}</small></div><div class="actions">${editable?`<button type="button" data-edit-entry="${index}">编辑</button>`:''}<button type="button" data-toggle-entry="${index}">${entry.enabled?'停用':'恢复'}</button>${entry.local?`<button type="button" data-delete-entry="${index}" class="remove">删除</button>`:''}</div></article>`;
  }).join('')||'<p class="picker-empty">本组暂无匹配规则。可添加域名或 IP，或调整搜索条件。</p>';
}
function openCategoryRule(entry=null){
  categoryRuleScope=entriesView.scope;$('category-rule-form').reset();$('category-rule-id').value=entry?.local?entry.id:'';$('category-rule-replaces').value=entry&&!entry.local?entry.text:entry?.replacesText||'';
  $('category-rule-title').textContent=entry?'编辑组内规则':'新增组内规则';
  $('category-rule-policy').innerHTML=(routingView.policies||[]).map(policy=>`<option value="${escapeHTML(policy)}">${escapeHTML(outboundName(policy))}</option>`).join('');
  $('category-rule-policy').value=entry?.policy||entriesView.policy||routingView.policies?.[0];
  $('category-rule-kind').value=entry?.kind?.startsWith('IP-CIDR')?'IP':entry?.kind||'DOMAIN-SUFFIX';$('category-rule-value').value=entry?.value||'';$('category-rule-enabled').checked=entry?.enabled??true;$('category-rule-position').value=entry?.local?entry.position:100;
  openDialog('category-rule-dialog');
}
async function sourceAction(task,message){
  await run(async()=>{try{const result=await task();await load();if(result?.apply&&!result.apply.applied)throw new Error("已保存，尚未生效："+(result.apply.error||"请重新应用"));notice(message);}catch(error){await load();throw error;}});
}
function initRoutingSourcesUI(){
  $('rule-sources-content').addEventListener('click',event=>{const button=event.target.closest('[data-action="category-rules"]');if(button&&!busy){$('rule-sources-dialog').close();openCategoryEntries(button.dataset.id);}});
  $('refresh-rule-sources').addEventListener('click',()=>run(async()=>{const result=await api('/api/rule-sets/refresh','POST',{});notice(result.failed?.length?`已更新 ${result.refreshed} 个规则来源，${result.failed.join('、')} 更新失败`:`已更新 ${result.refreshed} 个规则来源`,!!result.failed?.length);},$('rule-sources-dialog')));

  $('source-form').addEventListener('input',()=>{sourcePreviewToken='';$('source-save').disabled=true;});
  $('source-preview').addEventListener('change',event=>{const name=event.target.dataset.sourceBinding;if(name){sourceBindings[name]=event.target.value;sourcePreviewToken='';$('source-save').disabled=true;}});
  $('source-preview-button').addEventListener('click',async()=>{
    if(!$('source-form').reportValidity())return;
    await run(async()=>{sourcePreviewToken='';$('source-preview').innerHTML='<p class="field-hint">正在下载并校验分组、节点映射及规则…</p>';renderSourcePreview(await api('/api/routing-sources/preview','POST',sourceFormData()));},$('source-dialog'));
    $('source-save').disabled=!sourcePreviewToken;
  });
  $('source-form').addEventListener('submit',event=>{event.preventDefault();if(!sourcePreviewToken)return;run(async()=>{await api('/api/routing-sources','POST',{token:sourcePreviewToken});$('source-dialog').close();routingTab='sources';await load();notice('分流方案已保存，可在方案列表启用');},$('source-dialog'));});
  $('group-inherit').addEventListener('change',renderCategoryEditor);
  let searchTimer;
  $('category-entry-search').addEventListener('input',()=>{clearTimeout(searchTimer);searchTimer=setTimeout(()=>{entriesView.offset=0;refreshCategoryEntries();},200);});
  $('entry-previous').addEventListener('click',()=>{entriesView.offset=Math.max(0,entriesView.offset-100);refreshCategoryEntries();});$('entry-next').addEventListener('click',()=>{entriesView.offset+=100;refreshCategoryEntries();});
  $('new-category-rule').addEventListener('click',()=>openCategoryRule());
  $('category-rule-form').addEventListener('submit',event=>{
    event.preventDefault();const id=$('category-rule-id').value,rule={policy:$('category-rule-policy').value,kind:$('category-rule-kind').value,value:$('category-rule-value').value,enabled:$('category-rule-enabled').checked,position:Number($('category-rule-position').value),replacesText:$('category-rule-replaces').value};
    run(async()=>{await save('/api/category-rules'+(id?'/'+id:''),id?'PUT':'POST',{scope:categoryRuleScope,rule},'组内规则已保存并应用',$('category-rule-dialog'));await refreshCategoryEntries();},$('category-rule-dialog'));
  });
  $('category-entry-list').addEventListener('click',event=>{
    if(busy)return;const edit=event.target.closest('[data-edit-entry]'),toggle=event.target.closest('[data-toggle-entry]'),remove=event.target.closest('[data-delete-entry]');
    if(edit){openCategoryRule(entriesView.entries[Number(edit.dataset.editEntry)]);return;}
    if(!toggle&&!remove)return;const entry=entriesView.entries[Number(toggle?toggle.dataset.toggleEntry:remove.dataset.deleteEntry)];
    run(async()=>{
      if(remove)await api('/api/category-rules/'+entry.id+'?scope='+encodeURIComponent(entriesView.scope),'DELETE');
      else if(entry.local)await api('/api/category-rules/'+entry.id,'PUT',{scope:entriesView.scope,rule:{...entry,enabled:!entry.enabled}});
      else await api('/api/routing-entries/block','POST',{scope:entriesView.scope,id:entry.id,text:entry.text,block:entry.enabled});
      await load();await refreshCategoryEntries();notice(remove?'本地规则已删除':entry.enabled?'规则已停用，更新后仍保持停用':'规则已恢复');
    },$('category-entries-dialog'));
  });
  $('table').addEventListener('click',event=>{
    if(busy)return;
    const sourceEdit=event.target.closest('[data-edit-source]'),activate=event.target.closest('[data-activate-source]'),refresh=event.target.closest('[data-refresh-source]'),remove=event.target.closest('[data-delete-source]'),restore=event.target.closest('[data-restore-category]');
    if(sourceEdit){openRoutingSource(sourceEdit.dataset.editSource);return;}
    if(activate){sourceAction(()=>api('/api/routing-sources/activate','POST',{id:activate.dataset.activateSource}),'分流方案已切换');return;}
    if(refresh){sourceAction(()=>api('/api/routing-sources/'+refresh.dataset.refreshSource+'/refresh','POST',{}),'检查完成，当前使用最新成功版本');return;}
    if(remove){sourceAction(()=>api('/api/routing-sources/'+remove.dataset.deleteSource,'DELETE'),'分流订阅已删除');return;}
    if(restore){sourceAction(()=>api('/api/categories/restore','POST',{scope:state.activeRoutingSource||'',name:restore.dataset.restoreCategory}),'分类已恢复');return;}
    const action=event.target.closest('[data-action]');if(!action)return;const {id}=action.dataset;
    if(action.dataset.action==='edit-category')openCategoryEditor(id);
    if(action.dataset.action==='category-rules')openCategoryEntries(id);
    if(action.dataset.action==='restore-category')sourceAction(()=>api('/api/categories/restore','POST',{scope:state.activeRoutingSource||'',name:id}),'已恢复方案原始分组设置');
    if(action.dataset.action==='delete-category')sourceAction(()=>api('/api/categories','PUT',{scope:state.activeRoutingSource||'',edit:{name:id,label:outboundName(id),deleted:true}}),'整个规则组已删除，关联规则已移除。可在方案订阅页恢复');
  });
}

function routingPageTools(){
 const settings='<button type="button" class="quiet" data-action="routing-settings">分流设置</button>';
 if(routingTab==='sources'||!state.activeRoutingSource)return settings;
 return '<button type="button" class="quiet" data-action="all-routing-entries">全部规则</button><button type="button" class="quiet" data-action="rule-sources">高级 · 规则来源</button>'+settings;
}
function openRuleSources(){
 $('rule-sources-content').innerHTML=renderRuleSets(state.ruleSets||[]);
 openDialog('rule-sources-dialog');
}
