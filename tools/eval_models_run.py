#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
多轮评测汇总：每模型跑 N 轮，输出均值/波动，给最终结论。
用法:
  python3 eval_models_run.py --base http://127.0.0.1:3002 --key sk-xxx \
      --models sensenova-6.7-flash-lite --bench deepseek-v4-flash --rounds 3
"""
import argparse, json, statistics, subprocess, sys, tempfile, os, re

def run_once(base, key, models_csv, bench, workers, direct=False, effort="default"):
    """跑一次 eval_models.py，解析 stdout 里的维度得分"""
    cmd = [sys.executable, os.path.join(os.path.dirname(__file__), "eval_models.py"),
           "--base", base, "--key", key, "--models", models_csv, "--bench", bench,
           "--workers", str(workers)]
    if direct:
        cmd.append("--direct")
    if effort != "default":
        cmd += ["--effort", effort]
    r = subprocess.run(cmd, capture_output=True, text=True, timeout=600)
    out = r.stdout
    if r.returncode != 0:
        return None, out + r.stderr
    # 解析报告表
    parsed = {"models": {}, "diffs": []}
    lines = out.splitlines()
    for ln in lines:
        # 找 "总计" 行:  "总计              57     51/57 (89.5%)     50/57 (87.7%)"
        m2 = re.search(r"^总计\s+(\d+)\s+(.*)$", ln)
        if m2:
            # 分解各模型
            parts = re.findall(r"(\d+)/(\d+) \(([\d.]+)%\)", m2.group(2))
            # 维度行：知识/智商
        m3 = re.search(r"^(知识|智商)\s+(\d+)\s+(.*)$", ln)
        if m3:
            parts = re.findall(r"(\d+)/(\d+) \(([\d.]+)%\)", m3.group(3))
            if m3.group(1) not in parsed:
                parsed[m3.group(1)] = []
            parsed[m3.group(1)].append([int(a) for a, _, _ in parts])
    # 从 "总得分" 行提取每个模型总分
    model_order = []
    in_eval = None
    for ln in lines:
        m = re.search(r"========== 评测模型: (.+) ==========", ln)
        if m:
            in_eval = m.group(1)
            continue
        m = re.search(r"^\s*总得分: (\d+)/(\d+)", ln)
        if m and in_eval:
            parsed.setdefault("models", {})[in_eval] = {"correct": int(m.group(1)), "total": int(m.group(2))}
            in_eval = None
    return parsed, out

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:3002")
    ap.add_argument("--key", required=True)
    ap.add_argument("--models", default="sensenova-6.7-flash-lite")
    ap.add_argument("--bench", default="deepseek-v4-flash")
    ap.add_argument("--rounds", type=int, default=3)
    ap.add_argument("--workers", type=int, default=6)
    ap.add_argument("--direct", action="store_true", help="直连上游（绕过璇玑路由）")
    ap.add_argument("--effort", default="default", help="思考深度: default/none/low/medium/high/max")
    args = ap.parse_args()

    models = [m.strip() for m in args.models.split(",")]
    if args.bench not in models:
        models.insert(0, args.bench)

    # rounds × models 记录
    scores = {m: [] for m in models}
    for rnd in range(1, args.rounds + 1):
        print(f"\n===== 第 {rnd}/{args.rounds} 轮 =====")
        parsed, out = run_once(args.base, args.key, ",".join(models), args.bench, args.workers, direct=args.direct, effort=args.effort)
        if parsed is None:
            print("运行失败：", out[-500:])
            continue
        for m in models:
            if m in parsed.get("models", {}):
                d = parsed["models"][m]
                scores[m].append(d["correct"])
                print(f"  {m}: {d['correct']}/{d['total']}")
            else:
                print(f"  {m}: 未解析到得分（可能全部 API 失败）")

    print("\n" + "=" * 60)
    print("多轮汇总（正确题数，满分 57）")
    print("=" * 60)
    hdr = f"{'模型':<28}{'各轮':<24}{'均值':>8}{'波动':>8}"
    print(hdr)
    print("-" * len(hdr))
    for m in models:
        vals = scores[m]
        if not vals:
            print(f"{m:<28}{'无数据':<24}")
            continue
        mean = sum(vals) / len(vals)
        spread = max(vals) - min(vals)
        print(f"{m:<28}{str(vals):<24}{mean:>7.1f}{spread:>7}")

    if len(models) >= 2:
        a, b = models[0], models[1]
        va, vb = scores[a], scores[b]
        if va and vb:
            ma, mb = sum(va)/len(va), sum(vb)/len(vb)
            diff = ma - mb
            total = 57
            print("\n结论：")
            print(f"  {a} 均值 {ma:.1f}/{total} ({ma/total*100:.1f}%)")
            print(f"  {b} 均值 {mb:.1f}/{total} ({mb/total*100:.1f}%)")
            if abs(diff) < 2:
                print(f"  差距 {diff:+.1f} 分（<2 分）→ 两模型能力基本同级，差异在噪声范围。")
            elif diff > 0:
                print(f"  差距 {diff:+.1f} 分 → {a} 略强，但需更多轮次确认。")
            else:
                print(f"  差距 {diff:+.1f} 分 → {b} 略强，但需更多轮次确认。")

if __name__ == "__main__":
    main()
