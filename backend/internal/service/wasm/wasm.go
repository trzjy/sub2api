// Package wasm 内嵌 shareai 扩展原版 wasm_crypto_bg.wasm，并用纯 Go 运行时 wazero
// 实例化。对外只暴露 DecryptZk01：与 oracle（原版扩展）逐字节一致地解密 metadata。
//
// 设计要点（对照 wasm_crypto.js 的 wasm-bindgen 胶水）：
//   - zk01 是纯计算（HKDF + AES-256-GCM），不调用任何 JS 导入，因此 28 个
//     __wbindgen_placeholder__ 导入全部以极简 stub 提供即可，wazero 仅要求签名匹配。
//   - 字符串 ABI 复刻 exports.zk01 包装：每串先经 wasm 自身 malloc 写入内存，再以
//     (ptr, len) 传入；第 4 个 key 参数传 0/0（undefined）。
//   - 结果布局：zk01 把 [out_ptr, out_len, err_ptr, err_len] 写进 __wbindgen_add_to_stack_pointer(-16)
//     返回的栈地址处；err_len!=0 表示异常。
//   - timestamp 必须作为字符串传入（传数字会 memory access out of bounds）。
//   - wasm 内部在 Rust String 析构时释放输入串，调用方只需用 __wbindgen_export4 释放输出串。
//
// 并发：wazero 的 Function.Call 非 goroutine 安全，且 wasm 堆/栈全局状态共享，
// 因此整段解密（分配→调用→读结果→释放）用一把互斥锁串行化。
package wasm

import (
	"context"
	_ "embed"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

//go:embed wasm_crypto_bg.wasm
var wasmBinary []byte

const moduleName = "__wbindgen_placeholder__"

// engine 持有已实例化的 wasm 模块与导出函数。所有导出函数均为纯计算，无副作用。
type engine struct {
	mu sync.Mutex

	ctx    context.Context
	mod    api.Module
	mem    api.Memory
	zk01   api.Function
	addSP  api.Function // __wbindgen_add_to_stack_pointer
	malloc api.Function // __wbindgen_export2
	free   api.Function // __wbindgen_export4
}

var (
	eng     *engine
	engErr  error
	engOnce sync.Once
)

// DecryptZk01 解密 shareai 的 metadata。
// metadata/mail/timestamp 均按字符串传入（timestamp 必须字符串）。
// 返回解密后的 JSON 字符串（含 mail/access_token/refresh_token/role/account_expire_in/mode）。
func DecryptZk01(metadata, mail, timestamp string) (string, error) {
	engOnce.Do(func() {
		eng, engErr = newEngine()
	})
	if engErr != nil {
		return "", engErr
	}
	return eng.decrypt(metadata, mail, timestamp)
}

func newEngine() (*engine, error) {
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)

	builder := rt.NewHostModuleBuilder(moduleName)
	for _, h := range hostImports() {
		builder.NewFunctionBuilder().WithFunc(h.fn).Export(h.name)
	}
	if _, err := builder.Instantiate(ctx); err != nil {
		return nil, fmt.Errorf("wasm: instantiate host module: %w", err)
	}

	mod, err := rt.Instantiate(ctx, wasmBinary)
	if err != nil {
		return nil, fmt.Errorf("wasm: instantiate crypto module: %w", err)
	}

	mem := mod.ExportedMemory("memory")
	if mem == nil {
		return nil, fmt.Errorf("wasm: memory export not found")
	}
	zk01 := mod.ExportedFunction("zk01")
	addSP := mod.ExportedFunction("__wbindgen_add_to_stack_pointer")
	malloc := mod.ExportedFunction("__wbindgen_export2")
	free := mod.ExportedFunction("__wbindgen_export4")
	if zk01 == nil || addSP == nil || malloc == nil || free == nil {
		return nil, fmt.Errorf("wasm: missing required export")
	}

	return &engine{
		ctx:    ctx,
		mod:    mod,
		mem:    mem,
		zk01:   zk01,
		addSP:  addSP,
		malloc: malloc,
		free:   free,
	}, nil
}

func (e *engine) decrypt(metadata, mail, timestamp string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	mdPtr, mdLen := e.alloc(metadata)
	mailPtr, mailLen := e.alloc(mail)
	tsPtr, tsLen := e.alloc(timestamp)

	// 预留 16 字节栈空间存放结果。
	spRes, err := e.addSP.Call(e.ctx, i32(-16))
	if err != nil {
		return "", fmt.Errorf("wasm: add_to_stack_pointer: %w", err)
	}
	sp := uint32(spRes[0])

	// zk01(stackptr, md_ptr, md_len, mail_ptr, mail_len, ts_ptr, ts_len, key_ptr=0, key_len=0)
	if _, err := e.zk01.Call(e.ctx,
		uint64(sp),
		uint64(mdPtr), uint64(mdLen),
		uint64(mailPtr), uint64(mailLen),
		uint64(tsPtr), uint64(tsLen),
		0, 0,
	); err != nil {
		return "", fmt.Errorf("wasm: zk01 call: %w", err)
	}

	buf, ok := e.mem.Read(sp, 16)
	if !ok {
		return "", fmt.Errorf("wasm: read stack result failed")
	}
	outPtr := binary.LittleEndian.Uint32(buf[0:4])
	outLen := binary.LittleEndian.Uint32(buf[4:8])
	errPtr := binary.LittleEndian.Uint32(buf[8:12])
	errLen := binary.LittleEndian.Uint32(buf[12:16])

	if errLen != 0 {
		eb, _ := e.mem.Read(errPtr, errLen)
		msg := string(append([]byte(nil), eb...))
		// 释放错误信息串（Rust 侧已分配）。
		e.free.Call(e.ctx, uint64(errPtr), uint64(errLen), 1)
		return "", fmt.Errorf("wasm: zk01 error: %s", msg)
	}

	out, _ := e.mem.Read(outPtr, outLen)
	result := string(append([]byte(nil), out...))
	// 释放输出串（对照胶水 __wbindgen_export4(r,o,1)）。
	e.free.Call(e.ctx, uint64(outPtr), uint64(outLen), 1)
	// 归还栈空间。
	e.addSP.Call(e.ctx, i32(16))

	return result, nil
}

