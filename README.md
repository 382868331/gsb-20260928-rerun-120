# localdns — 本地名称解析系统 (v1)

纯本地解析,不查询公网。所有适配器共享同一解析核心。

## 结构

- `resolver/` — 解析核心
  - `zone.go` 不可变区域;名称规范化工具。
  - `view.go` 区域视图与批次变更:所有区域挂在单一原子视图指针后,一次查询只读一个视图;`Batch`(Upsert/Remove)整体发布,`PreviewBatch` 预演(报告区域差异与将失效的缓存项,不改状态);每次变更记入 `ViewRecord` 历史(区域→版本映射 + 备注)。
  - `cache.go` TTL 缓存:肯定/否定结果分别存储、分别过期(否定用区域 NegativeTTL);迟到更新(同区域更旧版本)被拒绝,来源 `Source{Zone, Version}` 可核对;视图发布后按来源版本不等即失效(升级、撤销委派、回退一视同仁),过期否定结果不会继续返回。
  - `resolver.go` 迭代解析:沿 NS 委派下钻子区域,委派切点以下的上级记录一律不用;CNAME 可跨区追踪,带环路检测,错误中包含完整别名路径;`ResolveInView` 对保留的历史视图重放查询(绕过缓存),答案携带 `View` 编号可对应历史记录。
  - `persist.go` JSON 快照(临时文件 + rename 原子写入):保存当前视图 ID、区域、未过期缓存与变更历史,重启后历史答复仍可解释来自哪组区域版本。
- `server/` — UDP 网络适配器,仅服务本地虚构区域:`QUERY <name>`、`QUERYAT <view> <name>`(历史视图查询)、`VIEWS`(变更历史)。
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
区域原子更新(并发读写不混版本)、跨区批次原子性(并发查询只见完整旧/新视图)、
预演不改状态且报告将失效缓存、规则变化后否定缓存失效、撤销委派与区域回退、
跨区别名、迟到缓存更新拒绝、父子区域来源核对、历史视图查询与重启后历史解释。

