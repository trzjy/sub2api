package service

// 本文件放**默认组与 unit 组都要用**的通用测试小助手，刻意不带 build tag。
//
// 为什么必须无 tag：Go 的构建约束是单向的——`go test -tags=unit` 时无 tag 的文件
// 同样参与编译，但 `go test ./...`（默认组）看不到 unit tag 文件。助手若留在
// unit tag 文件里，默认组就会 undefined 编译失败（2026-11-06 实际踩到：
// ollama_cloud_usage_test.go / opencode_go_usage_test.go 调 ptr 编不过）。
//
// 反向约束：本文件里的符号对两组都可见，新增助手前先确认全包没有同名定义。

// ptr 返回 v 的指针，用于测试里给 *T 字段造值。
//
// 全包唯一一处定义（原先在 gateway_multiplatform_test.go，该文件带 unit tag，
// 导致默认组用不了）；禁止在带 build tag 的测试文件里另建同名助手。
func ptr[T any](v T) *T {
	return &v
}
