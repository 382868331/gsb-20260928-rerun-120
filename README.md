# localdns — 本地名称解析系统 (v1)

纯本地解析,不查询公网。所有适配器共享同一解析核心。

## 结构

- `resolver/` — 解析核心
  - `zone.go` 不可变区域 + `ZoneStore`(atomic.Pointer):区域更新整体生效,进行中的查询只读到单一版本,不会拼接两份版本。
  - `cache.go` TTL 缓存:肯定/否定结果分别存储、分别过期(否定用区域 NegativeTTL);迟到更新(同区域更旧版本)被拒绝,来源 `Source{Zone, Version}` 可核对。
  - `resolver.go` 迭代解析:沿 NS 委派下钻子区域,委派切点以下的上级记录一律不用;CNAME 追踪带环路检测,错误中包含完整别名路径。
  - `persist.go` JSON 快照(临时文件 + rename 原子写入),重启后区域与未过期缓存完整恢复。
- `server/` — UDP 网络适配器(`QUERY <name>` 行协议),仅服务本地虚构区域。
- `main.go` — 虚构区域树:root → example.test → shop.example.test。

## 运行

```sh
go run . -listen 127.0.0.1:5353 -snapshot /tmp/localdns.json
echo 'QUERY www.example.test' | nc -u 127.0.0.1 5353
```

## 测试

```sh
go test ./...
```

覆盖:别名与链路、别名环路路径、缓存过期 vs 否定缓存、子区域忽略上级无效记录、
区域原子更新(并发读写不混版本)、迟到缓存更新拒绝、父子区域来源核对、重启后正确读取。

