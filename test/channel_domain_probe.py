"""逐 (渠道, 域名) 组合测试临时邮箱能否通过 OpenAI 注册。

两步判定（一次跑完同时产出）：
  第一步 code_received：创建该域名邮箱 → OpenAI 真实发码(authorize/register/send_otp)
    → 在 first_step_wait 秒内轮询是否收到验证码。收到即为候选。
  第二步 registered：收到码后继续 validate_otp → create_account → 换 token 成功。

用法：
    python test/channel_domain_probe.py [provider1,provider2,...] [每组合次数] [代理]
不传 provider 列表则测全部零配置渠道。结果同时打印并写入 data/channel_probe_result.json。

作者：wangqiupei
"""

from __future__ import annotations

import json
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import warnings

warnings.filterwarnings("ignore")

from services.register import mail_provider, openai_register  # noqa: E402

ALL_PROVIDERS = ["dropmail", "openinbox"]

FIRST_STEP_WAIT = 35  # 第一步收码等待上限（秒）；OpenAI 验证码通常 5-10s 内到


def _mail_conf(proxy: str) -> dict:
    return {"request_timeout": 30, "wait_timeout": FIRST_STEP_WAIT, "wait_interval": 3, "user_agent": openai_register.user_agent, "proxy": proxy}


def _enumerate_domains(provider_type: str, proxy: str) -> list[str]:
    """枚举该渠道的全部候选域名；服务端分配的渠道返回 [""] 作为单一单元。"""
    try:
        conf = mail_provider._config(_mail_conf(proxy))
        provider = mail_provider._create_provider({"providers": [{"type": provider_type, "enable": True}], **_mail_conf(proxy)}, provider_type, f"{provider_type}#1")
        try:
            domains = provider.list_candidate_domains()
        finally:
            provider.close()
        return [d for d in domains if d] or [""]
    except Exception as error:
        print(f"  [枚举域名失败 {provider_type}] {error}")
        return [""]


def _probe_once(provider_type: str, domain: str, proxy: str, index: int) -> dict:
    """跑一次：创建(指定域名)邮箱 → 发码 → 收码(第一步) → 完整注册(第二步)。"""
    entry = {"type": provider_type, "enable": True}
    if domain:
        entry["domain"] = [domain]
    openai_register.config["proxy"] = proxy
    openai_register.config["mail"] = {**_mail_conf(proxy), "api_use_register_proxy": True, "providers": [entry]}

    result = {"mail_created": False, "code_received": False, "registered": False, "address": "", "error": ""}
    registrar = openai_register.PlatformRegistrar(proxy)
    try:
        mailbox = openai_register.create_mailbox(register_proxy=proxy)
        email = str(mailbox.get("address") or "").strip()
        if not email:
            raise RuntimeError("邮箱服务未返回 address")
        result["mail_created"] = True
        result["address"] = email
        first, last = openai_register._random_name()
        registrar._platform_authorize(email, index)
        # 与主注册流程保持一致：走 passwordless signup，authorize 后直接发码，不再调已废弃的 user/register
        if not registrar.passwordless_signup:
            registrar._start_passwordless_signup(index)
        code = openai_register.wait_for_code(mailbox, register_proxy=proxy)
        if not code:
            raise RuntimeError("第一步：等待验证码超时（渠道未收到 OpenAI 验证码）")
        result["code_received"] = True
        # 第二步：完整注册
        registrar._validate_otp(code, index)
        registrar._create_account(f"{first} {last}", openai_register._random_birthdate(), index)
        tokens = registrar._exchange_registered_tokens(index)
        result["registered"] = bool(str(tokens.get("access_token") or "").strip())
    except Exception as error:
        result["error"] = str(error)[:200]
    finally:
        registrar.close()
    return result


def probe(providers: list[str], times: int, proxy: str) -> dict:
    summary: dict = {}
    for ptype in providers:
        domains = _enumerate_domains(ptype, proxy)
        print(f"\n########## {ptype} （{len(domains)} 个候选域名）##########")
        for domain in domains:
            label = f"{ptype}@{domain}" if domain else f"{ptype}(服务端域名)"
            created = code = reg = 0
            last_err = ""
            for i in range(1, times + 1):
                r = _probe_once(ptype, domain, proxy, i)
                created += int(r["mail_created"])
                code += int(r["code_received"])
                reg += int(r["registered"])
                if r["error"]:
                    last_err = r["error"]
                mark = "注册成功" if r["registered"] else ("收到码" if r["code_received"] else ("建箱ok" if r["mail_created"] else "建箱失败"))
                print(f"  [{label}] 第{i}/{times}: {mark} addr={r['address']}" + (f" err={r['error']}" if r["error"] and not r["code_received"] else ""))
            summary[label] = {"provider": ptype, "domain": domain, "times": times, "mail_created": created, "code_received": code, "registered": reg, "last_error": last_err}
            print(f"  => [{label}] 建箱{created}/{times} 收码{code}/{times} 注册{reg}/{times}")
    return summary


if __name__ == "__main__":
    provs = sys.argv[1].split(",") if len(sys.argv) > 1 and sys.argv[1] else ALL_PROVIDERS
    n = int(sys.argv[2]) if len(sys.argv) > 2 else 5
    px = sys.argv[3] if len(sys.argv) > 3 else "http://127.0.0.1:7890"
    out = probe(provs, n, px)
    result_file = Path(__file__).resolve().parents[1] / "data" / "channel_probe_result.json"
    result_file.write_text(json.dumps(out, ensure_ascii=False, indent=2), encoding="utf-8")
    print("\n================ 汇总 ================")
    for label, s in out.items():
        print(f"{label:34} 建箱{s['mail_created']}/{s['times']} 收码{s['code_received']}/{s['times']} 注册{s['registered']}/{s['times']}")
    print(f"\n结果已写入 {result_file}")
