#!/usr/bin/env python3
"""Full offline suite ablation of the three uncommitted review repairs.
Runs only isolated source snapshots; production source is never mutated.
"""
import argparse, base64, concurrent.futures, hashlib, importlib.util, itertools, json, os, pathlib, subprocess, tempfile
import ablate_full as framework

HTTP='internal/app/http.go'
DEPLOY='deploy/wslc-common.ps1'

def once(s, old, new):
    if s.count(old)!=1: raise ValueError('mutation anchor is not unique: '+old[:80])
    return s.replace(old,new,1)

def mutate(files, name):
    s=files[DEPLOY]
    if name=='restore':
        start=s.index('  # Docker creates missing named volumes')
        end=s.index('  # A failed run/update',start)
        s=s[:start]+s[end:]
        s=once(s,"$restore = $validate + ' && find /target", "$restore = 'find /target")
    elif name=='inspect':
        start=s.index('  $backupVolume =')
        end=s.index('  $validate =',start)
        s=s[:start]+s[end:]
    elif name=='volume_identity':
        line=next(x for x in s.splitlines() if 'if ($backupVolume.Count' in x)
        s=once(s,line,'  # ablated inspected volume identity validation')
    elif name=='preflight':
        line=next(x for x in s.splitlines() if 'Invoke-UasDocker' in x and "'-c',$validate)" in x)
        s=once(s,line,'  # ablated preflight snapshot validation')
    elif name=='recheck':
        s=once(s,"$restore = $validate + ' && find /target", "$restore = 'find /target")
    elif name.startswith('file_'):
        file={'file_db':'search.db','file_master':'master.key','file_credentials':'credentials.json'}[name]
        s=once(s,'test -s /backup/'+file, 'true')
    elif name=='dedupe':
        files[HTTP]=once(files[HTTP],'if duplicate {','if false && duplicate {')
    elif name=='hidden':
        files[HTTP]=once(files[HTTP],'hasAttr(n, "hidden")','attr(n, "hidden") != ""')
    elif name=='negative_control':
        files[HTTP]=once(files[HTTP],'if a.Key == name {','if name == a.Key {')
    else: raise ValueError(name)
    files[DEPLOY]=s

