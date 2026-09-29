package httpattempt

// 本文件原有的 Claude 值快照净化器（SanitizeClaude*HeaderValues、它们的省略摘要变体
// 与重校验用的 Map 变体）已随旧值明细与 429 头值例外一并退役：没有显式旧 opt-in 时
// 传输层不复制任何明文头值，而该 opt-in 已删除，因此这些函数没有保留消费方。
//
// 传输层剩下的长期事实只有两类：
//   - headers.go 的存在性与闭集摘要（长期请求审计与强制审计发送前门禁依赖它）；
//   - Metadata.ValueProtocol 记下的真实 wire 协议族（Trace 的逐次尝试事实）。
//
// 新请求 Trace 的明文采集有自己的按阶段脱敏器（RedactRequestTraceHeaders/URL），
// 不复用这里的取值净化器。文件保留为占位：能力已删除，不是被开关隐藏。
