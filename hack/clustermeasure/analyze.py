import sys, subprocess, statistics
raw = subprocess.run(["docker","exec","memtest-control-plane","cat","/provider-bin/samples.tsv"],
                     capture_output=True, text=True).stdout.splitlines()
hdr=None; rows=[]; marks=[]
for l in raw:
    if l.startswith("#"):
        p=l.split(None,2)
        marks.append((int(p[1]), p[2] if len(p)>2 else ""))
        continue
    f=l.split("\t")
    if hdr is None: hdr=f; continue
    if len(f)!=len(hdr): continue
    d=dict(zip(hdr,f))
    try: d["epoch"]=int(d["epoch"])
    except: continue
    rows.append(d)
# segment per arm
arms=[]
cur=None
for e,m in marks:
    t=m.split()
    if t[0]=="ARM":
        cur={"arm":t[1],"env":" ".join(t[3:]),"start":e}
        arms.append(cur)
    elif cur is None: continue
    elif t[0]=="READY": cur["ready"]=e; cur["ready_s"]=t[2].split("=")[1]
    elif t[0]=="PHASE": cur["idle_end"]=e
    elif t[0]=="CREATED": cur["created"]=e; cur["bucket_s"]=t[2].split("=")[1]+"/"+t[4]
    elif t[0]=="END": cur["end"]=e
    elif t[0]=="LOG" and "sinceStart" in m:
        import re
        mm=re.search(r'"sinceStart":"([^"]+)"',m); rr=re.search(r'"clusterResources":(\d+)',m)
        if mm: cur["sinceStart"]=mm.group(1)
        if rr: cur["nres"]=rr.group(1)
    elif t[0]=="LOG" and "startup heap" in m:
        import re
        mm=re.search(r'"took":"([^"]+)"',m)
        if mm: cur["scavenge_took"]=mm.group(1)

def sel(a,lo,hi):
    return [r for r in rows if r["arm"]==a["arm"] and lo<=r["epoch"]<=hi]
def med(rs,k):
    v=[float(r[k]) for r in rs if r.get(k) not in (None,"")]
    return statistics.median(v)/1024 if v else float('nan')

cols=["cg_current_kB","cg_peak_kB","anon_kB","cg_active_anon_kB","cg_anon_thp_kB","privclean_kB","VmRSS_kB","VmHWM_kB",
      "heapalloc_kB","heapinuse_kB","heapidle_kB","heapreleased_kB","heapsys_kB","gosys_kB"]
print(f"{'arm':<22} {'phase':<8} {'n':>2} {'podMEM':>7} {'podPEAK':>8} {'anon':>7} {'actAnon':>8} {'anonTHP':>8} {'pClean':>7} {'VmRSS':>7} {'VmHWM':>7} {'halloc':>7} {'hinuse':>7} {'hidle':>7} {'hrel':>7} {'hsys':>7} {'goSys':>7}")
for a in arms:
    if "ready" not in a: continue
    wins=[]
    if a.get("idle_end"): wins.append(("idle", a["ready"]+30, a["idle_end"]))
    if a.get("end"): wins.append(("steady", a["end"]-150, a["end"]))
    for name,lo,hi in wins:
        rs=sel(a,lo,hi)
        if not rs: continue
        print(f"{a['arm']:<22} {name:<8} {len(rs):>2} " + " ".join(f"{med(rs,c):>7.1f}" for c in cols))
print()
print(f"{'arm':<22} {'ready_s':>8} {'cfgBuild':>10} {'nRes':>6} {'buckets':>10} {'scavenge':>10}")
for a in arms:
    if "ready" not in a: continue
    print(f"{a['arm']:<22} {a.get('ready_s',''):>8} {a.get('sinceStart',''):>10} {a.get('nres',''):>6} {a.get('bucket_s',''):>10} {a.get('scavenge_took',''):>10}")
