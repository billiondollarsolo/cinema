#!/usr/bin/env python3
# How the lab counts savings (RESULTS.md, "How savings are counted"): per job,
# GitHub rounds up to whole minutes and bills by runner OS (docs.github.com
# billing/reference/actions-runner-pricing, standard runners, 2026 rates).
# Covered = upstream jobs the gate's reused tasks stand in for; the gate's own
# jobs are subtracted. Usage: python3 -I ci-bill.py   (needs gh, read access)
import json, math, subprocess, sys
RATE = {"linux": 0.006, "windows": 0.010, "macos": 0.062}
def gh(path):
    out = subprocess.run(["gh", "api", "--paginate", path], capture_output=True, text=True, check=True).stdout
    dec, i, docs = json.JSONDecoder(), 0, []
    while i < len(out):
        while i < len(out) and out[i].isspace():
            i += 1
        if i >= len(out):
            break
        d, i = dec.raw_decode(out, i)
        docs.append(d)
    return docs
def os_of(labels, name):
    s = " ".join(labels + [name]).lower()
    return "windows" if "windows" in s else "macos" if "macos" in s else "linux"
def jobs(repo, branch):
    runs = gh(f"repos/{repo}/actions/runs?branch={branch}&event=pull_request&per_page=100")
    out = []
    for page in runs:
        for r in page["workflow_runs"]:
            for jp in gh(f"repos/{repo}/actions/runs/{r['id']}/jobs?per_page=100"):
                for j in jp["jobs"]:
                    if not j.get("started_at") or not j.get("completed_at") or j["conclusion"] == "skipped":
                        continue
                    from datetime import datetime
                    d = (datetime.fromisoformat(j["completed_at"].replace("Z","+00:00")) - datetime.fromisoformat(j["started_at"].replace("Z","+00:00"))).total_seconds()
                    out.append(dict(wf=r["name"], job=j["name"], os=os_of(j.get("labels") or [], j["name"]), sec=max(d, 0)))
    return out
def tally(js):
    raw = sum(j["sec"] for j in js) / 60
    billed = sum(math.ceil(j["sec"] / 60) for j in js)
    usd = sum(math.ceil(j["sec"] / 60) * RATE[j["os"]] for j in js)
    return raw, billed, usd
EXP = {
  "prometheus#2": ("mjtechguy/cinema-lab-prometheus", "lab/pr-1632", lambda j: j["wf"] == "CI" and (j["job"] in {"Go tests", "More Go tests", "golangci-lint", "Go tests for 32-bit x86", "UI tests", "Compliance testing", "Check generated parser"} or j["job"].startswith("Build Prometheus for common architectures"))),
  "traefik#3": ("mjtechguy/cinema-lab-traefik", "lab/pr-1307", lambda j: j["wf"] in {"Build Binaries", "Validate", "Test Unit", "Test Integration"} and j["os"] == "linux"),
  "prettier#1": ("mjtechguy/cinema-lab-prettier", "lab/pr-1800", lambda j: (j["wf"], j["job"]) in {("Lint", "Lint"), ("Prod", "Lint"), ("Prod", "Build / Build"), ("Prod", "Node.js 22 on Linux"), ("Prod", "Node.js 24 on Linux"), ("Prod", "[Full Test]  Node.js 26 on Linux"), ("Dev", "Node.js 22 on Linux"), ("Dev", "[Full Test]  Node.js 26 on Linux")}),
  "eslint#1": ("mjtechguy/cinema-lab-eslint", "lab/pr-1807", lambda j: j["wf"] == "CI" and j["job"] in {"Verify Files", "Test Types", "Test (ubuntu-latest, 20.x)", "Test (ubuntu-latest, 22.x)", "Test (ubuntu-latest, 24.x)", "Test (ubuntu-latest, 26.x)"}),
}
print(f"{'PR':13} {'upstream raw/billed min, $':30} {'covered raw/billed min, $':30} {'gate billed min, $':20} {'net billed min saved':22} {'net $ saved (private)':22}")
for name, (repo, br, covered) in EXP.items():
    js = jobs(repo, br)
    up = [j for j in js if j["wf"] != "cinema"]
    gate = [j for j in js if j["wf"] == "cinema"]
    cov = [j for j in up if covered(j)]
    u, c, g = tally(up), tally(cov), tally(gate)
    print(f"{name:13} {u[0]:6.1f} / {u[1]:4d} / ${u[2]:5.2f}{'':9} {c[0]:6.1f} / {c[1]:4d} / ${c[2]:5.2f}{'':9} {g[1]:3d} / ${g[2]:4.2f}{'':9} {c[1]-g[1]:4d} of {u[1]:4d} ({(c[1]-g[1])/u[1]*100:3.0f}%)   ${c[2]-g[2]:5.2f} of ${u[2]:5.2f} ({(c[2]-g[2])/u[2]*100:3.0f}%)")
    byos = {}
    for j in up:
        byos.setdefault(j["os"], [0, 0.0]); byos[j["os"]][0] += math.ceil(j["sec"]/60); byos[j["os"]][1] += math.ceil(j["sec"]/60)*RATE[j["os"]]
    print(f"{'':13} upstream by OS: " + ", ".join(f"{k} {v[0]} min ${v[1]:.2f}" for k, v in sorted(byos.items())) + f"; covered jobs: {len(cov)}")
