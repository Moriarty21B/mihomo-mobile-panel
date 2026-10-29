'use strict';
function renderStatistics(){
 const q=state?.quota,m=state?.maintenance,full=q?.available&&q?.full;
 $('quotaProgress').classList.toggle('hidden',!full||q.unlimited);
 if(q?.available){const formatted=bytes(q.remaining).split(' ');text('quotaRemaining',q.unlimited?'不限流量':formatted[0]);text('quotaUnit',q.unlimited?'':formatted[1]);text('quotaUsage',full?'已用 '+bytes(q.upload+q.download)+' / 总量 '+(q.unlimited?'不限':bytes(q.total)):'服务商仅提供剩余流量');
  $('quotaProgress').firstElementChild.style.width=(q.total?Math.min(100,(q.upload+q.download)/q.total*100):0)+'%';$('quotaProgress').classList.toggle('low',!q.unlimited&&q.total>0&&q.remaining/q.total<.1);
  text('quotaExpire',q.expire?'到期 · '+new Date(q.expire).toLocaleDateString('zh-CN'):'未提供到期时间');text('quotaReset',q.resetDays?'距重置 '+q.resetDays+' 天':'');
  const stale=Date.now()-Date.parse(q.updated)>60*60*1000;text('quotaUpdated',(stale?'上次数据 · ':'更新于 ')+new Date(q.updated).toLocaleString('zh-CN')+' · 来自机场');
 }else{text('quotaRemaining',state?.hasSubscription?'暂无数据':'未保存订阅');text('quotaUnit','');text('quotaUsage','保存订阅后自动读取；暂不可用时不会显示为零');text('quotaExpire','');text('quotaReset','');text('quotaUpdated','可点击刷新重试，网络规则不会改变。')}
 if(m){text('storageSummary',m.storage?.available?'代理存储可用 '+bytes(m.storage.free)+' / '+bytes(m.storage.total):'存储数据暂不可用');text('logSummary','自动清理已开启 · 当前日志 '+bytes(m.logBytes)+' · 上限约 10 MB');text('cleanupSummary',m.error||(m.cleaned?'上次清理 '+new Date(m.cleaned).toLocaleString('zh-CN')+' · 释放 '+bytes(m.freed):'每分钟检查，超过上限缩减日志；旧日志最长保留 7 天。'));$('storageSummary').classList.toggle('low-storage',!!m.storage?.low)}
}
$('refreshQuota').onclick=()=>act('quota');$('cleanLogs').onclick=()=>act('clean-logs');if(state)renderStatistics();
