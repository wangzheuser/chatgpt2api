"""代理 URL 模板渲染工具。

某些轮换出口代理把会话 ID 放在用户名里，形如：
    http://baokemng.{uuid}:admin2012@127.0.0.1:9200
每次建连时把 {uuid} 替换成一个新的随机会话 ID，就能让代理服务商为本次
连接分配独立出口 IP，达到"一次注册一个 IP"的效果。

参考 kirox 项目 internal/proxy/runtime.go 的处理方式：
- 同一个 URL 内的多个 {uuid} 必须替换成同一个值（保证代理会话一致）；
- 会话 ID 用去掉短横线的小写 uuid（部分代理商的账号规则不接受短横线）。
"""

from __future__ import annotations

import uuid as _uuid

# 占位符字面量。保持与 kirox 一致，方便两个项目复用同一份代理配置。
UUID_PLACEHOLDER = "{uuid}"


def render_proxy_template(proxy: str) -> str:
    """渲染代理 URL 中的 {uuid} 占位符。

    不含占位符时原样返回，避免对普通代理（如 http://127.0.0.1:7890）产生任何影响。
    """
    text = str(proxy or "").strip()
    if UUID_PLACEHOLDER not in text:
        return text
    session_id = _uuid.uuid4().hex  # 无短横线小写
    return text.replace(UUID_PLACEHOLDER, session_id)


if __name__ == "__main__":
    # 自检：普通代理不变、占位符被替换、同一 URL 多占位符取同值。
    assert render_proxy_template("http://127.0.0.1:7890") == "http://127.0.0.1:7890"
    assert render_proxy_template("") == ""
    rendered = render_proxy_template("http://baokemng.{uuid}:admin2012@127.0.0.1:9200")
    assert "{uuid}" not in rendered and rendered.startswith("http://baokemng.")
    multi = render_proxy_template("http://{uuid}:{uuid}@127.0.0.1:9200")
    user, _, rest = multi[len("http://"):].partition("@")
    left, _, right = user.partition(":")
    assert left == right and len(left) == 32, multi
    print("proxy_template self-check OK")
