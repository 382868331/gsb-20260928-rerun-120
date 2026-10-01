# localdns — 本地名称解析系统 (v2)

纯本地解析,不查询公网。所有适配器共享同一解析核心。

## 结构

- `resolver/` — 解析核心
  - `zone.go` 不可变区域 + `ZoneStore`(atomic.Pointer):区域更新整体生效,进行中的查询只读到单一版本,不会拼接两份版本。
  - `cache.go` TTL 缓存:肯定/否定结果分别存储、分别过期(否定用区域 NegativeTTL);迟到更新(同区域更旧版本)被拒绝,来源 `Source{Zone, Version}` 可核对;区域发布新版本后,源自旧版本的缓存(含否定结果)立即失效。
  - `resolver.go` 迭代解析:沿 NS 委派下钻子区域,委派切点以下的上级记录一律不用;CNAME 可跨区追踪,带环路检测,错误中包含完整别名路径;保留全部区域版本历史,`ResolveAt` 按指定版本组合重放历史视图,结果 `Source` 指明答复出自哪组区域版本。
  - `batch.go` 区域批次变更:`PreviewBatch` 预演(记录增删 diff + 将失效的缓存条目,不改任何状态),`ApplyBatch` 整体校验后原子发布;`RollbackZone` 以新版本号重新发布旧内容(版本单调递增,回退本身也入历史);`RemoveZone` 撤销委派并清除该区域来源的缓存。
  - `persist.go` JSON 快照(临时文件 + rename 原子写入),包含当前区域、完整版本历史与未过期缓存,重启后历史答复仍可解释来源。
- `server/` — UDP 网络适配器(`QUERY <name>` / `QUERYAT <name> <zone@version,...>` 行协议),仅服务本地虚构区域。
- `main.go` — 虚构区域树:root → example.test → shop.example.test。

## 运行

```sh
go run . -listen 127.0.0.1:5353 -snapshot /tmp/localdns.json
echo 'QUERY www.example.test' | nc -u 127.0.0.1 5353
echo 'QUERYAT www.example.test example.test@1' | nc -u 127.0.0.1 5353
```

## 测试

```sh
go test ./...
```

覆盖:别名与链路、别名环路路径、缓存过期 vs 否定缓存、子区域忽略上级无效记录、
区域原子更新(并发读写不混版本)、迟到缓存更新拒绝、父子区域来源核对、重启后正确读取、
批次预演不改状态、批次整体校验(部分失败不应用)、规则变化后否定缓存立即失效、
跨区别名、撤销委派后上级恢复权威、区域回退(版本单调)、历史视图查询、
重启后历史答复来源可解释。
