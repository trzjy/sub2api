#!/usr/bin/env python3
"""从 backend/internal/server/routes/*.go 生成路由索引。

用法（在仓库根或任意目录执行）：

    python3 skills/sub2api-api-atlas/scripts/gen-route-atlas.py [--check]

输出：
    skills/sub2api-api-atlas/references/routes-admin.md
    skills/sub2api-api-atlas/references/routes-gateway-user-auth.md

设计要点：
  1. 只读源码，不联网、不连库、不需要编译。
  2. 前缀按**行序**解析：同名 group 变量在不同作用域里会被重新赋值
     （例如 `users := admin.Group("/users")` 与 `users := affiliates.Group("/users")`
     同文件共存），必须用每行生效的定义，不能全文件取一次。
  3. 每条记录带 handler 符号与 file:line，便于直接跳回实现读请求体。
  4. --check 模式只比对不写盘，退出码非 0 表示索引与源码不一致（可挂进 CI/钩子）。
"""

import os
import re
import sys
from collections import OrderedDict

# realpath 必须解软链：skill 通过 ~/.claude/skills/<name> 软链加载，__file__
# 会变成 /home/zjy/.claude/skills/.../scripts/xxx.py，直接往上取三级会落到
# /home/zjy/.claude 而不是仓库根。
REPO = os.path.abspath(
    os.path.join(os.path.dirname(os.path.realpath(__file__)), "..", "..", "..")
)
ROUTES_DIR = os.path.join(REPO, "backend", "internal", "server", "routes")
OUT_DIR = os.path.join(REPO, "skills", "sub2api-api-atlas", "references")

VERBS = ("GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS")

# 直接挂在 engine 或 router 上的组前缀（源码里 inline 声明，没有可解析的父 group）。
# v1 在 router.go 里声明为 `v1 := r.Group("/api/v1")`，auth/user/payment/admin
# 等文件都直接引用它，故在这里登记根前缀。
ENGINE_ROOTS = {
    "r": "",
    "router": "",
    "engine": "",
    "v1": "/api/v1",
}

# 每个路由文件归属的鉴权域，写在索引表头，避免使用者张冠李戴。
SCOPE = {
    "admin.go": "adminAuth（管理员 API Key 或管理员 JWT）+ 审计 + 合规守卫",
    "user.go": "jwtAuth（登录用户）",
    "auth.go": "公开端点与 jwtAuth 混合，逐条以源码为准",
    "gateway.go": "apiKeyAuth（用户 API Key）+ 平台/分组中间件",
    "payment.go": "用户 jwtAuth / 公开 webhook / 管理端 adminAuth 混合",
    "model_plaza.go": "optionalJWTAuth",
    "common.go": "无面板鉴权（健康检查、内部服务间令牌）",
    "xgj_supply.go": "六段验签中间件（闲管家货源模式）",
    "页面内容（handler 包内注册）": "jwtAuth / 无鉴权（图片）/ adminAuth 混合",
    "安装向导（仅未初始化实例）": "setupGuard（仅 NeedsSetup 为真时注册）",
}

GROUP_RE = re.compile(r'(\w+)\s*:?=\s*(\w+)\.Group\("([^"]*)"')
ROUTE_RE = re.compile(
    r'(?<![\w.])(\w+)\.(' + "|".join(VERBS) + r')\("([^"]*)"\s*,(.*)$'
)

# routes/ 之外还有登记路由的地方：这些文件的注册点不能漏，否则索引会系统性
# 少一块（页面内容接口就在 handler 包里注册，不在 routes/）。
# 形如 {相对仓库根的路径: (展示名, 该文件里 group 的根前缀, 鉴权说明)}。
# 这些文件里的 group 都从函数参数 v1 派生（v1 := r.Group("/api/v1")），
# 已登记在 ENGINE_ROOTS。
EXTRA_SOURCES = {
    "backend/internal/handler/page_handler.go": "页面内容（handler 包内注册）",
    # 首次安装向导，只在 setup.NeedsSetup() 为真时由 cmd/server/main.go 注册；
    # 已初始化的生产实例上这几条不存在，别把它当常规端点。
    "backend/internal/setup/handler.go": "安装向导（仅未初始化实例）",
}


def source_files():
    return sorted(
        f for f in os.listdir(ROUTES_DIR)
        if f.endswith(".go") and not f.endswith("_test.go")
    )


