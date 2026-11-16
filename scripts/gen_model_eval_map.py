#!/usr/bin/env python3
"""生成/更新璇玑管理界面的 MODEL_EVAL map（路由规则页「评测」列数据源）。

数据来源：/data/codes/model-eval/reports/*-benchmark.json（138 题八维题库）。
规则：每模型取「得分最高 → 思考token最少」的档位作为最佳档。
落点：cmd/server/web/admin_vue.html 中 MODEL_EVAL_BEGIN / MODEL_EVAL_END 之间。

用法: python3 gen_model_eval_map.py [--dry]
改完需重编二进制（web 已 go:embed）: ./deploy.sh
"""
import glob, json, os, re, sys, argparse

REPORTS = "/data/codes/model-eval/reports"
HTML = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "cmd", "server", "web", "admin_vue.html")
HTML = os.path.normpath(HTML)
BEGIN = "// === MODEL_EVAL_BEGIN"
END = "// === MODEL_EVAL_END ==="


def collect():
    best = {}
    for p in glob.glob(os.path.join(REPORTS, "*benchmark.json")):
        b = os.path.basename(p)
        if "smoke" in b:
            continue
        m = re.match(r'^(.+?)-(none|minimal|low|medium|high|xhigh|max)(?:-[a-z0-9]+)*-benchmark\.json$', b)
        if not m:
            continue
        model, eff = m.group(1), m.group(2)
        try:
            d = json.load(open(p, encoding="utf-8"))
        except Exception:
            continue
        n = sum(v[1] for v in d.get("scores", {}).values())
        if n != 138:
            continue
        tk = d.get("tokens", {}) or {}
        key = (d.get("total", 0), -tk.get("reasoning", 0))
        if model not in best or key > best[model][0]:
            best[model] = (key, d.get("total", 0), eff)
    # 按得分降序输出，便于人读
    return sorted(best.items(), key=lambda kv: (-kv[1][1], kv[0]))


def render(entries):
    lines = [BEGIN + "（自动生成，勿手改；用 scripts/gen_model_eval_map.sh 更新）===",
             "        // 模型评测数据（138 题八维题库，来源 /data/codes/model-eval）。",
             "        // 独立 map：思考等级列表里出现但此处没有的模型 → 显示 “—”，不影响其它行。",
             "        // key = 模型名；value = { score 得分, total 题量, effort 最佳思考等级 }。",
             "        var MODEL_EVAL = {"]
    rows = []
    for model, (_, tot, eff) in entries:
        rows.append('  "%s": { score: %d, total: 138, effort: "%s" }' % (model, tot, eff))
    lines.append(",\n".join(rows))
    lines.append("};")
    lines.append(END)
    return "\n".join(lines)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dry", action="store_true", help="只打印，不写文件")
    a = ap.parse_args()
    entries = collect()
    block = render(entries)
    if a.dry:
        print(block)
        print("\n# %d 个模型" % len(entries))
        return
    html = open(HTML, encoding="utf-8").read()
    pat = re.compile(re.escape(BEGIN) + r".*?" + re.escape(END), re.S)
    if not pat.search(html):
        raise SystemExit("未找到 MODEL_EVAL 标记块，请先在 admin_vue.html 插入占位")
    new = pat.sub(lambda _: block, html, count=1)
    open(HTML, "w", encoding="utf-8").write(new)
    print("已更新 %s（%d 个模型）" % (HTML, len(entries)))


if __name__ == "__main__":
    main()
