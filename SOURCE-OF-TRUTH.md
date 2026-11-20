# 单一源码树与发版规则（2026-11-20 立）

## 唯一源码树

    /home/ubuntu/loomy-build-vision    ← 只在这里改、只在这里编
    /home/ubuntu/loomy-verify.deprecated-20261120  ← 已废弃，不要再改、不要再部署

## 为什么立这条

2026-11-18～20 期间，两个 DSH 会话各用一棵源码树编译同一个插件，而部署是**整文件替换**
`plugins/linux/arm64/loomy-v*.so`。后发版的一方会把前一方的补丁整片覆盖——v1.0.7 的超时补丁
就是这样被 v1.0.8 冲掉的，排查方向也被带偏了一整天。

## 发版流程

1. 只改本仓库；改完必须过：`go vet ./... && go test -count=1 ./...`
2. 编译：`go build -buildmode=c-shared -trimpath -o build/loomy-<ver>-arm64.so .`（build/ 不入库）
3. 改 `plugin/main.go` 的 `pluginVer`，写 CHANGELOG，提交并打 tag `vX.Y.Z`
4. 备份并部署：
       BK=$HOME/cliproxy/plugin-backups/release-$(date +%Y%m%d%H%M%S)
       mkdir -p $BK && cp $HOME/cliproxy/config.yaml $BK/
       mv $HOME/cliproxy/plugins/linux/arm64/loomy-v*.so $BK/   # 旧件归档
       install -m0755 build/loomy-<ver>-arm64.so $HOME/cliproxy/plugins/linux/arm64/loomy-v<ver>.so
       sudo systemctl restart cliproxy
5. 验证：`journalctl -u cliproxy` 或 cliproxy.err.log 出现
   `plugin registered plugin_id=loomy ... version=<ver>`

## 版本号规则

一个 tag 只对应一次真实发布。调试、试错一律用 `build/` 下的临时文件名，
不要为每次尝试递增版本号——2026-11-20 当天曾一路编到 1.0.12，纯属返工噪声。

## 当前生产状态（2026-11-20 17:30）

- 部署：`loomy-v1.0.9.so`（本仓库 `release/1.0.9` 分支，tag v1.0.9）
- CPA 配置：`plugins.configs.loomy.stream-mode: buffered`、`routing.retry.request-retry: 1`
- 回滚基线：`$HOME/cliproxy/plugin-backups/baseline-v1.0.8/`（v1.0.8 的 .so + 当时的 config.yaml）
- 已知上游特性：`loomyad.xunfei.cn` 同一模型响应耗时 2～90 秒波动，偶发 429/503；HTTP/2 常挂死，故强制 1.1