def resolve(prefixes, var, depth=0):
    """从当前生效的 group 定义表里解析某个 group 变量的完整前缀。"""
    if depth > 10:
        return ""
    if var in prefixes:
        base, seg = prefixes[var]
        return resolve(prefixes, base, depth + 1) + seg
    if var in ENGINE_ROOTS:
        return ENGINE_ROOTS[var]
    return ""  # 未知（如局部 helper 变量），由调用方兜底


def split_args(text):
    """按顶层逗号切分实参，忽略括号/花括号/方括号/字符串内部的逗号。"""
    parts, depth, buf, in_str, esc = [], 0, [], None, False
    for ch in text:
        if in_str:
            buf.append(ch)
            if esc:
                esc = False
            elif ch == "\\":
                esc = True
            elif ch == in_str:
                in_str = None
            continue
        if ch in "\"'`":
            in_str = ch
            buf.append(ch)
        elif ch in "([{":
            depth += 1
            buf.append(ch)
        elif ch in ")]}":
            depth -= 1
            buf.append(ch)
        elif ch == "," and depth == 0:
            parts.append("".join(buf).strip())
            buf = []
        else:
            buf.append(ch)
    if buf:
        parts.append("".join(buf).strip())
    return [p for p in parts if p]


def last_handler(arglist):
    """取注册调用最后一个顶层实参作为 handler 符号。

    形如 `gin.HandlerFunc(apiKeyAuth), requireGroup, h.Gateway.Messages`
    → `h.Gateway.Messages`；handler 是内联匿名函数时标注 `(内联函数)`。
    """
    args = split_args(arglist)
    if not args:
        return ""
    last = args[-1].strip()
    if last.startswith("func("):
        return "(内联函数)"
    return last


def route_call_end(lines, lineno, col):
    """从注册行起向后累积，直到该调用的括号配平，返回 (整段文本, 结束行号)。

    col 定位到注册调用左括号之后（depth 从 1 起算），因此单行调用第一行即配平。
    """
    buf = []
    depth = 1
    i = lineno - 1
    while i < len(lines):
        seg = lines[i][col:] if i == lineno - 1 else lines[i]
        col = 0
        for ch in seg:
            if ch == "(":
                depth += 1
            elif ch == ")":
                depth -= 1
        buf.append(seg)
        if depth <= 0:
            return "\n".join(buf), i + 1
        i += 1
    return "\n".join(buf), i


def parse_helper_registrations(path, prefixes):
    """解析「闭包转发」式注册。

    xgj_supply.go 里先定义
        register := func(path string, fn gin.HandlerFunc) {
            grp.POST(path, fn)
            ...
        }
    再 register("/goofish/open/info", h.SupplyCatalog.PlatformInfo)。
    这里从闭包体里取出实际使用的 verb 与 group 前缀，再按每个 register 调用
    展开路径（保留源码里 path 与 path+"/" 两个变体）。
    """
    src = open(path, encoding="utf-8", errors="replace").read()
    out = []
    for fm in re.finditer(
        r'(\w+)\s*:=\s*func\(\s*\w+\s+string\s*,\s*\w+\s+gin\.HandlerFunc\s*\)\s*\{(.*?)\n\t\}',
        src, re.S,
    ):
        fn_name, body = fm.group(1), fm.group(2)
        inner = re.findall(r'(\w+)\.(' + "|".join(VERBS) + r')\(path', body)
        if not inner:
            continue
        grp_var, verb = inner[0]
        pre = resolve(prefixes, grp_var)
        for cm in re.finditer(
            r'(?<![\w.])' + re.escape(fn_name) + r'\("([^"]*)"\s*,\s*([^)\n]+)\)', src
        ):
            sub, handler = cm.group(1), cm.group(2).strip()
            variants = (sub,) if sub.endswith("/") else (sub, sub + "/")
            for sub_path in variants:
                out.append((pre + sub_path, verb, handler,
                            src[: cm.start()].count("\n") + 1))
    return out


