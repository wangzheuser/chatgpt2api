"""新移植的公共临时邮箱渠道的离线自检（不联网）。

只校验纯本地逻辑：DropMail token 生成算法、代理 {uuid} 渲染、以及各新渠道
能被工厂正确实例化。真实收发能力由 test/channel_register_probe.py 联网验证。

作者：wangqiupei
"""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from services.register import mail_provider as m
from utils.proxy_template import render_proxy_template


def _conf() -> dict:
    return {"request_timeout": 10, "wait_timeout": 5, "wait_interval": 1, "user_agent": "test-agent", "proxy": ""}


def test_proxy_template_renders_uuid_placeholder() -> None:
    assert render_proxy_template("http://127.0.0.1:7890") == "http://127.0.0.1:7890"
    rendered = render_proxy_template("http://baokemng.{uuid}:pw@127.0.0.1:9200")
    assert "{uuid}" not in rendered and rendered.startswith("http://baokemng.")
    # 同一 URL 内多个 {uuid} 必须同值
    multi = render_proxy_template("http://{uuid}:{uuid}@h:1")
    user = multi[len("http://"):].split("@")[0]
    left, right = user.split(":")
    assert left == right and len(left) == 32


def test_dropmail_token_algorithm() -> None:
    token = m.DropMailProvider._make_token()
    # 形如 website_<日期+16随机>_<hex hash>
    assert token.startswith("website_")
    parts = token.split("_")
    assert len(parts) == 3 and len(parts[1]) == 24 and int(parts[2], 16) >= 0


def test_factory_instantiates_new_channels() -> None:
    conf = _conf()
    for ptype, cls in [
        ("dropmail", m.DropMailProvider),
        ("openinbox", m.OpenInboxProvider),
    ]:
        provider = m._create_provider(
            {"providers": [{"type": ptype, "enable": True}], "request_timeout": 10, "wait_timeout": 5, "wait_interval": 1},
            ptype,
            f"{ptype}#1",
        )
        assert isinstance(provider, cls) and provider.name == ptype
        provider.close()


if __name__ == "__main__":
    test_proxy_template_renders_uuid_placeholder()
    test_dropmail_token_algorithm()
    test_factory_instantiates_new_channels()
    print("all offline checks passed")
