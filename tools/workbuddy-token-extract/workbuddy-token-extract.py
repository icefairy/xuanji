#!/usr/bin/env python3
"""
workbuddy-token-extract.py
从 WorkBuddy 桌面应用本地文件中提取 refresh_token，供璇玑网关手工导入使用。

支持平台：
  - Windows: %USERPROFILE%\\.workbuddy\\models.json
  - macOS:   ~/Library/Application Support/workbuddy/models.json
  - Linux:   ~/.workbuddy/models.json  或  ~/.config/workbuddy/models.json

提取字段：
  - access_token（部分版本存在 models.json 顶层）
  - refresh_token
  - domain
  - expires_at / refresh_expires_at（如有）

输出格式：JSON 到 stdout，同时打印可复制的璇玑导入 JSON。
"""

import json
import re
import os
import sys
import platform
from pathlib import Path


def find_models_json() -> Path | None:
    home = Path.home()
    candidates = [
        # Windows (WSL/Git Bash also checks these)
        home / ".workbuddy" / "models.json",
        home / ".workbuddy-ai" / "models.json",
        home / ".codebuddy" / "models.json",
        # macOS
        home / "Library" / "Application Support" / "workbuddy" / "models.json",
        home / "Library" / "Application Support" / ".workbuddy" / "models.json",
        # Linux
        home / ".config" / "workbuddy" / "models.json",
        home / ".config" / ".workbuddy" / "models.json",
        home / ".workbuddy" / "models.json",
    ]
    for p in candidates:
        if p.exists():
            return p
    return None


def extract_tokens(path: Path) -> dict:
    data = json.loads(path.read_text(encoding="utf-8"))
    result = {
        "source_file": str(path),
        "access_token": "",
        "refresh_token": "",
        "domain": "www.workbuddy.ai",
        "expires_at": 0,
        "refresh_expires_at": 0,
        "nickname": "",
        "uid": "",
        "enterprise_id": "",
    }

    # 常见结构：顶层有 accounts 数组
    accounts = data.get("accounts", [])
    if isinstance(accounts, list) and accounts:
        acct = accounts[0]
        result["access_token"] = acct.get("accessToken", acct.get("access_token", ""))
        result["refresh_token"] = acct.get("refreshToken", acct.get("refresh_token", ""))
        result["domain"] = acct.get("domain", "www.workbuddy.ai")
        result["expires_at"] = acct.get("expiresAt", acct.get("expires_at", 0))
        result["refresh_expires_at"] = acct.get("refreshExpiresAt", acct.get("refresh_expires_at", 0))
        result["nickname"] = acct.get("nickname", acct.get("nickname", ""))
        result["uid"] = acct.get("uid", acct.get("uid", ""))
        result["enterprise_id"] = acct.get("enterpriseId", acct.get("enterprise_id", ""))
        return result

    # 另一种结构：顶层直接是 account
    if isinstance(data, dict):
        result["access_token"] = data.get("accessToken", data.get("access_token", ""))
        result["refresh_token"] = data.get("refreshToken", data.get("refresh_token", ""))
        result["domain"] = data.get("domain", "www.workbuddy.ai")
        result["expires_at"] = data.get("expiresAt", data.get("expires_at", 0))
        result["refresh_expires_at"] = data.get("refreshExpiresAt", data.get("refresh_expires_at", 0))
        result["nickname"] = data.get("nickname", data.get("nickname", ""))
        result["uid"] = data.get("uid", data.get("uid", ""))
        result["enterprise_id"] = data.get("enterpriseId", data.get("enterprise_id", ""))

    return result


