#!/usr/bin/env python3
"""生成 DeepSeek 官方分词器的一致性基准（testdata/tokenizer_cases.json.gz）。

产物是「文本 → 官方 token id 序列」的样本集，供 Go 实现做逐 id 回归测试，
使测试自包含（CI 无需 Python）。

用法: python3 gen_cases.py <tokenizer.json> <输出路径>
"""
import gzip
import json
import random
import string
import sys

from tokenizers import Tokenizer

tok_path = sys.argv[1] if len(sys.argv) > 1 else '/tmp/ds-official/extracted/deepseek_v4_tokenizer/tokenizer.json'
out_path = sys.argv[2] if len(sys.argv) > 2 else 'internal/proxy/testdata/tokenizer_cases.json.gz'

tk = Tokenizer.from_file(tok_path)

texts = [
    # 基础中英数
    "Hello, world!",
    "你好，世界！",
    "你好",
    "世界",
    "璇玑网关是一个 AI 协议网关",
    "1234567890",
    "The quick brown fox jumps over the lazy dog.",
    # 空白与缩进
    "  leading spaces",
    "trailing spaces   ",
    "a  b",
    "\t\tindent",
    "    \t\n\r\n   ",
    "                    many          spaces",
    "1 12 123 1234 12345 123456",
    # 代码
    "def fibonacci(n):\n    return n if n < 2 else fibonacci(n-1) + fibonacci(n-2)",
    "func (h *Handler) MetricsByModel(w http.ResponseWriter, r *http.Request) {",
    "SELECT COALESCE(NULLIF(rl.upstream_model,''),'(未知)') FROM request_log GROUP BY 1;",
    "```json\n{\"key\": \"value\"}\n```",
    "```python\nprint('hello')\n```",
    # URL / 符号
    "https://www.modelscope.cn/models/deepseek-ai/DeepSeek-V4.1-Flash/resolve/master/tokenizer.json",
    "～！@#￥%……&*（）——+",
    "①②③④⑤ Ⅰ Ⅱ Ⅲ Ⅳ Ⅴ ㈠ ㈡ ㈢",
    "ｆｕｌｌｗｉｄｔｈ　ｔｅｘｔ",
    # 中文数字（cl100k 偏差最大的类别）
    "零一二三四五六七八九十百千万亿",
    "九",
    "用户输入：请帮我统计一下今天用了多少 token？",
    # 多语言
    "Ångström café naïve résumé",
    "日本語のテキストとひらがなとカタカナ",
    "한국어 텍스트",
    "ÐÑÖØÜßñçøåæ",
    # emoji 与控制字符
    "emoji 🚀 test",
    "🗿🌍🚀✨😀😃😄😁 funny emoji block",
    "\x00\x01\x02 control chars",
    "\u200b\u200c\u200d zero width",
    "a\u0301e\u0301 combining accents",
    "混合\u3000全角空格",
    # special token
    "<｜begin▁of▁sentence｜>hello<｜end▁of▁sentence｜>",
    "<｜User｜>你好<｜Assistant｜>",
    "<｜place▁holder▁no▁7｜>",
    # 纯重复
    "very" * 50,
    "x" * 500,
    "中文测试。" * 40,
    # 边界
    "",
    " ",
    "\n",
    "\n\n",
    "a",
    "中",
    "1",
]

# 随机样本：覆盖多种字符池与长度
random.seed(20260923)
pools = [
    string.printable,
    string.ascii_letters + string.digits,
    "".join(chr(i) for i in range(0x4E00, 0x4E00 + 300)),
    "".join(chr(i) for i in range(0x20, 0x7F)) + "\n\t ",
    "abc123 .,!?\n\t中文日本語한국어🚀",
    "".join(chr(i) for i in range(0x3000, 0x3100)),  # CJK 符号/标点/假名
]
for _ in range(120):
    pool = random.choice(pools)
    n = random.randint(0, 500)
    texts.append("".join(random.choice(pool) for _ in range(n)))

# 长文本：模拟真实 prompt
block = "def process(data):\n    for item in data:\n        yield item * 2\n\n"
texts.append(block * 30)
texts.append("璇玑网关负责在多种上游之间路由请求并统计 token 用量。" * 50)

cases = []
for t in texts:
    cases.append({"text": t, "ids": tk.encode(t).ids})

with gzip.open(out_path, 'wt', encoding='utf-8') as f:
    json.dump(cases, f, ensure_ascii=False, separators=(',', ':'))

total = sum(len(c['ids']) for c in cases)
print(f'cases={len(cases)} total_tokens={total} -> {out_path}')
