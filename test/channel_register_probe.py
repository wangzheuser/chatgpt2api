"""逐个临时邮箱渠道跑真实 OpenAI 注册测试。

用法（在项目根目录、激活 .venv 后）：
    python test/channel_register_probe.py <provider_type> [次数] [代理]

例：
    python test/channel_register_probe.py dropmail 5 http://127.0.0.1:7890

每次尝试会走完整注册流程（authorize → register → 发码 → 等码 → 建账号 → 换 token），
打印每次的成功/失败及失败阶段，最后汇总成功率。仅用于评估渠道可用性，不入库。

作者：wangqiupei
"""

from __future__ import annotations

import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import warnings

warnings.filterwarnings("ignore")

from services.register import openai_register  # noqa: E402


def probe(provider_type: str, times: int, proxy: str) -> None:
    # 注入待测渠道与代理；wait_timeout 放宽到 150s 给 OTP 邮件足够时间。
    openai_register.config["proxy"] = proxy
    openai_register.config["mail"] = {
        "request_timeout": 30,
        "wait_timeout": 150,
        "wait_interval": 3,
        "api_use_register_proxy": True,
        "providers": [{"type": provider_type, "enable": True}],
    }

    results: list[tuple[bool, str]] = []
    for i in range(1, times + 1):
        print(f"\n===== [{provider_type}] 第 {i}/{times} 次 =====")
        registrar = openai_register.PlatformRegistrar(proxy)
        start = time.time()
        try:
            result = registrar.register(i)
            cost = time.time() - start
            has_token = bool(str(result.get("access_token") or "").strip())
            results.append((has_token, "" if has_token else "无 access_token"))
            print(f"  -> 成功 email={result.get('email')} token={'有' if has_token else '无'} 耗时{cost:.1f}s")
        except Exception as error:
            cost = time.time() - start
            results.append((False, str(error)[:200]))
            print(f"  -> 失败 耗时{cost:.1f}s 原因: {str(error)[:200]}")
        finally:
            registrar.close()

    ok = sum(1 for success, _ in results if success)
    print(f"\n##### [{provider_type}] 汇总: {ok}/{times} 成功 #####")
    for idx, (success, err) in enumerate(results, 1):
        print(f"  第{idx}次: {'成功' if success else '失败: ' + err}")


if __name__ == "__main__":
    ptype = sys.argv[1] if len(sys.argv) > 1 else "dropmail"
    n = int(sys.argv[2]) if len(sys.argv) > 2 else 5
    px = sys.argv[3] if len(sys.argv) > 3 else "http://127.0.0.1:7890"
    probe(ptype, n, px)
