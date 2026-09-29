package httpattempt

// 本文件原有的 Claude 值快照净化器测试已随被删的能力一路移除（票据 10）：那些用例断言的是
// 「哪些取值可以进快照」「省略摘要怎么计数」，而这条采集路径已不存在，留着测试会在编译期
// 指向已删除的符号。
//
// 传输层默认路径「不复制任何明文头值」由 counter_test.go 的
// TestDefaultPathKeepsNoHeaderValues 钉住；长期审计摘要自己的净化规则仍由 headers.go
// 的测试覆盖。