def parse_file(path):
    lines = open(path, encoding="utf-8", errors="replace").read().splitlines()

    prefixes = {}   # var -> (parent_var, segment)，按行序更新，反映当前作用域
    rows = []
    skip_until = 0
    for idx, line in enumerate(lines):
        lineno = idx + 1
        if lineno <= skip_until:
            continue
        for m in GROUP_RE.finditer(line):
            prefixes[m.group(1)] = (m.group(2), m.group(3))
        m = ROUTE_RE.search(line)
        if not m:
            continue
        var, verb, sub = m.group(1), m.group(2), m.group(3)
        call_text, end_line = route_call_end(lines, lineno, m.start(4))
        skip_until = end_line
        pre = resolve(prefixes, var)
        full = pre + sub
        if not full.startswith("/"):
            full = "/" + full
        rows.append((full, verb, last_handler(call_text.rstrip(")")), lineno))

    rows.extend(parse_helper_registrations(path, prefixes))
    return rows


def collect():
    """返回 OrderedDict：展示名 -> (相对仓库根路径, [(path, verb, handler, line)])。"""
    out = OrderedDict()

    def add(label, rel_path):
        rows = parse_file(os.path.join(REPO, rel_path))
        if not rows:
            return
        seen, uniq = set(), []
        for full, verb, handler, line in rows:
            key = (full, verb)
            if key in seen:
                continue
            seen.add(key)
            uniq.append((full, verb, handler, line))
        out[label] = (rel_path, uniq)

    for fname in source_files():
        add(fname, os.path.join("backend", "internal", "server", "routes", fname))
    for rel_path, label in EXTRA_SOURCES.items():
        add(label, rel_path)
    return out


def bucket(rows):
    """按一级路径段（/admin 下取二级）分桶，便于按域查找。"""
    def key_of(p):
        parts = [x for x in p.split("/") if x]
        if parts[:2] == ["api", "v1"]:
            parts = parts[2:]
        if not parts:
            return "(root)"
        if parts[0] == "admin" and len(parts) > 1:
            return "/admin/" + parts[1]
        return "/" + parts[0]

    b = OrderedDict()
    for full, verb, handler, line in rows:
        b.setdefault(key_of(full), []).append((full, verb, handler, line))
    for k in b:
        b[k].sort(key=lambda r: (r[0], r[1]))
    return OrderedDict(sorted(b.items()))


def render_file(label, rel_path, rows):
    parts = [
        f"# 路由索引：{label}",
        "",
        f"- 鉴权域：{SCOPE.get(label, '以源码为准')}",
        f"- 端点数：{len(rows)}",
        f"- 源码：`{rel_path}`",
        "",
        "路径列里的 `:id` / `*subpath` 是 gin 路径参数；handler 列给出方法符号，",
        "需要请求体结构时按「位置」列的 文件:行 跳转，读该 handler 里绑定的 Request struct。",
        "",
    ]
    base = os.path.basename(rel_path)
    for sec, items in bucket(rows).items():
        parts.append(f"## {sec}（{len(items)}）")
        parts.append("")
        parts.append("| 方法 | 路径 | handler | 位置 |")
        parts.append("| --- | --- | --- | --- |")
        for full, verb, handler, line in items:
            parts.append(f"| {verb} | `{full}` | `{handler}` | `{base}:{line}` |")
        parts.append("")
    return "\n".join(parts)


def main():
    check = "--check" in sys.argv
    data = collect()
    os.makedirs(OUT_DIR, exist_ok=True)

    admin_labels = [k for k in data if k == "admin.go"]
    other_labels = [k for k in data if k != "admin.go"]
    targets = [
        ("routes-admin.md", "管理面（/api/v1/admin）", admin_labels),
        ("routes-gateway-user-auth.md", "网关 / 用户面 / 支付 / 通用", other_labels),
    ]

    stale = []
    for out_name, title, labels in targets:
        chunks = [
            "<!-- 由 scripts/gen-route-atlas.py 自动生成，请勿手改。 -->",
            "",
            f"# {title}路由索引",
            "",
            "重新生成：`python3 skills/sub2api-api-atlas/scripts/gen-route-atlas.py`",
            "",
        ]
        for label in labels:
            rel_path, rows = data[label]
            chunks.append(render_file(label, rel_path, rows))
            chunks.append("\n---\n")
        text = "\n".join(chunks)
        dest = os.path.join(OUT_DIR, out_name)
        old = open(dest, encoding="utf-8").read() if os.path.exists(dest) else None
        if check:
            if old != text:
                stale.append(out_name)
        else:
            open(dest, "w", encoding="utf-8").write(text)
            print(f"写入 {dest}（{sum(len(data[k][1]) for k in labels)} 端点）")

    if check and stale:
        print("索引与源码不一致，需重新生成：" + ", ".join(stale))
        return 1
    if check:
        print("索引与源码一致。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
