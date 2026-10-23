#!/usr/bin/env python3
# Raw-draft PR experiments: same counting as ci-bill.py (each job rounded up to
# a minute, priced by OS, the gate's own jobs subtracted), but runs are given by
# ID (two PRs can share a branch) and coverage comes in two strengths:
#   strict   = upstream jobs whose draft task had verdict "task" and was reused
#   faithful = also reused review drafts that reproduce all of the job's steps
#              (only the environment differs, e.g. no container); hollow drafts
#              whose real work was an untranslated action are never counted.
# Usage: python3 -I ci-bill-draft.py [PR...]   (needs gh, read access)
import importlib.util, math, os
spec = importlib.util.spec_from_file_location("bill", os.path.join(os.path.dirname(os.path.abspath(__file__)), "ci-bill.py"))
RATE = {"linux": 0.006, "windows": 0.010, "macos": 0.062}

def load():
    # ci-bill.py prints its own table at import; only its helpers are wanted.
    src = open(spec.origin).read().split("\nEXP = {")[0]
    ns = {}
    exec(compile(src, spec.origin, "exec"), ns)
    return ns

B = load()

def run_jobs(repo, run_id):
    out = []
    for jp in B["gh"](f"repos/{repo}/actions/runs/{run_id}/jobs?per_page=100"):
        for j in jp["jobs"]:
            if not j.get("started_at") or not j.get("completed_at") or j["conclusion"] in ("skipped", "cancelled"):
                continue
            from datetime import datetime
            d = (datetime.fromisoformat(j["completed_at"].replace("Z", "+00:00")) - datetime.fromisoformat(j["started_at"].replace("Z", "+00:00"))).total_seconds()
            out.append(dict(job=j["name"], os=B["os_of"](j.get("labels") or [], j["name"]), sec=max(d, 0)))
    return out

EXP = {
    # PR, repo, upstream run IDs, gate run ID, strict job names, extra faithful job names
    "prometheus#4": ("mjtechguy/cinema-lab-prometheus", [39898752627], 39898752531,
                     {"List active LTS releases", "List the oldest supported Go version"},
                     {"Check generated parser", "Compliance testing", "Go tests", "Go tests for 32-bit x86", "Mixins tests", "UI tests"}),
    "eslint#3": ("mjtechguy/cinema-lab-eslint", [39914241580, 39914241048, 39914241081, 39914241043, 39914241076], 39914241061,
                 {'Test Types', 'Test (ubuntu-latest, 20.x)', 'Are the types wrong? (eslint-config-eslint, packages/eslint-config-eslint)', 'Test (ubuntu-latest, 20.19.0)', 'Are the types wrong? (eslint, .)', 'Test (ubuntu-latest, 24.x)', 'Verify Files', 'Test (ubuntu-latest, 22.x)', 'Test (ubuntu-latest, 24.x, --experimental-transform-types)', 'Test (ubuntu-latest, 26.x)'},
                 {'Test Ecosystem Plugin (@eslint-community/eslint-plugin-eslint-comments)', 'Test Ecosystem Plugin (eslint-plugin-unicorn)', 'Test Ecosystem Plugin (@typescript-eslint/typescript-eslint)', 'Test Ecosystem Plugin (@eslint/json)', 'Test Ecosystem Plugin (eslint-plugin-vue)', 'Browser Test', 'Test Ecosystem Plugin (@eslint/markdown)', 'Test Ecosystem Plugin (@eslint/css)'}),
}

import sys
for name, (repo, ups, gate_id, strict, faithful) in EXP.items():
    if sys.argv[1:] and name not in sys.argv[1:]:
        continue
    up = [j for r in ups for j in run_jobs(repo, r)]
    gate = run_jobs(repo, gate_id)
    u, g = B["tally"](up), B["tally"](gate)
    print(f"{name}: upstream {u[1]} billed min (${u[2]:.2f}), {len(up)} jobs; gate {g[1]} billed min (${g[2]:.2f}), {len(gate)} jobs")
    for label, names in (("strict", strict), ("faithful", strict | faithful)):
        cov = [j for j in up if j["job"] in names]
        missing = names - {j["job"] for j in cov}
        c = B["tally"](cov)
        print(f"  {label:8} covered {c[1]:4d} min (${c[2]:.2f}), {len(cov)} jobs; net saved {c[1]-g[1]:4d} of {u[1]} ({(c[1]-g[1])/u[1]*100:.0f}%), ${c[2]-g[2]:.2f} of ${u[2]:.2f} ({(c[2]-g[2])/u[2]*100:.0f}%)" + (f"  [no such job: {sorted(missing)}]" if missing else ""))
    for j in sorted(gate, key=lambda j: -j["sec"]):
        print(f"    gate job {j['job']}: {math.ceil(j['sec']/60)} min")
