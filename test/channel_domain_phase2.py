"""第二步：对第一步能收到验证码的 (渠道,域名) 组合跑多次完整注册。

读取 data/channel_probe_result.json（第一步产物），挑出 code_received>0 的组合，
每个跑 <times> 次完整注册，统计成功率，结果写入 data/channel_probe_phase2.json。

用法：
    python test/channel_domain_phase2.py [每组合次数] [代理]

作者：wangqiupei
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import warnings

warnings.filterwarnings("ignore")

from test.channel_domain_probe import _probe_once  # noqa: E402


def main(times: int, proxy: str) -> None:
    root = Path(__file__).resolve().parents[1]
    phase1 = json.loads((root / "data" / "channel_probe_result.json").read_text(encoding="utf-8"))
    # 第一步已注册成功过的组合最值得复验；如需更广可改成 code_received>0
    candidates = [(v["provider"], v["domain"]) for v in phase1.values() if int(v.get("registered") or 0) > 0]
    print(f"第一步注册成功过的组合共 {len(candidates)} 个，每个跑 {times} 次完整注册\n")

    out: dict = {}
    for ptype, domain in candidates:
        label = f"{ptype}@{domain}" if domain else f"{ptype}(服务端域名)"
        reg = code = 0
        last_err = ""
        for i in range(1, times + 1):
            r = _probe_once(ptype, domain, proxy, i)
            code += int(r["code_received"])
            reg += int(r["registered"])
            if r["error"]:
                last_err = r["error"]
            print(f"  [{label}] 第{i}/{times}: {'注册成功' if r['registered'] else ('收到码' if r['code_received'] else '失败')} {r['address']}")
        out[label] = {"provider": ptype, "domain": domain, "times": times, "code_received": code, "registered": reg, "last_error": last_err}
        print(f"  => [{label}] 收码{code}/{times} 注册{reg}/{times}\n")

    (root / "data" / "channel_probe_phase2.json").write_text(json.dumps(out, ensure_ascii=False, indent=2), encoding="utf-8")
    print("================ 第二步汇总（注册成功率）================")
    for label, s in sorted(out.items(), key=lambda kv: -kv[1]["registered"]):
        print(f"{label:34} 注册{s['registered']}/{s['times']} 收码{s['code_received']}/{s['times']}")


if __name__ == "__main__":
    n = int(sys.argv[1]) if len(sys.argv) > 1 else 5
    px = sys.argv[2] if len(sys.argv) > 2 else "http://127.0.0.1:7890"
    main(n, px)
