package httpattempt

// 本文件原有的按协议值净化入口（SanitizeProtocol*HeaderValues 与 Map 变体）已随旧值
// 明细退役：它们只是把值快照净化器按协议分派，因此没有值快照就没有调用方。
//
// 协议分派本身**不再需要**：真实 wire 协议族由 Metadata.ValueProtocol 记录
// （服务层在发送前写入），Trace 直接消费这个事实，不再经过取值净化。
// 详见 claude_header_values.go 的退役说明。
