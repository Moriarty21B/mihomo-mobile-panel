'use strict';
const $=id=>document.getElementById(id);
let token=localStorage.getItem('mihomo-mobile-token')||'', state=null, page='home', busy=false, currentJob='', toastTimer, pollTimer, delays=new Map(), hasSavedSubscription=false;
const titles={on:'正在开启全屋代理',off:'正在关闭全屋代理',repair:'正在修复网络规则',select:'正在切换节点',delay:'正在测量节点延迟',auto:'正在恢复自动选择',restart:'正在重启代理服务',test:'正在检查国内和国外网络',subscription:'正在下载、检查并更新订阅',restore:'正在恢复上一版订阅',devices:'正在保存设备策略','device-defaults':'正在恢复设备默认策略',quota:'正在刷新套餐流量','clean-logs':'正在检查和清理日志'};
function text(id,value){$(id).textContent=value}
function toast(message,error=false){clearTimeout(toastTimer);text('toast',message);$('toast').classList.toggle('error',error);$('toast').classList.remove('hidden');toastTimer=setTimeout(()=>$('toast').classList.add('hidden'),error?9000:4500)}
async function api(path,options={}){
 const response=await fetch('/api/'+path,{...options,headers:{'Content-Type':'application/json','Authorization':'Bearer '+token,...options.headers},cache:'no-store'});
 let result;try{result=await response.json()}catch{throw Error('管理页面暂时无法连接，请确认手机连着家里的 Wi-Fi')}
 if(!response.ok){if(response.status===401&&path!=='login')showLogin();throw Error(result.error||'操作没有完成，请稍后再试')}
 return result;
}
function showLogin(){token='';localStorage.removeItem('mihomo-mobile-token');$('app').classList.add('hidden');$('login').classList.remove('hidden');$('busy').classList.add('hidden');clearTimeout(pollTimer)}
function showApp(){$('login').classList.add('hidden');$('app').classList.remove('hidden');navigate(location.hash.slice(1)||'home');refresh();resumeJob()}
function setBusy(value,action=''){busy=value;$('busy').classList.toggle('hidden',!value);text('busyTitle',titles[action]||'正在处理');document.querySelectorAll('main button').forEach(b=>b.disabled=value);if(!value&&state){$('powerButton').disabled=!state.online&&state.rules.total===0;$('restoreButton').disabled=!state.hasBackup}if(!value&&typeof deviceDrafts!=='undefined'){$('saveAllDevices').disabled=!deviceData?.ready||!deviceDrafts.size;$('resetDevices').disabled=!deviceData?.ready}}
async function refresh(){
 if(!token)return;
 try{state=await api('status');text('connection','已连接');$('connection').classList.add('connected');renderState()}
 catch(e){text('connection','连接中断');$('connection').classList.remove('connected');text('warning','暂时连不上路由器，请确认手机连着家里的 Wi-Fi。');$('warning').classList.remove('hidden');$('powerButton').disabled=true}
}
function renderState(){
 const r=state.rules,on=r.enabled&&state.online,partial=r.partial;
 $('powerCard').classList.toggle('off',!on);text('powerTag',on?'运行正常':partial?'需要修复':r.total&&!state.online?'服务未就绪':'普通直连');
 text('powerTitle','全屋代理');
 text('powerSubtitle',on?'已开启':partial?'规则待修复':r.total&&!state.online?'服务未就绪':'已关闭');
 $('powerButton').setAttribute('aria-checked',String(on));$('powerButton').setAttribute('aria-label',r.total?'关闭代理':'开启代理');
 text('selectedNode',state.selected||'暂时无法读取');text('actualNode',state.selected==='自动选择'?'当前节点 · '+(state.actual||'选择中'):'点这里，更换节点');
 let warning=state.error||state.rulesError||(state.maintenance?.storage?.low?'代理存储空间不足，请在更多里检查日志与存储。':partial?'发现不完整或重复的规则，可在“更多”里点“修复网络规则”。':'');
 text('warning',warning);$('warning').classList.toggle('hidden',!warning);$('autoCheck').classList.toggle('selected',state.selected==='自动选择');
 $('restoreButton').disabled=busy||!state.hasBackup;if(!busy)$('powerButton').disabled=!state.online&&r.total===0;
 renderNodes();if(typeof loadDevices==='function')loadDevices();if(typeof renderStatistics==='function')renderStatistics();
}
function renderNodes(){
 if(!state)return;const q=$('search').value.trim().toLocaleLowerCase();let nodes=(state.nodes||[]).filter(n=>n.name!=='自动选择'&&n.name.toLocaleLowerCase().includes(q));
 if(delays.size)nodes=nodes.slice().sort((a,b)=>((delays.get(a.name)||1e9)-(delays.get(b.name)||1e9)));
 text('nodeCount',q?'找到 '+nodes.length+' 个结果':'可选节点 · '+nodes.length);text('sortHint',delays.size?'按延迟由低到高':'点击即可切换');const list=$('nodeList');list.replaceChildren();
 if(!nodes.length){const div=document.createElement('div');div.className='empty';div.textContent=q?'没有找到，试试其他关键词':'暂无节点信息，请确认代理服务正在运行';list.append(div);return}
 for(const n of nodes){const button=document.createElement('button');button.className='node-option'+(n.name===state.selected?' active':'');button.disabled=busy;button.setAttribute('aria-label','选择 '+n.name);
 const name=document.createElement('span');name.className='name';name.textContent=n.name;
 const delay=document.createElement('span');const ms=delays.has(n.name)?delays.get(n.name):n.delay;delay.className='delay'+(ms>0&&ms<250?' fast':'');delay.textContent=ms>0?ms+' ms':delays.has(n.name)?'未连通':'未测速';
 const radio=document.createElement('span');radio.className='radio'+(n.name===state.selected?' selected':'');button.append(name,delay,radio);button.addEventListener('click',()=>act('select',{name:n.name}));list.append(button)}
}
function navigate(next){page=['home','nodes','devices','subscription','more','rules','traffic','connections','safety'].includes(next)?next:'home';document.querySelectorAll('.page').forEach(p=>p.classList.toggle('hidden',p.id!=='page-'+page));document.querySelectorAll('nav button').forEach(b=>b.classList.toggle('active',b.dataset.page===(['rules','safety'].includes(page)?'more':['traffic','connections'].includes(page)?'devices':page)));if(location.hash!=='#'+page)history.replaceState(null,'','#'+page);window.scrollTo({top:0});requestAnimationFrame(()=>window.scrollTo({top:0}));if(page==='subscription')loadSubscription();if(page==='devices'&&typeof loadDevices==='function')loadDevices();if(typeof onFeatureNavigate==='function')onFeatureNavigate(page)}
async function loadSubscription(){if(typeof loadProfileCards==='function')return loadProfileCards()}
async function act(action,extra={}){
 if(busy)return;setBusy(true,action);
 try{const job=await api('action',{method:'POST',body:JSON.stringify({action,...extra})});currentJob=job.id;localStorage.setItem('mihomo-mobile-job',currentJob);pollJob()}
 catch(e){setBusy(false);toast(e.message,true)}
}
async function resumeJob(){try{const job=await api('job');if(job.running){currentJob=job.id;setBusy(true,job.action);pollJob()}else if(job.id&&job.id===localStorage.getItem('mihomo-mobile-job')){finishJob(job)}}catch{}}
async function pollJob(){clearTimeout(pollTimer);try{const job=await api('job');if(job.id!==currentJob){setBusy(false);toast('操作状态已变化，正在刷新页面');refresh();return}if(job.running){setBusy(true,job.action);pollTimer=setTimeout(pollJob,1300)}else finishJob(job)}catch(e){if(!token)return;text('busyTitle','连接暂时中断，正在重新检查');pollTimer=setTimeout(pollJob,3500)}}
async function finishJob(job){localStorage.removeItem('mihomo-mobile-job');setBusy(false);if(job.error){openDialog('这次操作没有完成',job.message)}else{toast(job.message);if(job.action==='test'&&job.result){for(const [key,id]of [['domestic','domestic'],['foreign','foreign']]){const r=job.result[key];text(id+'Result',r.label+(r.ok?' · '+r.ms+' ms':''));$(id+'Dot').className=r.ok?'good':'bad'}}if((job.action==='delay'||job.action==='auto')&&Array.isArray(job.result)){delays=new Map(job.result.map(r=>[r.name,r.delay]));navigate('nodes')}if(job.action==='subscription'||job.action==='restore'){delays.clear();loadSubscription()}}
 if(typeof deviceJobFinished==='function')deviceJobFinished(job);if(typeof featureJobFinished==='function')await featureJobFinished(job);await refresh();
}
function openDialog(title,message,confirmLabel=null,onConfirm=null){text('dialogTitle',title);const body=$('dialogBody');body.replaceChildren();const p=document.createElement('p');p.textContent=message;body.append(p);$('dialogActions').replaceChildren();if(confirmLabel){const cancel=document.createElement('button');cancel.textContent='先不改';cancel.onclick=()=>$('dialog').close();const go=document.createElement('button');go.className='primary';go.textContent=confirmLabel;go.onclick=()=>{$('dialog').close();onConfirm()};$('dialogActions').append(cancel,go)}if(!$('dialog').open)$('dialog').showModal()}
async function showDetails(title,path){openDialog(title,'正在读取…');try{const r=await api(path);const pre=document.createElement('pre');pre.textContent=r.text||'暂时没有匹配的记录';$('dialogBody').replaceChildren(pre)}catch(e){text('dialogBody',e.message)}}
$('loginForm').addEventListener('submit',async e=>{e.preventDefault();text('loginError','');$('loginButton').disabled=true;try{const r=await api('login',{method:'POST',body:JSON.stringify({password:$('password').value})});token=r.token;localStorage.setItem('mihomo-mobile-token',token);$('password').value='';showApp()}catch(e){text('loginError',e.message)}finally{$('loginButton').disabled=false}});
$('showPassword').onclick=()=>{const visible=$('password').type==='password';$('password').type=visible?'text':'password';text('showPassword',visible?'隐藏':'显示')};
document.querySelectorAll('nav button').forEach(b=>b.onclick=()=>navigate(b.dataset.page));$('currentNode').onclick=()=>navigate('nodes');$('powerButton').onclick=()=>act(state.rules.total?'off':'on');$('testButton').onclick=()=>act('test');$('delayButton').onclick=()=>act('delay');$('autoButton').onclick=()=>act('select',{name:'自动选择'});$('search').oninput=renderNodes;
$('updateSubscription').onclick=()=>{const raw=$('subscriptionURL').value.trim();if(!raw&&!hasSavedSubscription){toast('请先粘贴订阅链接',true);return}if(raw){try{const u=new URL(raw);if(!['http:','https:'].includes(u.protocol))throw Error()}catch{toast('链接应以 http:// 或 https:// 开头',true);return}}openDialog('更新订阅','会先备份并检查新配置，应用时代理会短暂断开。','开始更新',()=>act('subscription',{url:raw}))};
$('restoreButton').onclick=()=>openDialog('恢复上一版订阅','将恢复更新前的节点和配置，期间代理会短暂断开。','恢复上一版',()=>act('restore'));
$('repairButton').onclick=()=>act('repair');$('restartButton').onclick=()=>openDialog('重启代理服务','会短暂中断代理，完成后恢复全屋开关与设备策略。','重启服务',()=>act('restart'));$('recoverAutoButton').onclick=()=>act('auto');$('detailsButton').onclick=()=>showDetails('详细状态','details');$('logsButton').onclick=()=>showDetails('最近日志','logs');$('errorsButton').onclick=()=>showDetails('错误日志','logs?errors=1');$('installHelpButton').onclick=()=>openDialog('添加到手机桌面','用安卓浏览器打开此页面，点击右上角 ⋮ 菜单，选择“添加到主屏幕”或“添加到桌面”。以后点图标就能打开。部分浏览器会把它添加成网页快捷方式，这不影响使用。');$('logoutButton').onclick=()=>{showLogin();toast('已退出；路由器当前状态不会改变')};$('closeDialog').onclick=()=>$('dialog').close();$('dialog').addEventListener('click',e=>{if(e.target===$('dialog'))$('dialog').close()});window.addEventListener('hashchange',()=>{if(token)navigate(location.hash.slice(1))});document.addEventListener('visibilitychange',()=>{if(!document.hidden&&token){refresh();resumeJob()}});setInterval(()=>{if(token&&!document.hidden&&!busy)refresh()},15000);
if(token)showApp();else showLogin();
