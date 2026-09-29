package httpattempt

// 本文件原有的按协议值净化测试已随被删的能力一路移除（票据 10）：分派入口
// SanitizeProtocol*HeaderValues 与 Map 变体没有保留消费方，其「bedrock 不支持」的用例
// 也不再指向任何现存能力。
//
// 真实 wire 协议族现在只作为事实存在（Metadata.ValueProtocol，由服务层在发送前写入，
// Trace 消费），不参与任何取值净化，因此没有对应的净化用例。
