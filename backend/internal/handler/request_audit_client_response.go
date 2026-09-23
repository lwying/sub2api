package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const (
	minClientResponseAuditStatus = 100
	maxClientResponseAuditStatus = 599
)

// snapshotClientResponseAudit 采集四阶段中「返回客户端的响应」阶段的协议事实：
// 已提交的 HTTP 状态码与写入客户端响应体的字节数。这是一个纯读快照：只读 c.Writer
// 的已提交事实，永不读响应正文或响应头，也绝不改动响应行为或请求作用域状态——
// 既不停止任何心跳，也不接管 writer（审计故障不得影响用户请求）。
//
// 未提交（c.Writer.Written() 为 false）时原样返回：gin 的 responseWriter 在
// WriteHeaderNow 之前就持有默认状态码 200 与哨兵字节数 -1，此时记录任何一项都
// 等于伪造一个从未到达客户端的响应阶段，所以既不填默认 200 也不填 -1。
//
// 状态与字节各自独立校验：状态不在 100..599（或字节为负）时只丢该项，不连带丢掉
// 另一项已观测到的事实。观测到的 0 字节是事实（如 204、空响应体），必须保留。
// Status / Bytes 都收敛到 service.RequestAuditClientResponseKey 这一个封闭键，
// 调用方传入的其他键不被复制。返回的 map 是新分配的，绝不改写调用方的 metadata。
//
// 与并发心跳的关系：本函数不需要调用方先停止 compact/流式 SSE 心跳。请求侧写回路径
// 会把 c.Writer 换成 openAICompactKeepaliveWriter，它的 Status/Size/Written 全部在
// 心跳互斥锁下读取，因此与心跳 goroutine 的写入之间不存在数据竞争；心跳 goroutine
// 直接写内层 writer（不经包装器），所以 Size() 读到的正是 writer 实际接受的字节——
// 已发出的心跳注释字节也真实到达了客户端，故计入，取的是实际字节数而非 Content-Length。
// 一旦调用方（或 defer）已停止心跳，Stop 会在同一互斥锁上等待返回，此后不会再有心跳
// 字节写出，读取同样是安全的。
//
// 因此本函数对 compact 与普通路径一视同仁，不内联 OpenAICompactKeepaliveAdjustedWrittenSize
// 的「排除心跳、仅心跳归一为 -1」策略：那会把只适用于 compact 的判定强加给其余调用点。
// 需要「语义响应字节」的调用方应在拿到结果后自行覆盖 Bytes，而不是让快照改写事实。
func snapshotClientResponseAudit(metadata service.RequestAuditMetadata, c *gin.Context) service.RequestAuditMetadata {
	if c == nil || c.Writer == nil || !c.Writer.Written() {
		return metadata
	}

	statuses := map[string]int{}
	if status := c.Writer.Status(); status >= minClientResponseAuditStatus && status <= maxClientResponseAuditStatus {
		statuses[service.RequestAuditClientResponseKey] = status
	}
	bytes := map[string]int64{}
	if size := c.Writer.Size(); size >= 0 {
		bytes[service.RequestAuditClientResponseKey] = int64(size)
	}

	metadata.Status = statuses
	metadata.Bytes = bytes
	return metadata
}