// i32 把有符号 32 位参数编码为 wazero Call 所需的 uint64（按 wasm i32 语义符号扩展）。
func i32(v int32) uint64 { return uint64(uint32(v)) }

// alloc 用 wasm 自身 malloc 分配并写入一个 UTF-8 字符串，返回 (ptr, len)。
// 对应胶水 x() 的 ASCII 快速路径；Rust 侧 String::from_raw_parts(ptr, len, len)
// 在析构时释放，故调用方无需（也不可）再释放输入串。
func (e *engine) alloc(s string) (uint32, uint32) {
	b := []byte(s)
	// wasm-bindgen malloc(size, align)；align 传 1 与胶水 x() 一致。
	res, err := e.malloc.Call(e.ctx, uint64(len(b)), 1)
	if err != nil {
		// 理论上 zk01 之前无失败路径；这里直接 panic 以保持与返回签名简单一致。
		panic(fmt.Sprintf("wasm: malloc(%d): %v", len(b), err))
	}
	ptr := uint32(res[0])
	if !e.mem.Write(ptr, b) {
		panic(fmt.Sprintf("wasm: write string at %d len %d out of range", ptr, len(b)))
	}
	return ptr, uint32(len(b))
}

// hostImport 是单个 wasm-bindgen JS 导入桩。zk01 不调用其中任何一个，
// 因此实现一律返回 0 / 空；仅 __wbindgen_throw 在意外调用时返回错误以触发陷阱。
type hostImport struct {
	name string
	fn   interface{}
}

func hostImports() []hostImport {
	// 各 stub 的 Go 签名必须与被导入 wasm 函数的 (参数, 结果) 完全一致。
	// zk01 不调用其中任何一个，因此一律返回 0 / 空即可。
	zeroI32 := func(_ context.Context, _ api.Module, _ uint32) uint32 { return 0 }        // (i32)->i32
	ret0 := func(_ context.Context, _ api.Module) uint32 { return 0 }                     // ()->i32
	void1 := func(_ context.Context, _ api.Module, _ uint32) {}                           // (i32)->()
	void2 := func(_ context.Context, _ api.Module, _, _ uint32) {}                        // (i32,i32)->()
	void3 := func(_ context.Context, _ api.Module, _, _, _ uint32) {}                     // (i32,i32,i32)->()
	retI32_2 := func(_ context.Context, _ api.Module, _, _ uint32) uint32 { return 0 }    // (i32,i32)->i32
	retI32_3 := func(_ context.Context, _ api.Module, _, _, _ uint32) uint32 { return 0 } // (i32,i32,i32)->i32
	// throw：不可达路径；zk01 不会调用，按 void 桩处理（wazero 不支持仅返回 error 的 host 函数）。
	trap := func(_ context.Context, _ api.Module, _, _ uint32) {}

	return []hostImport{
		{"__wbg_crypto_574e78ad8b13b65f", zeroI32},
		{"__wbg_process_dc0fbacc7c1c06f7", zeroI32},
		{"__wbg_versions_c01dfd4722a88165", zeroI32},
		{"__wbg_node_905d3e251edff8a2", zeroI32},
		{"__wbg_require_60cc747a6bc5215a", ret0},
		{"__wbg_msCrypto_a61aeb35a24c1329", zeroI32},
		{"__wbg_getRandomValues_b8f5dbd5f3995a9e", void2},
		{"__wbg_randomFillSync_ac0988aba3254290", void2},
		{"__wbindgen_object_drop_ref", void1},
		{"__wbindgen_object_clone_ref", zeroI32},
		{"__wbg_new_with_length_01aa0dc35aa13543", zeroI32},
		{"__wbg_length_69bca3cb64fc8748", zeroI32},
		{"__wbg_prototypesetcall_2a6620b6922694b2", void3},
		{"__wbg_subarray_480600f3d6a9f26c", retI32_3},
		{"__wbg_static_accessor_GLOBAL_THIS_8b530f326a9e48ac", ret0},
		{"__wbg_static_accessor_SELF_6fdf4b64710cc91b", ret0},
		{"__wbg_static_accessor_GLOBAL_89e1d9ac6a1b250e", ret0},
		{"__wbg_static_accessor_WINDOW_b45bfc5a37f6cfa2", ret0},
		{"__wbg_new_no_args_ee98eee5275000a4", retI32_2},
		{"__wbg_call_e762c39fa8ea36bf", retI32_2},
		{"__wbg_call_525440f72fbfc0ea", retI32_3},
		{"__wbg___wbindgen_throw_b855445ff6a94295", trap},
		{"__wbg___wbindgen_is_object_c818261d21f283a4", zeroI32},
		{"__wbg___wbindgen_is_string_fbb76cb2940daafd", zeroI32},
		{"__wbg___wbindgen_is_function_ee8a6c5833c90377", zeroI32},
		{"__wbg___wbindgen_is_undefined_2d472862bd29a478", zeroI32},
		{"__wbindgen_cast_2241b6af4c4b2941", retI32_2},
		{"__wbindgen_cast_cb9088102bce6b30", retI32_2},
	}
}
