'use strict';
let deviceData=null,deviceDrafts=new Map(),deviceSaving=[],deviceLoading=false,usagePeriod='today',draftRevision=null;
const policyNames={rule:'跟随规则',direct:'完全直连',force:'强制代理'};
const kindNames={computer:'电脑',phone:'手机',tablet:'平板',speaker:'智能音箱',iot:'智能家居',tv:'电视 / 盒子',unknown:'其他设备'};
const iconPaths={computer:'M3 4h18v12H3z M8 21h8 M12 16v5',phone:'M7 2h10v20H7z M10 5h4 M11 19h2',tablet:'M4 2h16v20H4z M11 19h2',speaker:'M6 3h12v18H6z M10 7h4 M9 14a3 3 0 1 0 6 0a3 3 0 1 0-6 0',iot:'M9 18h6 M10 21h4 M8 14a6 6 0 1 1 8 0l-1 2H9z',tv:'M2 5h20v13H2z M8 22l4-4 4 4',unknown:'M4 4h6v6H4z M14 4h6v6h-6z M4 14h6v6H4z M14 14h6v6h-6z'};
function el(tag,cls,content){const n=document.createElement(tag);if(cls)n.className=cls;if(content!==undefined)n.textContent=content;return n}
function kindIcon(kind){const svg=document.createElementNS('http://www.w3.org/2000/svg','svg');svg.setAttribute('viewBox','0 0 24 24');const p=document.createElementNS(svg.namespaceURI,'path');p.setAttribute('d',iconPaths[kind]||iconPaths.unknown);svg.append(p);return svg}
function setAppearance(){const choice=localStorage.getItem('mihomo-mobile-appearance')||'system';const dark=choice==='dark'||choice==='system'&&matchMedia('(prefers-color-scheme: dark)').matches;document.documentElement.dataset.theme=dark?'dark':'light';$('appearance').value=choice;document.querySelector('meta[name="theme-color"]').content=dark?'#111b29':'#eef3f8';$('quickAppearance').setAttribute('aria-label',dark?'切换浅色外观':'切换深色外观')}
setAppearance();matchMedia('(prefers-color-scheme: dark)').addEventListener('change',setAppearance);$('appearance').onchange=()=>{localStorage.setItem('mihomo-mobile-appearance',$('appearance').value);setAppearance()};
$('quickAppearance').onclick=()=>{localStorage.setItem('mihomo-mobile-appearance',document.documentElement.dataset.theme==='dark'?'light':'dark');setAppearance()};
if((navigator.deviceMemory&&navigator.deviceMemory<=4)||(navigator.hardwareConcurrency&&navigator.hardwareConcurrency<=4)||matchMedia('(prefers-reduced-motion: reduce)').matches)document.documentElement.classList.add('reduced-effects');
function bytes(value,decimals=1){if(!Number.isFinite(value))return '—';const units=['B','KB','MB','GB','TB'];let n=Math.max(0,value),i=0;while(n>=1024&&i<units.length-1){n/=1024;i++}return n.toLocaleString('zh-CN',{maximumFractionDigits:i?decimals:0})+' '+units[i]}
function pairTotal(p){return (p?.download||0)+(p?.upload||0)}
function periodPair(v,proxy=true){return v.usage?.[proxy?{today:'proxyToday',month:'proxyMonth',total:'proxy'}[usagePeriod]:usagePeriod]}
async function loadDevices(force=false){
 if(!token||deviceLoading)return;deviceLoading=true;
 try{const data=await api('devices');deviceData=data;
  for(const [key,id]of [['online','Online'],['rule','Rule'],['direct','Direct'],['force','Force']])text('count'+id,data.counts[key]);
  if(force||!$('deviceList').children.length)renderDevices();else patchDeviceMetrics();
  deviceNotice('deviceSwitchNote',data.error||(!data.ready?'设备管理正在准备，请稍候。':''));
  const age=data.statistics?.updated?Date.now()-Date.parse(data.statistics.updated):Infinity;
  deviceNotice('statisticsNote',data.statistics?.error||(age>20000||!data.statistics?.proxyAvailable?'代理统计暂未更新，已保留上次记录':''));
  text('statisticsStarted',data.statistics?.started?'开始记录：'+new Date(data.statistics.started).toLocaleString('zh-CN')+'。升级前的用量不计入；每 5 分钟保存。':'统计正在准备…');
 }catch(e){deviceNotice('deviceSwitchNote',e.message)}finally{deviceLoading=false}
}
function deviceNotice(id,message){text(id,message);$(id).classList.toggle('hidden',!message)}
function syncDeviceActions(){const dirty=deviceDrafts.size>0;$('deviceActions').classList.toggle('hidden',!dirty);$('saveAllDevices').disabled=busy||!deviceData?.ready||!dirty;for(const card of $('deviceList').querySelectorAll('[data-mac]')){const edited=deviceDrafts.has(card.dataset.mac),footer=card.querySelector('.device-card-foot'),save=card.querySelector('.save-one');footer.classList.toggle('hidden',!edited);save.disabled=busy||!deviceData?.ready}}
function rememberDraft(v,field,value){if(!deviceDrafts.size)draftRevision=deviceData.revision;const current=deviceDrafts.get(v.mac)||{mac:v.mac,policy:v.policy,note:v.note||'',kind:v.kind};current[field]=value;deviceDrafts.set(v.mac,current);deviceNotice('deviceDirty',deviceDrafts.size+' 台设备有未保存的修改');syncDeviceActions()}
function sortedDevices(){
 const q=$('deviceSearch').value.trim().toLowerCase(),rank={rule:0,force:1,direct:2},sort=$('deviceSort').value,fresh=Date.now()-Date.parse(deviceData.statistics?.updated||0)<20000;
 const view=typeof v4!=='undefined'?v4.deviceView:'current';
 const records=deviceData.devices.filter(v=>(view==='all'||view==='archived'?view==='all'||v.archived:!v.archived)&&(!$('onlineOnly').checked||v.online)&&[v.name,v.hostname,v.note,v.ip,v.lastIP,v.mac].join(' ').toLowerCase().includes(q));
 return records.sort((a,b)=>{let d=0;if(sort==='policy')d=(rank[a.policy]-rank[b.policy])||Number(!!b.pinned)-Number(!!a.pinned);if(sort==='traffic')d=pairTotal(periodPair(b))-pairTotal(periodPair(a));if(sort==='speed'){const speed=v=>fresh&&v.usage?.proxySpeedAvailable?pairTotal(v.usage.proxySpeed):0;d=speed(b)-speed(a)}if(sort==='ip'){const ip=n=>(n.ip||n.lastIP||'0.0.0.0').split('.').reduce((sum,s)=>sum*256+Number(s),0);d=ip(a)-ip(b)}
  return d||Number(b.online)-Number(a.online)||(a.note||a.name).localeCompare(b.note||b.name,'zh-CN')||a.mac.localeCompare(b.mac)});
}
function renderDevices(){
 if(!deviceData)return;
 const opened=new Set([...$('deviceList').querySelectorAll('.device-edit[open]')].map(d=>d.closest('[data-mac]').dataset.mac));
 const list=$('deviceList');const wasOfflineOpen=!!list.querySelector('.v4-device-offline[open]');list.replaceChildren();const records=sortedDevices();
 let offlineGroup=null;const foldOffline=typeof v4!=='undefined'&&v4.deviceView==='current'&&!$('deviceSearch').value.trim()&&!$('onlineOnly').checked;const offline=records.filter(v=>!v.online&&!v.pinned&&!deviceDrafts.has(v.mac));if(foldOffline&&offline.length){offlineGroup=el('details','v4-device-offline');offlineGroup.open=wasOfflineOpen;offlineGroup.append(el('summary','','离线设备 · '+offline.length+' 台'))}
 if($('deviceViewCount'))text('deviceViewCount',records.length+' 台');
 for(const v of records){const draft=deviceDrafts.get(v.mac)||v,card=el('article','device-card');card.dataset.mac=v.mac;
  const heading=el('div','device-heading'),icon=el('span','device-kind');icon.append(kindIcon(draft.kind));const title=el('div','device-title'),deviceName=el('strong','',draft.note||v.name);if(v.pinned)deviceName.append(el('span','v4-pin','★'));title.append(deviceName,el('small','',draft.note?v.name:(v.hostname&&v.hostname!==v.name?v.hostname:kindNames[draft.kind])));heading.append(icon,title,el('span','presence '+(v.online?'online':''),v.online?'在线':v.presence==='unknown'?'未确认':'离线'));
  const address=el('div','device-address');address.append(el('span','device-ip',v.ip||('上次 '+(v.lastIP||'未分配 IP'))),el('code','',v.mac));
  const metrics=el('div','device-metrics'),usage=el('div','usage-total');usage.append(el('small','usage-label'),el('strong','usage-value'),el('small','usage-detail'));const speeds=el('div','device-speeds');speeds.append(el('small','speed-label','代理网速'),el('span','speed-down'),el('span','speed-up'));metrics.append(usage,speeds);
  const policyLabel=el('label','device-policy');policyLabel.append(el('span','','代理策略'));const select=el('select');select.setAttribute('aria-label',(draft.note||v.name)+'的代理策略');select.dataset.field='policy';for(const [value,label]of Object.entries(policyNames)){const option=el('option','',label);option.value=value;select.append(option)}select.value=draft.policy;select.disabled=busy;select.onchange=()=>rememberDraft(v,'policy',select.value);policyLabel.append(select);
  const details=el('details','device-edit');details.open=opened.has(v.mac);details.append(el('summary','','备注与设备详情'));const noteLabel=el('label','','自定义备注'),note=el('input');note.setAttribute('aria-label',(draft.note||v.name)+'的备注');note.placeholder='例如 我的电脑、客厅电视';note.value=draft.note||'';note.maxLength=48;note.dataset.field='note';note.oninput=()=>rememberDraft(v,'note',note.value);noteLabel.append(note);
  const kindLabel=el('label','','设备类型'),kinds=el('select');for(const [value,label]of Object.entries(kindNames)){const o=el('option','',label);o.value=value;kinds.append(o)}kinds.value=draft.kind;kinds.onchange=()=>rememberDraft(v,'kind',kinds.value);kindLabel.append(kinds);details.append(noteLabel,kindLabel,el('p','total-network-reference'),el('p','subtle','设备信息来源：'+(v.sources||[]).join('、')));
  if(v.randomMAC){const info=el('details','random-mac');info.append(el('summary','','可能使用随机 MAC'),el('p','','固定的私有 MAC 可以保持策略。更换 MAC 后会视为新设备；如果每次连接都变，可在该 Wi-Fi 设置中使用固定 MAC。'));details.append(info)}
  const footer=el('div','device-card-foot');const save=el('button','save-one','保存这台设备');save.disabled=busy||!deviceData.ready;save.onclick=()=>saveDevices([v.mac]);footer.append(save);card.append(heading,address,metrics,policyLabel);
  if(v.policy==='direct')card.append(el('p','v4-device-direct','完全直连 · 当前连接不经过 Mihomo'));
  if(typeof openConnections==='function'){const toolbar=el('div','v4-device-toolbar');for(const [label,fn]of [['实时连接',()=>openConnections(v.mac)],[v.pinned?'取消置顶':'置顶',()=>setDeviceMetadata(v,{pinned:!v.pinned})],[v.archived?'移出归档':'归档',()=>setDeviceMetadata(v,{archived:!v.archived})]]){const b=el('button','',label);b.type='button';b.disabled=busy;b.onclick=fn;toolbar.append(b)}card.append(toolbar)}card.append(details,footer);if(offlineGroup&&!v.online&&!v.pinned&&!deviceDrafts.has(v.mac))offlineGroup.append(card);else list.append(card);
 }
 if(offlineGroup)list.append(offlineGroup);if(!records.length)list.append(el('p','empty',typeof v4!=='undefined'&&v4.deviceView==='archived'?'还没有归档设备':'没有找到匹配的设备'));
 patchDeviceMetrics();deviceNotice('deviceDirty',deviceDrafts.size?deviceDrafts.size+' 台设备有未保存的修改':'');syncDeviceActions();$('resetDevices').disabled=busy||!deviceData.ready;
}
function patchDeviceMetrics(){
 if(!deviceData)return;const views=new Map(deviceData.devices.map(v=>[v.mac,v])),fresh=Date.now()-Date.parse(deviceData.statistics?.updated||0)<20000;
 for(const card of $('deviceList').querySelectorAll('[data-mac]')){const v=views.get(card.dataset.mac);if(!v)continue;const p=periodPair(v),known=!!p&&(v.usage?.proxyAvailable||pairTotal(v.usage?.proxy)>0);card.querySelector('.usage-label').textContent={today:'今日代理用量',month:'本月代理用量',total:'累计代理用量'}[usagePeriod];card.querySelector('.usage-value').textContent=known?'≈ '+bytes(pairTotal(p)):'暂不可用';card.querySelector('.usage-detail').textContent=known?'下载 '+bytes(p.download)+' · 上传 '+bytes(p.upload):'等待代理提供连接数据';
  const speed=v.usage?.proxySpeed,speedOK=fresh&&v.usage?.proxySpeedAvailable&&speed;card.querySelector('.speed-down').textContent='↓ '+(speedOK?bytes(speed.download)+'/s':'—');card.querySelector('.speed-up').textContent='↑ '+(speedOK?bytes(speed.upload)+'/s':'—');
  const native=periodPair(v,false);card.querySelector('.total-network-reference').textContent='总网络用量（含直连，仅供参考）：'+(v.usage?.available?bytes(pairTotal(native)):'暂不可用');
  const presence=card.querySelector('.presence');presence.textContent=v.online?'在线':v.presence==='unknown'?'未确认':'离线';presence.classList.toggle('online',v.online);card.querySelector('.device-ip').textContent=v.ip||('上次 '+(v.lastIP||'未分配 IP'));
 }
}
function saveDevices(macs){if(!deviceData||busy)return;const edits=macs.map(mac=>{const v=deviceData.devices.find(d=>d.mac===mac);return deviceDrafts.get(mac)||{mac:v.mac,policy:v.policy,note:v.note||'',kind:v.kind}});deviceSaving=macs;act('devices',{devices:edits,revision:draftRevision??deviceData.revision})}
function deviceJobFinished(job){if(['devices','device-defaults'].includes(job.action)&&!job.error){if(job.action==='device-defaults')deviceDrafts.clear();else for(const mac of deviceSaving)deviceDrafts.delete(mac);deviceSaving=[];if(!deviceDrafts.size)draftRevision=null}if(['devices','device-defaults','on','off','repair','restart','subscription','restore'].includes(job.action))loadDevices(true)}
$('deviceSearch').oninput=renderDevices;$('deviceSort').onchange=renderDevices;$('onlineOnly').onchange=renderDevices;$('refreshDevices').onclick=()=>loadDevices(true);$('saveAllDevices').onclick=()=>saveDevices([...deviceDrafts.keys()]);
document.querySelectorAll('[data-period]').forEach(b=>b.onclick=()=>{usagePeriod=b.dataset.period;document.querySelectorAll('[data-period]').forEach(n=>{n.classList.toggle('active',n===b);n.setAttribute('aria-pressed',String(n===b))});patchDeviceMetrics();if($('deviceSort').value==='traffic')renderDevices()});
$('resetDevices').onclick=()=>openDialog('恢复设备默认策略','电脑、智能家居、音箱和电视改为完全直连；手机、平板跟随规则。备注会保留。','恢复默认',()=>act('device-defaults',{revision:deviceData.revision}));
$('deviceHelp').onclick=()=>openDialog('策略与代理用量说明','大数字、网速和用量排序只统计实际走代理节点的连接。当前规则下，TikTok、X／推特等走节点，抖音、B站、Steam 等直连不计入。是否计入由实际出口决定，不是只按 App 名称猜测。统计每 5 秒采样，会漏掉短连接及结束前的少量数据，也不包含机场倍率，不是精确账单。总网络用量（含直连）只放在展开详情里。跟随规则按国内／国外分流；完全直连绕过 TCP 和 DNS 接管；强制代理将公网 TCP 和 DNS 交给 Mihomo，但出口仍遵循规则。总开关关闭时暂停接管，业务 UDP 不做代理。');
setInterval(()=>{if(token&&page==='devices'&&!document.hidden)loadDevices()},5000);if(token)loadDevices();
