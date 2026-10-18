#!/usr/bin/env python3
"""Measure selected Go unit packages only; never starts the application."""
import argparse, json, os, platform, resource, subprocess, sys, time

SAFE = {"./internal/importplan", "./internal/postgres", "./cmd/payplump"}

def main():
    p=argparse.ArgumentParser()
    p.add_argument("--run",action="store_true",help="run selected safe go test packages")
    p.add_argument("--packages",nargs="+",default=["./internal/importplan","./internal/postgres","./cmd/payplump"])
    p.add_argument("--image-file",help="optional local image archive; reports only its known file size")
    a=p.parse_args()
    if any(x not in SAFE for x in a.packages):
        p.error("only allowlisted unit-test packages are supported")
    commit=subprocess.run(["git","rev-parse","HEAD"],capture_output=True,text=True,check=True).stdout.strip()
    dirty=bool(subprocess.run(["git","status","--porcelain"],capture_output=True,text=True,check=True).stdout.strip())
    report={"architecture":platform.machine(),"commit":commit,"worktree_dirty":dirty,"workload":{"kind":"go_test","packages":a.packages},"executed":False,"duration_seconds":None,"child_max_rss_bytes":None,"child_cpu_seconds":None,"container_memory_bytes":None,"container_stats_reason":"cgroup memory counter unavailable unless exposed by runner","image_file_bytes":None,"compressed_image_bytes":None,"compressed_image_reason":"not measured: image archive compression is not inferred"}
    if a.image_file:
        try: report["image_file_bytes"]=os.path.getsize(a.image_file)
        except OSError: report["image_file_reason"]="specified image file unavailable"
    if a.run:
        allowed={"PATH","HOME","GOROOT","GOPATH","GOCACHE","GOMODCACHE","CGO_ENABLED","GOOS","GOARCH","TMPDIR","SSL_CERT_FILE"}
        env={k:v for k,v in os.environ.items() if k in allowed}
        env.update({"GOPROXY":"off","GOSUMDB":"sum.golang.org","GOTOOLCHAIN":"auto"})
        before=resource.getrusage(resource.RUSAGE_CHILDREN)
        start=time.monotonic()
        result=subprocess.run(["go","test","-count=1",*a.packages],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,env=env)
        report["duration_seconds"]=round(time.monotonic()-start,3);report["executed"]=True;report["exit_code"]=result.returncode
        usage=resource.getrusage(resource.RUSAGE_CHILDREN)
        report["child_cpu_seconds"]=round((usage.ru_utime-before.ru_utime)+(usage.ru_stime-before.ru_stime),3)
        report["child_max_rss_bytes"]=int(usage.ru_maxrss*(1 if sys.platform=="darwin" else 1024))
        try:
            with open("/sys/fs/cgroup/memory.current",encoding="ascii") as f: report["container_memory_bytes"]=int(f.read().strip())
            report["container_stats_reason"]=None
        except (OSError,ValueError): pass
    else: report["dry_run_reason"]="pass --run to execute only the allowlisted unit tests"
    print(json.dumps(report,sort_keys=True))
    return report.get("exit_code",0)
if __name__=="__main__": raise SystemExit(main())
