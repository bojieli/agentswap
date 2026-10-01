#!/usr/bin/env python3
"""Verify native OpenCode import/export and Codex read/resume without model calls.
Requires Go, Python 3, OpenCode and Codex. Uses only fabricated histories and
isolated harness state. Retains its temporary artifacts for inspection.
"""
import pathlib, tempfile, os, subprocess, json, select, time
root=pathlib.Path(tempfile.mkdtemp(prefix='agentswap-native-'))
cwd=root/'project'; cwd.mkdir()
env=os.environ.copy()
for k,v in {'CLAUDE_CONFIG_DIR':root/'claude','CODEX_HOME':root/'codex','KIMI_CODE_HOME':root/'kimi','KIMI_SHARE_DIR':root/'kimi-python','AGENTSWAP_HOME':root/'agentswap','XDG_DATA_HOME':root/'xdg-data','XDG_CONFIG_HOME':root/'xdg-config','XDG_CACHE_HOME':root/'xdg-cache','XDG_STATE_HOME':root/'xdg-state','OPENCODE_CONFIG_DIR':root/'oc-config'}.items(): env[k]=str(v)
for k in ['OPENAI_API_KEY','ANTHROPIC_API_KEY','ANTHROPIC_AUTH_TOKEN','OPENCODE_CONFIG_CONTENT','OPENCODE_CONFIG','AGENTSWAP_OPENCODE_BIN']:env.pop(k,None)
env['OPENCODE_DISABLE_MODELS_FETCH']='true'
sid='11111111-1111-4111-8111-111111111111'
projectkey=''.join(c if c.isalnum() else '-' for c in str(cwd))
dir=root/'claude'/'projects'/projectkey; dir.mkdir(parents=True)
def line(role,content,id):return {'type':role,'uuid':id,'sessionId':sid,'cwd':str(cwd),'timestamp':'2026-10-01T01:00:00Z','message':{'role':role,'content':content,'model':'claude-sonnet-4-6'}}
records=[line('user','Investigate the parser','u1'),line('assistant',[{'type':'tool_use','id':'task1','name':'Agent','input':{'description':'Inspect parser','subagent_type':'explorer','prompt':'Inspect parser'}}],'a1'),line('user',[{'type':'tool_result','tool_use_id':'task1','content':'agentId: abcd\nParser investigated.'}],'u2'),line('assistant','Continue the investigation.','a2')]
(dir/(sid+'.jsonl')).write_text(''.join(json.dumps(r)+'\n' for r in records))
subs=dir/sid/'subagents';subs.mkdir(parents=True)
subs.joinpath('agent-abcd.jsonl').write_text(''.join(json.dumps({**line(role,text,id),'isSidechain':True,'agentId':'abcd'})+'\n' for role,text,id in [('user','Inspect parser','su'),('assistant','The parser has a bug in token splitting.','sa')]))
subs.joinpath('agent-abcd.meta.json').write_text(json.dumps({'agentType':'explorer','description':'Inspect parser','toolUseId':'task1'}))
repo=pathlib.Path(__file__).resolve().parents[1]
exe=str(root/'agentswap-bin')
subprocess.run(['go','build','-o',exe,'./cmd/agentswap'],cwd=repo,check=True)
def transfer(a,b,sid):
 p=subprocess.run([exe,'teleport',a,b,'--session',sid,'--cwd',str(cwd)],env=env,text=True,capture_output=True,timeout=45)
 if p.returncode:raise Exception(p.stderr)
 print(p.stdout.strip());print(p.stderr.strip())
 for line in p.stdout.splitlines():
  if line.startswith('Created '):return line.split(' session ')[1].strip()
 raise Exception('No session id')
oc=transfer('claude','opencode',sid)
export=subprocess.run(['opencode','export',oc],env=env,cwd=cwd,text=True,capture_output=True,timeout=45)
if export.returncode:raise Exception(export.stderr)
data=json.loads(export.stdout);meta=[p.get('state',{}).get('metadata',{}) for m in data['messages'] for p in m['parts'] if p['type']=='tool'][0]
child=meta['sessionId'];exp=subprocess.run(['opencode','export',child],env=env,cwd=cwd,text=True,capture_output=True,timeout=45)
assert exp.returncode==0,exp.stderr
child_data=json.loads(exp.stdout);assert child_data['info']['parentID']==oc
assert 'token splitting' in exp.stdout
print('PASS native OpenCode root and child import/export')
codex=transfer('opencode','codex',oc)
claude=transfer('codex','claude',codex)
files=list((root/'codex'/'sessions').rglob('*.jsonl'))
proc=subprocess.Popen(['codex','app-server','--stdio'],env=env,cwd=cwd,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=open(root/'codex-stderr.log','w'),text=True,bufsize=1)
def rpc(id,method,params):
 proc.stdin.write(json.dumps({'id':id,'method':method,'params':params})+'\n');proc.stdin.flush()
 end=time.monotonic()+30
 while time.monotonic()<end:
  ready,_,_=select.select([proc.stdout],[],[],1)
  if ready:
   line=proc.stdout.readline()
   if not line:raise Exception('app-server exited')
   msg=json.loads(line)
   if msg.get('id')==id:
    if 'error' in msg:raise Exception(json.dumps(msg['error']))
    return msg['result']
 raise Exception('app-server RPC timeout '+method)
try:
 rpc(1,'initialize',{'clientInfo':{'name':'agentswap-native-readback','version':'0.8.0'},'capabilities':{'experimentalApi':True}})
 proc.stdin.write(json.dumps({'method':'initialized'})+'\n');proc.stdin.flush()
 for n,path in enumerate(files,2):
  threadid=json.loads(path.read_text().splitlines()[0])['payload']['id']
  result=rpc(n*2,'thread/read',{'threadId':threadid,'includeTurns':True})
  assert result['thread']['id']==threadid
  print('PASS native Codex thread/read',threadid)
  resumed=rpc(n*2+1,'thread/resume',{'threadId':threadid,'cwd':str(cwd)})
  assert resumed['thread']['id']==threadid
  print('PASS native Codex thread/resume (no turn)',threadid)
finally:proc.terminate();proc.wait(timeout=10)
print('ARTIFACTS',root)