def main():
    p=argparse.ArgumentParser();p.add_argument('--report',required=True);p.add_argument('--scratch-root',required=True);p.add_argument('--workers',type=int,choices=[1,2],default=1)
    args=p.parse_args()
    repo=pathlib.Path(__file__).resolve().parent.parent
    dest=pathlib.Path(args.report).resolve();dest.parent.mkdir(parents=True,exist_ok=True)
    logs=dest.with_suffix('');logs.mkdir(exist_ok=True)
    paths=subprocess.check_output(['git','ls-files','-co','--exclude-standard','-z'],cwd=repo).decode().split('\0')
    paths=sorted(set(x for x in paths if x and not x.startswith('docs/audit/review-repairs-')))
    snapshot={x:(repo/x).read_bytes() for x in paths if (repo/x).is_file()}
    report={'revision':subprocess.check_output(['git','rev-parse','HEAD'],cwd=repo,text=True).strip(),
      'source':'tracked and nonignored untracked working tree snapshot, including uncommitted repairs',
      'snapshot_sha256':{x:hashlib.sha256(b).hexdigest() for x,b in snapshot.items()},
      'method':'all 2^3 repair combinations plus finite component ablations; each runs all offline suites; one repetition',
      'workers':args.workers,'environment':{'go':subprocess.check_output(['go','version'],text=True).strip(),'pwsh':subprocess.check_output(['pwsh','-Version'],text=True).strip()},'cases':[]}
    cases=[('baseline_before',[])]
    for n in range(1,4):
        for combo in itertools.combinations(['restore','dedupe','hidden'],n):cases.append(('without_'+'_'.join(combo),list(combo)))
    cases += [('without_'+n,[n]) for n in ['inspect','volume_identity','preflight','recheck','file_db','file_master','file_credentials']]
    cases += [('negative_control',['negative_control']),('baseline_after',[])]
    # Never inherit private service settings into tests or persisted output.
    for k in list(os.environ):
        if k.startswith('SAS_') or k.lower() in {'http_proxy','https_proxy','all_proxy','no_proxy'}:os.environ.pop(k)
    os.environ.pop('PSModulePath', None)
    native=framework.command_result
    def checkpoint():dest.write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n',encoding='utf-8')
    def trial(item):
        name,changes=item
        # Each concurrent trial owns its runner module and log capture hook.
        spec=importlib.util.spec_from_file_location('trial_framework',repo/'scripts/ablate_full.py')
        local=importlib.util.module_from_spec(spec);spec.loader.exec_module(local)
        case={'name':name,'removed':changes,'logs':[]}
        with tempfile.TemporaryDirectory(prefix='uas-review-ablate-',dir=args.scratch_root) as directory:
            root=pathlib.Path(directory)
            for path,b in snapshot.items():
                f=root/path;f.parent.mkdir(parents=True,exist_ok=True);f.write_bytes(b)
            files={x:(root/x).read_text(encoding='utf-8') for x in [HTTP,DEPLOY]}
            for change in changes:mutate(files,change)
            for path,s in files.items():(root/path).write_text(s,encoding='utf-8')
            case['mutated_sha256']={x:hashlib.sha256(s.encode()).hexdigest() for x,s in files.items()}
            mock=root/'deploy/tests/wslc-mock.tests.ps1'
            mock.write_text(once(mock.read_text(encoding='utf-8'),'throw $Message',"throw ('ABLATION_ASSERT: '+$Message)"),encoding='utf-8')
            def capture(command,root,timeout=240):
                # Linux PowerShell exports a Linux PSModulePath into this environment.
                # Windows PowerShell must discover its own Windows modules.
                if command[0].endswith('powershell.exe'):
                    script=command[-1].replace("'", "''")
                    source="$env:PSModulePath = Join-Path $PSHOME 'Modules'; & '"+script+"'"
                    command=command[:-2]+['-EncodedCommand',base64.b64encode(source.encode('utf-16le')).decode()]
                code,text,seconds=native(command,root,timeout)
                log=logs/(name+'-'+str(len(case['logs']))+'.log')
                log.write_text(text,encoding='utf-8');case['logs'].append(log.name)
                return code,text,seconds
            local.command_result=capture
            suites=local.run_suites(root,'/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe')
            for label,cmd in [('go_vet',['go','vet','./...']),('python_all',['python3','-m','unittest','discover','-s','scripts','-p','test_*.py'])]:
                code,text,secs=capture(cmd,root)
                suites[label]={'status':'pass' if code==0 else 'runner_error','exit_code':code,'seconds':secs}
            case['suites']=suites;case['outcome']=framework.classify(suites)
            failed=suites['go_race_all']['failed_tests']
            witnesses=[t for t in failed if t.endswith('/TestReplacementCredentialUniqueness') or t.endswith('/TestReadHTMLHiddenAttributes')]
            # Only the new invalid-backup assertions attribute deployment failures.
            for log in case['logs']:
                txt=(logs/log).read_text(encoding='utf-8')
                for marker in ['Invalid backup accepted:','Invalid backup touched current deployment']:
                    if 'ABLATION_ASSERT: '+marker in txt:witnesses.append(marker)
            case['witnesses']=sorted(set(witnesses))
            if changes and changes!=['negative_control'] and case['outcome']=='detected' and not witnesses:case['outcome']='unwitnessed_failure'
            return case
    def record(case):
        report['cases'].append(case);checkpoint()
        print(case['name']+': '+case['outcome'],flush=True)
    before=trial(cases[0]);record(before)
    if before['outcome']!='survived':raise RuntimeError('baseline failed: inspect report/logs before attributing mutations')
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.workers) as pool:
        pending=[pool.submit(trial,case) for case in cases[1:-1]]
        for future in concurrent.futures.as_completed(pending):record(future.result())
    after=trial(cases[-1]);record(after)
    report['cases'].sort(key=lambda case: [name for name,_ in cases].index(case['name']))
    report['summary']={o:sum(c['outcome']==o for c in report['cases']) for o in ['survived','detected','invalid_or_incomplete','unwitnessed_failure']}
    report['summary']['surviving_harmful_variants']=[c['name'] for c in report['cases'] if c['removed'] and c['removed']!=['negative_control'] and c['outcome']=='survived']
    checkpoint();print(json.dumps(report['summary']),flush=True)
if __name__=='__main__':main()
