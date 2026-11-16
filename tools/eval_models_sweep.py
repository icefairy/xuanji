#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
思考档位扫描评测：对同一模型逐档(如 low/medium/high/max) × N 轮跑 eval_models.py，
汇总各档均值/波动，写出合并 JSON（供八维总表使用）。

用法:
  python3 eval_models_sweep.py --base http://127.0.0.1:3001 --key sk-xxx \
      --model hy4-preview --bench deepseek-v4.1-flash \
      --efforts low,medium,high,max --rounds 3 --workers 6 --max-tokens 8192 \
      --gap 10 --out /data/codes/xuanji/docs/reports/hy4-preview-benchmark.json

说明:
  - 每轮调用 eval_models.py --json-out 落单轮原始 JSON（含每题明细）
  - 档位之间 sleep --gap 秒，避免上游 429
  - 合并结果含：各档每轮得分/均值/波动/分维度均值/逐题稳定率
"""
import argparse, json, os, statistics, subprocess, sys, time

HERE = os.path.dirname(os.path.abspath(__file__))
EVAL = os.path.join(HERE, "eval_models.py")


def run_once(args, effort, rnd, raw_path):
    cmd = [sys.executable, EVAL, "--base", args.base, "--key", args.key,
           "--models", args.model, "--bench", args.bench,
           "--workers", str(args.workers), "--max-tokens", str(args.max_tokens),
           "--timeout", str(args.req_timeout),
           "--effort", effort, "--json-out", raw_path]
    t0 = time.time()
    r = subprocess.run(cmd, capture_output=True, text=True, timeout=args.timeout)
    dt = time.time() - t0
    if r.returncode != 0 or not os.path.exists(raw_path):
        print(f"  [第{rnd}轮 effort={effort}] 失败 rc={r.returncode} {dt:.0f}s")
        print((r.stdout + r.stderr)[-800:])
        return None
    with open(raw_path, encoding="utf-8") as f:
        return json.load(f)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:3001")
    ap.add_argument("--key", required=True)
    ap.add_argument("--model", default="hy4-preview")
    ap.add_argument("--bench", default="deepseek-v4.1-flash")
    ap.add_argument("--efforts", default="low,medium,high,max")
    ap.add_argument("--rounds", type=int, default=3)
    ap.add_argument("--workers", type=int, default=6)
    ap.add_argument("--max-tokens", type=int, default=8192)
    ap.add_argument("--req-timeout", type=int, default=300, help="单次请求超时秒数（高思考档需要）")
    ap.add_argument("--gap", type=float, default=10.0, help="两次运行之间间隔秒数（避免 429）")
    ap.add_argument("--timeout", type=int, default=3600, help="单次 eval_models.py 子进程超时秒数")
    ap.add_argument("--rawdir", default="/tmp/hy4-sweep")
    ap.add_argument("--out", default="/data/codes/xuanji/docs/reports/hy4-preview-benchmark.json")
    args = ap.parse_args()

    efforts = [e.strip() for e in args.efforts.split(",") if e.strip()]
    os.makedirs(args.rawdir, exist_ok=True)
    os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)

    combined = {
        "model": args.model,
        "bench": args.bench,
        "base": args.base,
        "workers": args.workers,
        "max_tokens": args.max_tokens,
        "rounds": args.rounds,
        "efforts": efforts,
        "generated_at": time.strftime("%Y-%m-%d %H:%M:%S"),
        "per_round": {},
        "summary": {},
    }

    first = True
    for effort in efforts:
        combined["per_round"][effort] = []
        for rnd in range(1, args.rounds + 1):
            if not first:
                time.sleep(args.gap)
            first = False
            raw_path = os.path.join(args.rawdir, f"{args.model}-{effort}-r{rnd}.json")
            print(f"\n===== effort={effort} 第 {rnd}/{args.rounds} 轮 =====", flush=True)
            data = run_once(args, effort, rnd, raw_path)
            if data is None:
                combined["per_round"][effort].append({"round": rnd, "error": "run_failed"})
                continue
            m = data["models"][args.model]
            b = data["models"][args.bench]
            rec = {"round": rnd,
                   "model": {"correct": m["correct"], "total": m["total"], "rate": m["rate"],
                             "duration_s": m["duration_s"], "usage": m.get("usage", {})},
                   "bench": {"correct": b["correct"], "total": b["total"], "rate": b["rate"],
                             "duration_s": b["duration_s"], "usage": b.get("usage", {})},
                   "raw_json": raw_path,
                   "api_errors": sum(1 for v in m["categories"].values()
                                     for it in v["items"] if str(it["answer"]).startswith("__ERROR__")),
                   "categories": {c: {"correct": v["correct"], "total": v["total"]}
                                  for c, v in m["categories"].items()},
                   "items": {c: [{"q": it["q"], "answer": it["answer"],
                                  "score": it["score"], "detail": it["detail"]}
                                 for it in v["items"]]
                             for c, v in m["categories"].items()},
                   "bench_categories": {c: {"correct": v["correct"], "total": v["total"]}
                                        for c, v in b["categories"].items()}}
            combined["per_round"][effort].append(rec)
            print(f"  {args.model}: {m['correct']}/{m['total']} ({m['rate']}%)  "
                  f"{args.bench}: {b['correct']}/{b['total']} ({b['rate']}%)  {m['duration_s']}s", flush=True)
            # 增量落盘：中断也不丢已完成结果
            with open(args.out, "w", encoding="utf-8") as f:
                json.dump(combined, f, ensure_ascii=False, indent=2)

    # ---- 汇总 ----
    cats = None
    for effort in efforts:
        recs = [r for r in combined["per_round"][effort] if "error" not in r]
        if not recs:
            combined["summary"][effort] = {"error": "no_data"}
            continue
        scores = [r["model"]["correct"] for r in recs]
        bscores = [r["bench"]["correct"] for r in recs]
        total = recs[0]["model"]["total"]
        btotal = recs[0]["bench"]["total"]
        cats = list(recs[0]["categories"].keys())
        cat_mean = {}
        for c in cats:
            cat_mean[c] = {
                "mean_correct": round(statistics.mean(r["categories"][c]["correct"] for r in recs), 2),
                "total": recs[0]["categories"][c]["total"],
            }
        # 逐题稳定率（各轮都对的题数 / 总题数）
        stable = {}
        for c in cats:
            n = len(recs[0]["items"][c])
            all_ok = 0
            for i in range(n):
                if all(r["items"][c][i]["score"] == 1 for r in recs):
                    all_ok += 1
            stable[c] = {"all_rounds_ok": all_ok, "total": n}
        combined["summary"][effort] = {
            "rounds": len(recs),
            "scores": scores,
            "mean_correct": round(statistics.mean(scores), 2),
            "spread": max(scores) - min(scores),
            "total": total,
            "mean_rate": round(statistics.mean(scores) / total * 100, 1),
            "mean_duration_s": round(statistics.mean(r["model"]["duration_s"] for r in recs), 1),
            "api_errors": sum(r.get("api_errors", 0) for r in recs),
            "usage_mean": {
                k: round(statistics.mean(r["model"].get("usage", {}).get(k, 0) for r in recs), 1)
                for k in ("requests", "prompt_tokens", "completion_tokens", "thinking_tokens")
            },
            "bench_scores": bscores,
            "bench_mean_correct": round(statistics.mean(bscores), 2),
            "bench_total": btotal,
            "categories": cat_mean,
            "stable": stable,
        }

    with open(args.out, "w", encoding="utf-8") as f:
        json.dump(combined, f, ensure_ascii=False, indent=2)

    print("\n" + "=" * 72)
    print(f"档位扫描汇总 —— {combined['model']}（满分 {combined['summary'][efforts[0]].get('total', '?')}）")
    print("=" * 72)
    print(f"{'effort':<10}{'各轮':<20}{'均值':>8}{'波动':>6}{'得分率':>9}{'耗时s':>8}{'基准均值':>10}{'thinkTok':>10}")
    print("-" * 82)
    for effort in efforts:
        s = combined["summary"][effort]
        if "error" in s:
            print(f"{effort:<10}{'无数据':<20}")
            continue
        print(f"{effort:<10}{str(s['scores']):<20}{s['mean_correct']:>8}{s['spread']:>6}"
              f"{s['mean_rate']:>8}%{s['mean_duration_s']:>8}{s['bench_mean_correct']:>10}"
              f"{s['usage_mean']['thinking_tokens']:>10.0f}")
    best = max(efforts, key=lambda e: (combined["summary"][e].get("mean_correct", -1),
                                       -combined["summary"][e].get("mean_duration_s", 1e9)))
    print(f"\n最佳档位: {best}  {json.dumps(combined['summary'][best].get('scores'), ensure_ascii=False)}")
    print(f"[JSON] 已写入 {args.out}")


if __name__ == "__main__":
    main()