def extract_from_localstorage_text(text: str) -> dict:
    """
    从 localStorage 输出的文本中解析 JSON。
    支持两种输入：
      1. 直接是 JSON.stringify(localStorage) 的结果（大 JSON 对象）
      2. 控制台输出的 key: value 文本（你手动粘贴的）
    """
    result = {
        "source": "browser-localstorage",
        "access_token": "",
        "refresh_token": "",
        "domain": "www.workbuddy.ai",
        "expires_at": 0,
        "refresh_expires_at": 0,
        "nickname": "",
        "uid": "",
        "enterprise_id": "",
        "keys": [],
    }

    # 先尝试解析为 JSON
    data = None
    try:
        data = json.loads(text)
    except Exception:
        pass

    # 如果成功解析为对象，提取全部 key
    if isinstance(data, dict):
        result["keys"] = sorted(data.keys())
        # 尝试常见的 token 字段名
        token_keys = ["access_token", "accessToken", "refresh_token", "refreshToken",
                      "token", "auth_token", "authToken", "session_token", "sessionToken",
                      "id_token", "idToken"]
        for k in token_keys:
            if k in data and data[k]:
                if "refresh" in k.lower():
                    result["refresh_token"] = data[k]
                else:
                    result["access_token"] = data[k]
        # 域名/用户信息
        for k in ["domain", "uid", "nickname", "user_id", "userId", "enterprise_id", "enterpriseId"]:
            if k in data and data[k]:
                field = k.replace("userId", "uid").replace("enterpriseId", "enterprise_id")
                result[field] = data[k]
        return result

    # 解析失败：按行查找 key: value 模式
    lines = text.splitlines()
    keys = []
    for line in lines:
        line = line.strip()
        m = re.match(r'^["\']?(\w+)["\']?\s*:\s*(.+)$', line)
        if m:
            key = m.group(1)
            val = m.group(2).strip().rstrip(",").rstrip("}").strip('"').strip("'")
            keys.append((key, val))
            if key in ["access_token", "accessToken"]:
                result["access_token"] = val
            elif key in ["refresh_token", "refreshToken"]:
                result["refresh_token"] = val
            elif key in ["domain"]:
                result["domain"] = val
            elif key in ["uid", "user_id", "userId"]:
                result["uid"] = val
            elif key in ["nickname"]:
                result["nickname"] = val
            elif key in ["enterprise_id", "enterpriseId"]:
                result["enterprise_id"] = val
    result["keys"] = sorted([k for k, _ in keys])
    return result


def main():
    import argparse
    parser = argparse.ArgumentParser(description="WorkBuddy token extractor")
    parser.add_argument("--browser-file", help="浏览器 localStorage 输出文件路径")
    args = parser.parse_args()

    # 浏览器模式
    if args.browser_file:
        path = Path(args.browser_file)
        if not path.exists():
            print(f"文件不存在: {path}", file=sys.stderr)
            sys.exit(1)
        text = path.read_text(encoding="utf-8")
        info = extract_from_localstorage_text(text)

        # 先输出全部 key，让用户看
        print("=== localStorage 全部 key ===")
        for k in info.get("keys", []):
            print(f"  {k}")
        print()

        # 输出疑似 token 字段
        print("=== 疑似 token 字段 ===")
        for k in ["access_token", "refresh_token", "token", "auth_token", "session_token"]:
            v = info.get(k, "")
            if v:
                print(f"  {k}: {v[:80]}...")
        print()

        # 输出完整结果
        print("=== 完整解析结果 ===")
        print(json.dumps({k: v for k, v in info.items() if k != "keys"}, ensure_ascii=False, indent=2))
        return

    # 桌面应用模式：自动搜 models.json
    path = find_models_json()
    if not path:
        print("未找到 WorkBuddy models.json，请确认桌面应用已登录。", file=sys.stderr)
        print("搜索路径：", file=sys.stderr)
        for p in [
            Path.home() / ".workbuddy" / "models.json",
            Path.home() / ".workbuddy-ai" / "models.json",
            Path.home() / "Library" / "Application Support" / "workbuddy" / "models.json",
            Path.home() / ".config" / "workbuddy" / "models.json",
        ]:
            print(f"  - {p}", file=sys.stderr)
        sys.exit(1)

    info = extract_tokens(path)
    rt = info.get("refresh_token", "")
    if not rt:
        print("错误：models.json 中未找到 refresh_token，请确认 WorkBuddy 已登录。", file=sys.stderr)
        sys.exit(1)

    # 输出完整 JSON
    print(json.dumps(info, ensure_ascii=False, indent=2))

    # 输出璇玑导入格式
    import_payload = {
        "vendor": "workbuddy",
        "name": info.get("nickname") or info.get("uid") or "wb-imported",
        "refresh_token": rt,
        "domain": info.get("domain", "www.workbuddy.ai"),
    }
    print("\n--- 璇玑手工导入 JSON ---")
    print(json.dumps(import_payload, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
