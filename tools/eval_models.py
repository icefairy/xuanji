#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
璇玑模型评测脚本 — 知识 × 智商 双维度对比
用法:
  python3 eval_models.py --base http://127.0.0.1:3002 --key sk-xxx \
      --models sensenova-6.7-flash-lite --bench deepseek-v4-flash
  python3 eval_models.py --base http://127.0.0.1:3002 --key sk-xxx --models all --bench deepseek-v4-flash

输出: 每个模型按 知识(中医/养生/玄学/生活) + 智商(逻辑/数学/代码) 得分，
      与基准模型 deepseek-v4-flash 对比。
"""
import argparse, concurrent.futures, json, os, re, subprocess, sys, time, urllib.request, urllib.error

# ============ 题库 ============
# 每道题: q=题目, type=judge(判断)/choice(选择)/numeric(数值)/code(代码)
# judge: answer 为 对/错；choice: answer 为 A/B/C/D；numeric: 数值容差 ±0.5；code: 见 run_code
QUESTIONS = {
    "中医·知识": [
        {"q": "中医理论中，\"五行\"指的是金、木、水、火、土五种基本元素及其相互关系。判断对错。", "type": "judge", "answer": "对"},
        {"q": "中医认为\"肝主疏泄\"，主要功能是调节全身气机。判断对错。", "type": "judge", "answer": "对"},
        {"q": "下列哪味中药属于补气药？\nA. 当归  B. 黄芪  C. 熟地黄  D. 川芎", "type": "choice", "answer": "B"},
        {"q": "中医里\"六淫\"指风、寒、暑、湿、燥、火六种外感病邪。判断对错。", "type": "judge", "answer": "对"},
        {"q": "《黄帝内经》分为《素问》和《灵枢》两部分。判断对错。", "type": "judge", "answer": "对"},
        {"q": "中医\"四诊\"是望、闻、问、切。判断对错。", "type": "judge", "answer": "对"},
        {"q": "下列哪项不属于中医\"八纲辨证\"的内容？\nA. 表里  B. 寒热  C. 虚实  D. 阴阳  E. 气血", "type": "choice", "answer": "E"},
        {"q": "中医认为\"肾为先天之本，脾为后天之本\"。判断对错。", "type": "judge", "answer": "对"},
        {"q": "针灸中的\"足三里\"穴位于小腿外侧，是足阳明胃经的穴位。判断对错。", "type": "judge", "answer": "对"},
        {"q": "中药\"四气五味\"中的\"四气\"指寒、热、温、凉。判断对错。", "type": "judge", "answer": "对"},
    ],
    "养生·知识": [
        {"q": "中医认为\"子时\"（23:00-1:00）是胆经当令，此时应进入睡眠状态以养肝胆。判断对错。", "type": "judge", "answer": "对"},
        {"q": "\"饭后百步走，活到九十九\"，饭后立即剧烈运动有助于消化。判断对错。", "type": "judge", "answer": "错"},
        {"q": "中医养生讲究\"春夏养阳，秋冬养阴\"。判断对错。", "type": "judge", "answer": "对"},
        {"q": "下列哪种饮品睡前饮用最不利于睡眠？\nA. 温牛奶  B. 淡蜂蜜水  C. 浓茶  D. 菊花茶", "type": "choice", "answer": "C"},
        {"q": "中医认为\"怒伤肝，喜伤心，思伤脾，忧伤肺，恐伤肾\"。判断对错。", "type": "judge", "answer": "对"},
        {"q": "三伏天进行艾灸（\"冬病夏治\"）的理论基础是顺应阳气最盛之时驱散寒邪。判断对错。", "type": "judge", "answer": "对"},
    ],
    "玄学·知识": [
        {"q": "八字命理中，天干有十个：甲乙丙丁戊己庚辛壬癸。判断对错。", "type": "judge", "answer": "对"},
        {"q": "十二地支的第六位是什么？\nA. 卯  B. 辰  C. 巳  D. 午", "type": "choice", "answer": "C"},
        {"q": "五行相生顺序是：木生火，火生土，土生金，金生水，水生木。判断对错。", "type": "judge", "answer": "对"},
        {"q": "地支六合中，子与什么相合？\nA. 丑  B. 寅  C. 卯  D. 辰", "type": "choice", "answer": "A"},
        {"q": "八字中\"日主\"指的是日柱的天干，代表命主自身。判断对错。", "type": "judge", "answer": "对"},
        {"q": "五行相克顺序是：木克土，土克水，水克火，火克金，金克木。判断对错。", "type": "judge", "answer": "对"},
        {"q": "十天干中，属于阳干的是甲丙戊庚壬。判断对错。", "type": "judge", "answer": "对"},
        {"q": "2026年是丙午年（天干地支纪年）。判断对错。", "type": "judge", "answer": "对"},
    ],
    "生活·知识": [
        {"q": "人体正常体温（腋下）大约在36.1°C至37.2°C之间。判断对错。", "type": "judge", "answer": "对"},
        {"q": "维生素C在高温烹饪中容易被破坏。判断对错。", "type": "judge", "answer": "对"},
        {"q": "酱油的主要酿造原料是大豆和小麦。判断对错。", "type": "judge", "answer": "对"},
        {"q": "我国标准电压是220伏，频率50赫兹。判断对错。", "type": "judge", "answer": "对"},
        {"q": "日常使用的食盐主要成分是氯化钠。判断对错。", "type": "judge", "answer": "对"},
        {"q": "在室温下，鲜牛奶放置超过4小时容易变质。判断对错。", "type": "judge", "answer": "对"},
    ],
    "逻辑·智商": [
        {"q": "如果所有的A都是B，并且所有的B都是C，那么以下哪项一定正确？\nA. 所有C都是A  B. 所有A都是C  C. 有些C不是A  D. 无法确定", "type": "choice", "answer": "B"},
        {"q": "小明比小红大3岁，小红比小刚大5岁，那么小明比小刚大几岁？直接回答数字。", "type": "numeric", "answer": 8},
        {"q": "一个数列：2, 6, 12, 20, 30, 下一个数是多少？直接回答数字。", "type": "numeric", "answer": 42},
        {"q": "袋子里有3个红球和2个蓝球，随机摸一个，摸到红球的概率是多少？直接回答分数或小数。", "type": "numeric", "answer": 0.6, "tolerance": 0.05},
        {"q": "甲乙丙三人中只有一人说真话。甲说：\"乙在说谎\"；乙说：\"丙在说谎\"；丙说：\"甲和乙都在说谎\"。谁说真话？", "type": "choice", "answer": "C", "options": {"A": "甲", "B": "乙", "C": "丙", "D": "无人"}},
        {"q": "时钟显示3点15分，时针和分针的夹角约多少度？直接回答数字。", "type": "numeric", "answer": 7.5, "tolerance": 0.6},
        {"q": "一个正方体有6个面，切一刀最多能把它分成几块？直接回答数字。", "type": "numeric", "answer": 2},
        {"q": "如果昨天是明天的话就好了，这样今天就周五了。问：句子中的\"今天\"实际上是星期几？\nA. 周三  B. 周四  C. 周五  D. 周日", "type": "choice", "answer": "D"},
        {"q": "1+2+3+...+100 = ? 直接回答数字。", "type": "numeric", "answer": 5050},
        {"q": "三个连续奇数之和是57，最大的那个奇数是多少？直接回答数字。", "type": "numeric", "answer": 21},
        {"q": "甲、乙合作一项工程需12天，甲单独做需20天，乙单独做需多少天？直接回答数字。", "type": "numeric", "answer": 30},
        {"q": "以下哪个推理是有效的？\nA. 所有猫都怕水，X怕水，所以X是猫\nB. 所有猫都怕水，X是猫，所以X怕水\nC. 有些猫不怕水，X是猫，所以X不怕水\nD. 以上都不对", "type": "choice", "answer": "B"},
        {"q": "一间房里有4个人，每人都有1只猫，每只猫有4条腿，房间里共有多少条腿（人和猫）？直接回答数字。", "type": "numeric", "answer": 24},
    ],
    "数学·智商": [
        {"q": "求解：3x + 7 = 22，x = ? 直接回答数字。", "type": "numeric", "answer": 5},
        {"q": "一个圆的半径是7，面积约是多少？（π取3.14）直接回答数字。", "type": "numeric", "answer": 153.86, "tolerance": 1},
        {"q": "2的10次方等于多少？直接回答数字。", "type": "numeric", "answer": 1024},
        {"q": "一个三角形三个内角的度数比是1:2:3，最大的角是多少度？直接回答数字。", "type": "numeric", "answer": 90},
        {"q": "某商品原价200元，打八折后又降价10%，现价多少元？直接回答数字。", "type": "numeric", "answer": 144},
        {"q": "log2(64) = ? 直接回答数字。", "type": "numeric", "answer": 6},
        {"q": "一个等差数列首项3，公差4，第10项是多少？直接回答数字。", "type": "numeric", "answer": 39},
        {"q": "计算：7! (7的阶乘) = ? 直接回答数字。", "type": "numeric", "answer": 5040},
    ],
    "代码·智商": [
        {"q": "写一个Python函数 is_palindrome(s)，判断字符串s是否为回文（忽略大小写和空格），返回布尔值。", "type": "code", "check": "palindrome", "tests": [("racecar", True), ("A man a plan a canal Panama", True), ("hello", False)]},
        {"q": "写一个Python函数 fibonacci(n)，返回斐波那契数列的第n项（n从1开始，第1项=0，第2项=1）。", "type": "code", "check": "fibonacci", "tests": [(1, 0), (2, 1), (10, 34)]},
        {"q": "写一个Python函数 count_vowels(s)，返回字符串中元音字母（aeiou，不分大小写）的数量。", "type": "code", "check": "vowels", "tests": [("hello world", 3), ("AEIOU", 5), ("xyz", 0)]},
        {"q": "写一个Python函数 fizzbuzz(n)，返回1到n的列表：3的倍数输出'Fizz'，5的倍数输出'Buzz'，同时是3和5的倍数输出'FizzBuzz'，其他输出数字本身（数字为整数）。", "type": "code", "check": "fizzbuzz", "tests": [(15, [1,2,'Fizz',4,'Buzz','Fizz',7,8,'Fizz','Buzz',11,'Fizz',13,14,'FizzBuzz'])]},
        {"q": "写一个Python函数 longest_word(s)，返回字符串中最长单词（单词以空格分隔，忽略标点），若有多个返回第一个。", "type": "code", "check": "longest_word", "tests": [("the quick brown fox jumps", "quick"), ("a bb ccc dddd", "dddd"), ("single", "single")]},
        {"q": "写一个Python函数 two_sum(nums, target)，返回两个数下标（从0开始），使得两数之和等于target，若无解返回None。", "type": "code", "check": "twosum", "tests": [(([2,7,11,15], 9), (0,1)), (([3,2,4], 6), (1,2)), (([1,2,3], 7), None)]},
    ],
}

def build_question_prompt(q):
    if q["type"] == "code":
        return ("请只输出可运行的Python代码，不要任何解释、注释或markdown代码块标记。\n\n" + q["q"])
    if q["type"] == "choice" and "options" in q:
        opts = "".join(f"{k}. {v}\n" for k, v in q["options"].items())
        return f"{q['q']}\n{opts}只回答选项字母。"
    if q["type"] == "choice":
        return q["q"] + "\n只回答选项字母（A/B/C/D）。"
    if q["type"] == "numeric":
        return q["q"] + "\n只回答数字，不要任何解释。"
    return q["q"] + "\n只回答：对 或 错。"

# ============ 代码评测执行器 ============
def run_code(q, answer_code):
    """提取代码并运行测试用例。answer_code 可能是纯代码或带 markdown。"""
    code = answer_code
    # 去掉 markdown 代码块
    m = re.search(r"```(?:python)?\s*\n(.*?)```", code, re.S)
    if m:
        code = m.group(1)
    # 去掉可能的解释行：只保留 def 函数定义之后的代码
    # 简单粗暴：找到包含 def 的那一行开始
    lines = code.strip().split("\n")
    start = 0
    for i, ln in enumerate(lines):
        if ln.strip().startswith("def "):
            start = i
            break
    code = "\n".join(lines[start:])
    if not code.strip():
        return False, "无代码"
    # 去掉 trailing 解释
    # 运行测试
    try:
        ns = {}
        exec(code, ns)
        for args, expected in q["tests"]:
            fn_name = "is_palindrome" if q["check"] == "palindrome" else \
                      "fibonacci" if q["check"] == "fibonacci" else \
                      "count_vowels" if q["check"] == "vowels" else \
                      "fizzbuzz" if q["check"] == "fizzbuzz" else \
                      "longest_word" if q["check"] == "longest_word" else "two_sum"
            if fn_name not in ns:
                return False, f"缺少函数 {fn_name}"
            if not isinstance(args, tuple):
                args = (args,)
            got = ns[fn_name](*args)
            if isinstance(expected, tuple):
                if not (isinstance(got, tuple) and len(got) == 2 and got[0] == expected[0] and got[1] == expected[1]):
                    return False, f"{args}: 期望 {expected} 得到 {got}"
            elif got != expected:
                return False, f"{args}: 期望 {expected} 得到 {got}"
        return True, "全部通过"
    except Exception as e:
        return False, f"运行错误: {type(e).__name__}: {e}"

# ============ API 调用 ============
def call_model(base, key, model, prompt, timeout=120, direct=False, effort=None):
    body = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "temperature": 0,
        "max_tokens": 6000,  # 思考模型需要大输出空间，避免 finish_reason=length
    }
    # 思考深度控制：none=关闭思考；low/medium/high/max=思考强度
    # 商汤: output_config.effort；deepseek(opencode): thinking.type + reasoning_effort
    if effort == "none":
        body["thinking"] = {"type": "disabled"}
    elif effort and effort != "default":
        if "sensenova" in model or "商汤" in model:
            body["output_config"] = {"effort": effort}
        else:
            body["reasoning_effort"] = effort
    body = json.dumps(body).encode()
    # direct 模式: base 已是上游地址，直接拼 /chat/completions（若末尾无 /v1 则拼 /v1/chat/completions）
    if direct:
        url = base.rstrip("/")
        if url.endswith("/v1"):
            url += "/chat/completions"
        else:
            url += "/v1/chat/completions"
    else:
        url = base.rstrip("/") + "/v1/chat/completions"
    req = urllib.request.Request(
        url,
        data=body,
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {key}",
                 "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"},
        method="POST",
    )
    for attempt in range(4):
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                data = json.loads(resp.read())
            msg = data.get("choices", [{}])[0].get("message", {})
            content = msg.get("content")
            # 部分上游（商汤）偶发返回 message 无 content（只有 reasoning），重试
            if content is None or content == "":
                if attempt < 3:
                    time.sleep(2)
                    continue
                return "__ERROR__: empty content"
            return content.strip()
        except urllib.error.HTTPError as he:
            # 429 限流：退避重试（商汤免费额度并发敏感）
            if he.code == 429 and attempt < 3:
                time.sleep(3 * (attempt + 1))
                continue
            if attempt == 3:
                return f"__ERROR__: HTTP {he.code}"
            time.sleep(2)
        except Exception as e:
            if attempt == 3:
                return f"__ERROR__: {e}"
            time.sleep(2)
    return "__ERROR__"

# ============ 评分 ============
def normalize_choice(text):
    t = text.strip().upper()
    m = re.search(r"\b([ABCD])\b", t)
    if m:
        return m.group(1)
    # 匹配选项内容
    return None

def normalize_numeric(text):
    t = text.strip().replace(",", "").replace("，", "")
    # 分数 a/b
    fm = re.search(r"(\d+(?:\.\d+)?)\s*/\s*(\d+(?:\.\d+)?)", t)
    if fm:
        try:
            return float(fm.group(1)) / float(fm.group(2))
        except ZeroDivisionError:
            pass
    m = re.search(r"-?\d+(?:\.\d+)?", t)
    if not m:
        return None
    return float(m.group(0))

def score_question(q, answer):
    if answer.startswith("__ERROR__"):
        return 0, answer
    if q["type"] == "judge":
        t = answer.strip()
        if re.search(r"^\s*对\s*$|^\s*正确\s*$", t):
            return (1 if q["answer"] == "对" else 0), "对"
        if re.search(r"^\s*错\s*$|^\s*错误\s*$|^\s*不对\s*$", t):
            return (1 if q["answer"] == "错" else 0), "错"
        return 0, f"无法解析: {answer[:30]}"
    if q["type"] == "choice":
        got = normalize_choice(answer)
        if got:
            return (1 if got == q["answer"] else 0), got
        # 尝试匹配选项内容
        if "options" in q:
            for k, v in q["options"].items():
                if v in answer:
                    return (1 if k == q["answer"] else 0), k
        return 0, f"无法解析: {answer[:30]}"
    if q["type"] == "numeric":
        got = normalize_numeric(answer)
        if got is None:
            return 0, f"无法解析: {answer[:30]}"
        tol = q.get("tolerance", 0.5)
        return (1 if abs(got - q["answer"]) <= tol else 0), f"{got}"
    if q["type"] == "code":
        ok, detail = run_code(q, answer)
        return (1 if ok else 0), detail
    return 0, "unknown"

# ============ 主流程 ============
def eval_model(base, key, model, questions, max_workers=6, verbose=False, direct=False, effort=None):
    results = {cat: {"total": 0, "correct": 0, "items": []} for cat in questions}
    total, correct = 0, 0
    tasks = [(cat, qi, q) for cat, qs in questions.items() for qi, q in enumerate(qs)]
    with concurrent.futures.ThreadPoolExecutor(max_workers=max_workers) as ex:
        futs = {}
        for cat, qi, q in tasks:
            prompt = build_question_prompt(q)
            futs[ex.submit(call_model, base, key, model, prompt, direct=direct, effort=effort)] = (cat, qi, q)
        # 用有序字典收集：answer_by[(cat, qi)] = answer，保证按题号对齐
        answers = {}
        for fut in concurrent.futures.as_completed(futs):
            cat, qi, q = futs[fut]
            answers[(cat, qi)] = fut.result()
        for cat, qi, q in tasks:
            answer = answers[(cat, qi)]
            score, detail = score_question(q, answer)
            results[cat]["total"] += 1
            results[cat]["correct"] += score
            total += 1
            correct += score
            results[cat]["items"].append({"q": q["q"][:50], "answer": answer[:60], "score": score, "detail": detail})
            if verbose:
                mark = "✓" if score else "✗"
                print(f"  [{mark}] {cat} #{qi+1}: {detail}")
    return results, total, correct

def main():
    ap = argparse.ArgumentParser(description="璇玑模型知识×智商评测")
    ap.add_argument("--base", default="http://127.0.0.1:3002", help="璇玑网关地址")
    ap.add_argument("--key", required=True, help="璇玑 API Key (api_tokens 表)")
    ap.add_argument("--models", default="sensenova-6.7-flash-lite", help="待测模型，逗号分隔或 all")
    ap.add_argument("--bench", default="deepseek-v4-flash", help="基准模型")
    ap.add_argument("--direct", action="store_true", help="直连上游（绕过璇玑路由），需 --base 为上游地址 + --key 为上游 key")
    ap.add_argument("--effort", default="default", help="思考深度: default/none/low/medium/high/max（none=关闭思考）")
    ap.add_argument("--workers", type=int, default=6)
    ap.add_argument("--verbose", action="store_true")
    args = ap.parse_args()

    models = ["deepseek-v4-flash", "sensenova-6.7-flash-lite"] if args.models == "all" else \
             [m.strip() for m in args.models.split(",")]
    # 确保基准模型在列表里
    if args.bench not in models:
        models.insert(0, args.bench)

    all_results = {}
    for model in models:
        print(f"\n========== 评测模型: {model} ==========")
        t0 = time.time()
        results, total, correct = eval_model(args.base, args.key, model, QUESTIONS,
                                             max_workers=args.workers, verbose=args.verbose,
                                             direct=args.direct, effort=args.effort)
        dt = time.time() - t0
        all_results[model] = (results, total, correct)
        print(f"  总得分: {correct}/{total} ({correct/total*100:.1f}%)  耗时 {dt:.0f}s")

    # 输出报告
    bench = args.bench
    print("\n" + "=" * 70)
    print("评测报告（对比基准: %s）" % bench)
    print("=" * 70)
    hdr = f"{'维度':<14}{'题数':>4}"
    for m in models:
        hdr += f"{m:>18}"
    print(hdr)
    print("-" * len(hdr))
    cat_names = list(QUESTIONS.keys())
    # 按大类汇总
    big_cats = {"知识": ["中医·知识", "养生·知识", "玄学·知识", "生活·知识"],
                "智商": ["逻辑·智商", "数学·智商", "代码·智商"]}
    for big, subs in big_cats.items():
        row = f"{big:<14}"
        n = sum(len(QUESTIONS[c]) for c in subs)
        row += f"{n:>4}"
        for m in models:
            results, total, correct = all_results[m]
            c = sum(results[c]["correct"] for c in subs)
            t = sum(results[c]["total"] for c in subs)
            row += f"{c}/{t} ({c/t*100 if t else 0:.0f}%)".rjust(18)
        print(row)
    print("-" * len(hdr))
    for cat in cat_names:
        row = f"  {cat:<12}"
        row += f"{len(QUESTIONS[cat]):>4}"
        for m in models:
            results, total, correct = all_results[m]
            c = results[cat]["correct"]; t = results[cat]["total"]
            row += f"{c}/{t}".rjust(18)
        print(row)
    print("-" * len(hdr))
    row = f"{'总计':<14}{sum(len(v) for v in QUESTIONS.values()):>4}"
    for m in models:
        results, total, correct = all_results[m]
        row += f"{correct}/{total} ({correct/total*100:.1f}%)".rjust(18)
    print(row)

    # 差题明细（基准对而待测错 或 反之）
    print("\n" + "=" * 70)
    print("差异明细（基准 vs 待测 不一致的题）")
    print("=" * 70)
    b_results, _, _ = all_results[bench]
    for m in models:
        if m == bench:
            continue
        r_results, _, _ = all_results[m]
        print(f"\n--- {bench} vs {m} ---")
        for cat in cat_names:
            for i, (bi, mi) in enumerate(zip(b_results[cat]["items"], r_results[cat]["items"])):
                if bi["score"] != mi["score"]:
                    print(f"  [{cat} #{i+1}] {bi['q']}...")
                    print(f"    基准({bench}): {'✓' if bi['score'] else '✗'} {bi['answer'][:60]}")
                    print(f"    待测({m}):   {'✓' if mi['score'] else '✗'} {mi['answer'][:60]}")

if __name__ == "__main__":
    main()
