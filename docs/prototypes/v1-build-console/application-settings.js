/* 应用内密钥与成员管理的内存原型；不保存密钥明文、不连接真实身份或秘密存储。 */
const applicationAccessExamples = {
  'gateway-api': {'zhou@example.com':'developer','xu@example.com':'viewer'},
  'marketing-web': {'chen@example.com':'maintainer'},
  'worker-service': {'zhou@example.com':'developer'},
  'docs-site': {'chen@example.com':'maintainer'},
};
const applicationSecretExamples = {
  'gateway-api': [{id:'secret_gateway_go_proxy',name:'go_proxy_token',purpose:'私有 Go 模块代理',version:1,updated:'10 月 18 日'}],
  'marketing-web': [{id:'secret_web_npm',name:'npm_token',purpose:'拉取私有 npm 依赖',version:2,updated:'10 月 20 日'}],
};
const applicationRoleLabels={maintainer:'维护者',developer:'开发者',viewer:'查看者'};
function applicationAccess(a=applicationByName(state.currentApp)){return Object.hasOwn(applicationAccessExamples,a.name)?applicationAccessExamples[a.name]:(applicationAccessExamples[a.name]=Object.create(null));}
function applicationBuildSecrets(a=applicationByName(state.currentApp)){return Object.hasOwn(applicationSecretExamples,a.name)?applicationSecretExamples[a.name]:(applicationSecretExamples[a.name]=[]);}
function workspacePeople(){
  const people=[['林舟','lin@example.com','空间管理员','','林',''],['陈予','chen@example.com','普通成员','','陈','green'],['周宁','zhou@example.com','普通成员','','周','blue'],['许安','xu@example.com','普通成员','','许','pink'],...state.members];
  return people.map(([name,email,role,scope,initial,color])=>[name,email,role,role==='空间管理员'?'全部应用（继承）':applications.filter(a=>applicationAccess(a)[email]).map(a=>a.name+' · '+applicationRoleLabels[applicationAccess(a)[email]]).join('、')||'尚未分配应用',initial,color]);
}
function applicationSecretsSection(a){
  const secrets=applicationBuildSecrets(a),write=state.role==='admin';
  return `<section class="form-section" aria-labelledby="app-build-secrets-title"><div class="section-heading"><div><h2 id="app-build-secrets-title">构建密钥</h2><p>仅属于 ${escapeHTML(a.name)}，用于拉取私有依赖；已有值不回显。运行时密码请在应用变量中配置。</p></div>${write?button('添加构建密钥','app-secret-add','sm','plus'):''}</div>${secrets.length?`<div class="panel table-wrap"><table><thead><tr><th>名称</th><th>用途</th><th>版本</th><th>状态</th><th>操作</th></tr></thead><tbody>${secrets.map(secret=>`<tr><td class="mono">${escapeHTML(secret.name)}</td><td>${escapeHTML(secret.purpose)||'—'}</td><td>v${secret.version}</td><td>${badge('已配置','green')}</td><td>${write?button('替换','app-secret-edit-'+secret.id,'sm'):'仅可查看状态'}</td></tr>`).join('')}</tbody></table></div>`:'<div class="panel empty"><h3>本应用尚无构建密钥</h3><p>没有私有依赖时无需配置。</p></div>'}<p class="inline-note">平台输出仓库凭证由全局管理维护，不出现在这里。构建仅引用本应用密钥及其版本。</p></section>`;
}
function applicationSecretDialog(id=''){
  const a=applicationByName(state.currentApp),secret=applicationBuildSecrets(a).find(s=>s.id===id);
  if(id&&!secret){toast('密钥不属于当前应用或已不可用');return;}
  state.pendingAppSecret={app:a.name,id,version:secret?.version};
  openDialog(`<h2 id="dialog-title">${secret?'替换':'添加'}构建密钥</h2><p>所属应用：${escapeHTML(a.name)}。保存后只保留引用与版本，已有明文不会返回。</p><div class="field"><label for="app-secret-name">名称</label><input id="app-secret-name" value="${secret?escapeHTML(secret.name):''}" ${secret?'readonly':''} placeholder="例如 npm_token" aria-describedby="app-secret-error" /></div><div class="field"><label for="app-secret-purpose">用途 <span class="muted">可选</span></label><input id="app-secret-purpose" value="${secret?escapeHTML(secret.purpose):''}" /></div><div class="field"><label for="app-secret-value">${secret?'新的值':'密钥值'}</label><input id="app-secret-value" type="password" autocomplete="new-password" aria-describedby="app-secret-error" /><p class="hint">原型不保存该输入；正式实现使用加密秘密存储。</p></div><p id="app-secret-error" class="field-error" role="alert"></p>${dialogFooter('app-secret-save','保存密钥')}`);
}
function applicationMembersPage(){
  const a=applicationByName(state.currentApp),grants=applicationAccess(a),write=state.role==='admin';
  const people=workspacePeople().filter(p=>p[2]==='空间管理员'||grants[p[1]]);
  return applicationFrame(a,`${runtimeSettingsNav()}<div class="section-heading"><div><h2>成员与权限</h2><p>只管理 ${escapeHTML(a.name)} 的访问权；成员需要先加入当前工作空间。</p></div>${write?button('添加应用成员','app-access-add','primary','plus'):''}</div><div class="notice">${icon('shield',16)}<p>入口位于应用，授权权限独立检查。第一版由工作空间管理员修改应用角色；不会因此授予空间或全局管理权。</p></div><div class="panel table-wrap"><table><thead><tr><th>成员</th><th>应用角色</th><th>来源</th><th>操作</th></tr></thead><tbody>${people.map(([name,email,role])=>`<tr><td><strong>${escapeHTML(name)}</strong><small>${escapeHTML(email)}</small></td><td>${role==='空间管理员'?'管理员访问':applicationRoleLabels[grants[email]]}</td><td>${role==='空间管理员'?'继承空间管理员':'本应用显式授权'}</td><td>${role==='空间管理员'?'<span class="muted">在空间层管理</span>':write?button('修改角色','app-access-edit-'+encodeURIComponent(email),'sm')+' '+button('收回访问','app-access-remove-'+encodeURIComponent(email),'ghost sm'):'只读'}</td></tr>`).join('')}</tbody></table></div>${write?`<p class="inline-note">需要邀请新成员或修改空间角色？${navButton('前往空间成员','members','ghost sm','users')}</p>`:''}`,'settings',false);
}
function applicationAccessDialog(email=''){
  const a=applicationByName(state.currentApp),grants=applicationAccess(a),people=workspacePeople();
  const person=email?people.find(p=>p[1]===email&&p[2]!=='空间管理员'&&grants[email]):null;
  if(email&&!person){toast('成员不在本应用授权范围内');return;}
  const candidates=people.filter(p=>p[2]!=='空间管理员'&&!grants[p[1]]);
  if(!email&&!candidates.length){openDialog('<h2 id="dialog-title">没有可添加的成员</h2><p>当前空间的成员已拥有本应用访问权。邀请新人需要在工作空间中完成。</p>'+dialogFooter('close-dialog','关闭'));return;}
  state.pendingAppAccess={app:a.name,email};
  openDialog(`<h2 id="dialog-title">${email?'修改应用角色':'添加应用成员'}</h2><p>应用固定为 ${escapeHTML(a.name)}；只改变本应用的访问权。</p>${person?`<p><strong>${escapeHTML(person[0])}</strong> · ${escapeHTML(person[1])}</p>`:`<div class="field"><label for="app-access-member">工作空间成员</label><select id="app-access-member" aria-describedby="app-access-error"><option value="" selected disabled>选择已有成员</option>${candidates.map(p=>`<option value="${escapeHTML(p[1])}">${escapeHTML(p[0])} · ${escapeHTML(p[1])}</option>`).join('')}</select></div>`}<div class="field"><label for="app-access-role">应用角色</label><select id="app-access-role" aria-describedby="app-access-error">${['viewer','developer','maintainer'].map(role=>`<option value="${role}" ${role===(person?grants[email]:'viewer')?'selected':''}>${applicationRoleLabels[role]}</option>`).join('')}</select></div><p class="inline-note">维护者管理本应用配置；开发者执行构建与发布；查看者只读。空间与全局角色不变。</p><p id="app-access-error" class="field-error" role="alert"></p>${dialogFooter('app-access-save','保存应用授权')}`);
}
function applicationSettingsHandleAction(action){
  if(action==='application-permissions'){
    if(state.role!=='admin'){toast('需要工作空间管理员权限');return true;}
    openDialog(`<h2 id="dialog-title">打开应用权限</h2><p>授权直接在应用中维护，空间成员页提供同一份数据的概览与入口。</p><div class="field"><label for="access-target-app">应用</label><select id="access-target-app"><option value="" selected disabled>选择应用</option>${applications.map(a=>`<option value="${a.name}">${escapeHTML(a.name)}</option>`).join('')}</select></div>${dialogFooter('open-app-access','进入应用权限')}`);return true;
  }
  if(action==='open-app-access'){
    if(state.role!=='admin'){toast('需要工作空间管理员权限');return true;}
    const name=document.querySelector('#access-target-app').value;if(!applications.some(a=>a.name===name)){toast('请先选择应用');return true;}
    dialog.close();dialog.innerHTML='';go(applicationRoute(name,'settings/access'));return true;
  }
  if(!action.startsWith('app-secret-')&&!action.startsWith('app-access-'))return false;
  if(state.role!=='admin'){toast(action.startsWith('app-access-')?'当前没有修改应用成员的权限':'当前没有管理本应用构建密钥的权限');return true;}
  const a=applications.find(a=>a.name===state.currentApp);if(!a||!visibleApplications().includes(a)){toast('当前应用不存在或无权访问');return true;}
  if(action==='app-secret-add'){applicationSecretDialog();return true;}
  if(action.startsWith('app-secret-edit-')){applicationSecretDialog(action.slice('app-secret-edit-'.length));return true;}
  if(action==='app-secret-save'){
    const request=state.pendingAppSecret;if(!request||request.app!==a.name){toast('应用上下文已变化，请重新打开');return true;}
    const input=document.querySelector('#app-secret-name'),name=input.value.trim(),valueInput=document.querySelector('#app-secret-value'),purpose=document.querySelector('#app-secret-purpose').value.trim();
    const secrets=applicationBuildSecrets(a),existing=secrets.find(s=>s.id===request.id);
    let error='';if(!/^[A-Za-z_][A-Za-z0-9_-]{0,63}$/.test(name))error='名称需以字母或下划线开头，只含字母、数字、下划线或连字符。';
    else if(!request.id&&secrets.some(s=>s.name===name))error='本应用已存在同名密钥，请替换已有密钥。';
    else if(!valueInput.value)error='请填写新的密钥值。';
    else if(request.id&&(!existing||existing.version!==request.version))error='密钥版本已变化，请重新打开替换表单。';
    if(error){document.querySelector('#app-secret-error').textContent=error;(!valueInput.value&&name?valueInput:input).focus();return true;}
    if(existing)Object.assign(existing,{purpose,version:existing.version+1,updated:'刚刚'});
    else secrets.push({id:a.id+'_secret_'+(secrets.length+1),name,purpose,version:1,updated:'刚刚'});
    dialog.close();dialog.innerHTML='';render();toast('已保存本应用密钥元数据示例，未保存输入明文');return true;
  }
  if(action==='app-access-add'){applicationAccessDialog();return true;}
  if(action.startsWith('app-access-edit-')){applicationAccessDialog(decodeURIComponent(action.slice('app-access-edit-'.length)));return true;}
  if(action==='app-access-save'){
    const request=state.pendingAppAccess;if(!request||request.app!==a.name){toast('应用上下文已变化，请重新打开');return true;}
    const email=request.email||document.querySelector('#app-access-member').value,role=document.querySelector('#app-access-role').value;
    if(!workspacePeople().some(p=>p[1]===email&&p[2]!=='空间管理员')||!['maintainer','developer','viewer'].includes(role)){document.querySelector('#app-access-error').textContent='请选择当前空间的有效成员与应用角色。';return true;}
    applicationAccess(a)[email]=role;dialog.close();dialog.innerHTML='';render();toast('已更新本应用授权；工作空间身份未改变');return true;
  }
  if(action.startsWith('app-access-remove-')){
    const email=decodeURIComponent(action.slice('app-access-remove-'.length));if(!applicationAccess(a)[email])return true;
    state.pendingAppAccessRemove={app:a.name,email};openDialog(`<h2 id="dialog-title">收回应用访问权？</h2><p>${escapeHTML(email)} 将失去 ${escapeHTML(a.name)} 的显式访问权。该成员仍留在工作空间中，不影响其他应用或已经运行的服务。</p>${dialogFooter('app-access-confirm-remove','收回本应用访问')}`);return true;
  }
  if(action==='app-access-confirm-remove'){
    const request=state.pendingAppAccessRemove;if(!request||request.app!==a.name){toast('应用上下文已变化，请重新打开');return true;}
    delete applicationAccess(a)[request.email];dialog.close();dialog.innerHTML='';render();toast('已收回本应用显式访问权，空间成员关系保留');return true;
  }
  return true;
}
