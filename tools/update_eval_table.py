#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
从 sweep JSON 更新八维总表：把某模型的最佳 effort 行写入 /data/codes/xuanji/docs/reports/model-eval-八维总表.md
用法:
  python3 update_eval_table.py --json docs/reports/hy4-preview-benchmark.json
只保留每模型最佳 effort 一行；最佳=均分最高、同分取耗时更低。
"""
import argparse, json, os, re, sys


def best_effort(summary):
    rows = [(e, s) for e, s in summary.items() if "error" not in s]
    return max(rows, key=lambda kv: (kv[1]["mean_correct"], -kv[1]["mean_duration_s"]))[0]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--json", required=True)
    ap.add_argument("--table", default="/data/codes/xuanji/docs/reports/model-eval-八维总表.md")
    ap.add_argument("--recommend", default="⏳ 待定", help="推荐列文本")
    ap.add_argument("--date", default="", help="评测记录日期（YYYY-MM-DD），留空则不改记录表")
    ap.add_argument("--bank", default="57 题", help="题库说明")
    args = ap.parse_args()

    d = json.load(open(args.json, encoding="utf-8"))
    model = d["model"]
    bench = d["bench"]
    s = d["summary"]
    be = best_effort(s)
    b = s[be]
    cats = b["categories"]
    kb = ["中医·知识", "养生·知识", "玄学·知识", "生活·知识"]
    iq = ["逻辑·智商", "数学·智商", "代码·智商"]
    ksum = sum(cats[c]["mean_correct"] for c in kb)
    ktot = sum(cats[c]["total"] for c in kb)
    isum = sum(cats[c]["mean_correct"] for c in iq)
    itot = sum(cats[c]["total"] for c in iq)
    bm = b.get("bench_mean_correct")

    print(f"model={model} best={be} mean={b['mean_correct']}/{b['total']} ({b['mean_rate']}%)")
    print(f"  bench({bench}) mean={bm}/{b['bench_total']}")
    print(f"  知识 {ksum:.1f}/{ktot} 智商 {isum:.1f}/{itot}")
    for c in kb + iq:
        print(f"   {c}: {cats[c]['mean_correct']:.1f}/{cats[c]['total']}")
    print(f"  各轮: {b['scores']}  波动: {b['spread']}  api_errors={b.get('api_errors')}")
    print(f"  effort 明细: " + "  ".join(
        f"{e}={s[e]['mean_correct']}/{s[e]['total']}({s[e]['mean_rate']}%)"
        for e in d["efforts"] if "error" not in s[e]))

    # ---- 生成总表行 ----
    row = (f"| {model} | {ksum:.0f}/{ktot} ({ksum/ktot*100:.0f}%) | {isum:.0f}/{itot} ({isum/itot*100:.0f}%) | "
           f"{cats['代码·智商']['mean_correct']:.0f}/{cats['代码·智商']['total']} | "
           f"{cats['逻辑·智商']['mean_correct']:.0f}/{cats['逻辑·智商']['total']} | "
           f"{cats['数学·智商']['mean_correct']:.0f}/{cats['数学·智商']['total']} | — | "
           f"{be} | {args.recommend} |")
    print("\n建议总表行:\n" + row)

    text = open(args.table, encoding="utf-8").read()
    # 若已有该模型行（占位或旧值），替换；否则插入到表尾前
    pat = re.compile(r"^\| " + re.escape(model) + r" \| .*$\n?", re.M)
    if pat.search(text):
        text = pat.sub(row + "\n", text, count=1)
        print(f"[ok] 已替换总表中 {model} 行")
    else:
        anchor = "\n## 评测记录"
        if anchor in text:
            text = text.replace(anchor, row + "\n" + anchor, 1)
            print(f"[ok] 已追加总表中 {model} 行")
        else:
            print(f"[warn] 总表未找到 {model} 行，也未找到锚点，未修改")
            return

    # 更新评测记录表：删掉同名模型旧记录行，追加新记录
    if args.date:
        rec_pat = re.compile(r"^\| \d{4}-\d{2}-\d{2} \| " + re.escape(model) + r" \| .*$\n?", re.M)
        text = rec_pat.sub("", text)
        rec_row = (f"| {args.date} | {model} | {args.bank} | {be} | "
                   f"`docs/reports/{os.path.basename(args.json)}` |\n")
        rec_anchor = "\n## 更新规则"
        if rec_anchor in text:
            text = text.replace(rec_anchor, rec_row + rec_anchor, 1)
            print(f"[ok] 已更新评测记录（{args.date}）")
        else:
            print("[warn] 未找到评测记录锚点")

    open(args.table, "w", encoding="utf-8").write(text)
    print(f"[ok] 已写入 {args.table}")


if __name__ == "__main__":
    main()
