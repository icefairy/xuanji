# 璇玑网关 v1.6.0

璇玑（Xuanji）——自研 OpenAI/Claude 汇聚网关，多上游负载均衡、成本路由、缓存降本、健康治理。

## 本次新增

- **统计接口性能**：daily_stats 按天预聚合 + 60s 接口缓存 + 后台定时重算，30d 统计接口冷算 <1s
- **数据保留治理**：request_log 保留 30 天、health_probe_log 保留 7 天，每日自动清理，DB 稳定不膨胀
- **级联治理**：删除上游时自动清理其 request_log 与统计；上游列表过滤已删除僵尸渠道
- **统一健康探测**：所有 OpenAI 兼容上游统一 POST `/chat/completions` 无凭证探测（响应 401/403 即视为健康，零 token 消耗、免真实推理），不再依赖 `GET /models`，天然支持微信小程序大赛等不提供该路径的端点
- **单二进制部署**：前端资源内嵌（go:embed），部署仅需一个 xuanji-server；`build.sh` 注入 buildDate 版本号
- **管理端**：离线 charts、刷新统计缓存按钮、收起已删除上游、dots 类型文案简化

## 安装（Linux amd64）

```bash
curl -sL -o xuanji-server.gz https://github.com/icefairy/xuanji/releases/download/v1.6.0/xuanji-server-linux-amd64.gz
gzip -d xuanji-server.gz && chmod +x xuanji-server && sudo mv xuanji-server /usr/local/bin/
# 运行前设置网关配置（配置文件/环境变量），然后：
# ./xuanji-server
```

其他平台：`xuanji-server-linux-arm64.gz`、`xuanji-server-windows-amd64.exe.gz` 见 Release 附件。

## 安装

一条命令上手（已构建好二进制）：

```
curl -sL -o xuanji-server.gz https://github.com/icefairy/xuanji/releases/latest/download/xuanji-server-linux-amd64.gz && gzip -d xuanji-server.gz && chmod +x xuanji-server && ./xuanji-server
```

觉得有用的话给个 star ⭐ https://github.com/icefairy/xuanji