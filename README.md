# localdns — 本地虚构名称解析系统（Go）

一个只查询本地虚构区域的名称解析核心，外加同一核心上的 UDP/TCP 适配。
**第一版不查询公网、不做转发**：所有答案都来自配置的区域、缓存或本地接口。

## 目录结构

- `resolver/` — 解析核心（与网络无关，可直接在进程内使用）
  - `model.go`：名称规范化与记录模型（`A` / `CNAME` / `NS`）
  - `zone.go`：不可变区域快照、父子委派边、整代原子更新与落盘
  - `resolve.go`：查询流程、CNAME 别名链、否定结果、循环路径
  - `cache.go`：肯定 / 否定分离的 TTL 缓存，带来源版本校验
  - `system.go`：把区域、缓存、解析核心组装在一起
- `net/server.go`：UDP（每包一个 JSON）与 TCP（JSON 流）适配，共用同一核心
- `cmd/localdns/main.go`：`seed` / `zone` / `query` / `serve` 命令

## 关键设计如何对应需求

- **虚构区域、缓存、查询过程**：`System` 由 `ZoneStore`、`Cache`、`Resolver` 组成；
  查询先查缓存，未命中再在冻结的一代区域上解析并回填缓存。
- **别名（CNAME）与子区域委派**：解析时逐跳跟随 CNAME；每次跳转重新选择
  “最具体的已配置区域”（来源原点最长匹配）。子区域对自己的命名空间负责，
  父区域中同名的无效记录永远不会被采用。
- **缓存过期与否定结果分别处理**：`positive` 与 `negative` 是两张独立的表；
  否定结果（`NXDOMAIN` 名字不存在 / `NODATA` 名字存在但无该类型）使用独立的
  否定 TTL，循环和错误结果不缓存。
- **别名循环指出路径**：再次访问任一名字即判定循环，返回 `ALIAS_LOOP`，并给出
  完整 `loop_path`（末尾是重复的名字）和可读的 `a -> b -> a` 错误文本。
- **区域更新完整生效、读不到两份版本拼接**：区域集合是不可变快照，整代替换，
  一次指针切换发布；查询开始时抓取一次 `generation`，整条别名链都在这一代上完成。
- **父子区域 + 迟到缓存更新核对来源**：每条缓存记录带 `sourceVersion`（来源代）。
  区域更新后旧代条目不被服务、不能覆盖新代条目；“迟到”的旧结果写入会被拒绝。
  提交更新同时清空缓存。
- **重启后正确读取**：每次提交用“临时文件 + rename”原子写入 `zones.json`；
  重启时载入完整的新一代数据，委派关系在加载后重建。
- **网络适配仍用同一核心，不查公网**：`net` 包只做 JSON 编解码与 I/O，
  答案一律调用同一个 `Resolver.Resolve`；没有任何上游/公网转发逻辑。

## 快速使用

```bash
go test ./... -race

go build -o localdns ./cmd/localdns
./localdns seed                      # 写入虚构区域 zones.json
./localdns query -name www.example.test
./localdns query -name gw.city.example.test   # 子区域委派答案
./localdns query -name loop1.example.test     # 带路径的循环报错
./localdns zone -origin example.test -add name:A:10.0.0.5:60
./localdns serve -addr 127.0.0.1:5354
```

UDP 请求/响应与 TCP 流载荷都是一个 JSON 对象：

```json
{"name": "www.example.test", "kind": "A"}
```
