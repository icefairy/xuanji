# 璇玑 Xuanji 项目上下文

## 项目定位
轻量 AI 协议网关：对外暴露 OpenAI 协议 + Claude(Anthropic) 协议，上游为多个 OpenAI 兼容端点（vLLM/OpenRouter/OpenCode Go 等），实现汇聚、负载、分流。自研原因：oneapi 对 OpenAI 协议兼容不好且太重。
中文名"璇玑"（北斗第一星，枢纽之意），仓库名 xuanji / xuanji-gate。
**仓库**: https://github.com/icefairy/xuanji.git

## 技术栈
- Go 1.22+（本机 1.26.3）
- github.com/openai/openai-go（上游 SDK）
- gopkg.in/yaml.v3（配置）
- 标准库 net/http + log/slog，无 Web 框架

## 目录
/data/codes/xuanji/ （完整规划见 PLAN.md）

## 架构要点
1. 对外入口：OpenAI /v1/* + Anthropic /v1/messages，内部统一 OpenAI 协议结构
2. 路由：按 model 字段匹配上游组
3. 分流策略：配额余量 / 延迟 / 主备 / 加权
4. 流式 SSE 纯透传（io.Copy 边收边发），协议转换时才解析
5. 熔断三态：healthy / degraded / dead，backoff 自动恢复

## 阶段划分（严格按顺序）
- Phase 0: 项目骨架 + config 解析 + /healthz
- Phase 1: OpenAI chat/completions 代理（流式+非流式）
- Phase 2: 多上游 + 路由 + 健康检查
- Phase 3: 分流策略 + 失败重试
- Phase 4: Claude 协议兼容
- Phase 5: embeddings/images/audio
- Phase 6: 统计 + 管理界面（暂缓）

## 编码规范
- 标准 Go 风格，函数有注释
- config 结构体与 yaml 严格对应
- 每个 Phase 完成可用 curl 验证
