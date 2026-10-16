// 本文件在既有 wasm 包内补齐 cursor 出站鉴权所需的 zk03(encryptLocalData) /
// zk04(decryptLocalData) 封装，复用同一份内嵌 wasm_crypto_bg.wasm（不新增第二份二进制）。
//
// 背景（E1 取证，证据见 ~/.sub2api-acceptance/cursor-20261016-wasm-forensics/）：
// 插件 :8090 generateEncryptedCredentials 以 wasm 的 encryptLocalData 加密
// JSON.stringify({d: deviceId, a: activationCode})，产物写入 ~/.cursor_free_data
// 的 encryptedCredentials；出站时作为 x-jg-auth 头注入（wasm zk27 活体实测确认）。
//
// 与 DecryptZk01 的差别：zk01 是纯计算（不调任何 JS 导入），而 zk03 需要
//   - crypto.getRandomValues（填 AES-256-GCM 的 IV）
//   - 一个极小的 JS 值堆（Uint8Array / subarray / prototype.set 拷贝）
//
// 故本文件自建一套独立运行时（不复用 zk01 的 engine），避免污染既有解密路径。
// 活体实测：zk04(zk03(x)) == x。
package wasm

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// authEngine 封装 zk03/zk04 所需的 wasm 运行时。
//
// 并发：wazero 的 Function.Call 非 goroutine 安全，且 wasm 堆/栈为共享全局状态，
// 因此整段调用（分配→调用→读结果→释放）用互斥锁串行化（与 engine.mu 同口径）。
type authEngine struct {
	mu sync.Mutex

	ctx    context.Context
	mem    api.Memory
	heap   []jsValue
	zk03   api.Function
	zk04   api.Function
	addSP  api.Function
	malloc api.Function
	free   api.Function
}

// jsValue 是被 wasm 以整数句柄引用的 JS 值：Uint8Array（bytes 非 nil）或哨兵对象。
type jsValue struct {
	bytes []byte
	tag   string
}

var (
	authEng    *authEngine
	authErr    error
	authOnce   sync.Once
	authMuLock sync.Mutex
)

// EncryptLocalData 调用 wasm zk03 加密明文，返回 base64(JSON{iv,ct,tag}) 密文。
//
// 该密文即 cursor 出站的 x-jg-auth 头值。
func EncryptLocalData(plain string) (string, error) {
	eng, err := getAuthEngine()
	if err != nil {
		return "", err
	}
	eng.mu.Lock()
	defer eng.mu.Unlock()
	out, err := eng.call(eng.zk03, plain)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", errors.New("wasm: zk03 returned empty ciphertext")
	}
	return out, nil
}

// DecryptLocalData 调用 wasm zk04 解密（供单测交叉验证 round-trip）。
func DecryptLocalData(cipher string) (string, error) {
	eng, err := getAuthEngine()
	if err != nil {
		return "", err
	}
	eng.mu.Lock()
	defer eng.mu.Unlock()
	return eng.call(eng.zk04, cipher)
}

// getAuthEngine 惰性初始化并返回鉴权引擎。
func getAuthEngine() (*authEngine, error) {
	// 先用互斥锁串行化初始化，避免并发首次调用重复实例化（sync.Once 的 Do 内不可
	// 返回错误，故在此包一层锁把错误回传给调用方）。
	authMuLock.Lock()
	defer authMuLock.Unlock()
	authOnce.Do(func() {
		authEng, authErr = newAuthEngine()
	})
	if authErr != nil {
		return nil, authErr
	}
	return authEng, nil
}

// newAuthEngine 实例化 wasm 并装配 zk03/zk04 所需的宿主导入。
func newAuthEngine() (*authEngine, error) {
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)

	eng := &authEngine{ctx: ctx}
	builder := rt.NewHostModuleBuilder(moduleName)
	add := func(name string, fn interface{}) {
		builder.NewFunctionBuilder().WithFunc(fn).Export(name)
	}

	// 静态访问器返回稳定的哨兵句柄（zk03 不读其内容，只做身份比较）。
	sentinel := func(tag string) uint32 { return eng.push(jsValue{tag: tag}) }
	globalThis := sentinel("globalThis")
	self := sentinel("self")
	globalObj := sentinel("global")
	windowH := sentinel("window")

	// 以下 6 个导入 zk03 不触达（与 wasm.go 同口径的极简桩），仅要求签名匹配。
	unusedI32 := func(_ context.Context, _ api.Module, _ uint32) uint32 { return 0 }
	unused0 := func(_ context.Context, _ api.Module) uint32 { return 0 }
	add("__wbg_crypto_574e78ad8b13b65f", unusedI32)
	add("__wbg_process_dc0fbacc7c1c06f7", unusedI32)
	add("__wbg_versions_c01dfd4722a88165", unusedI32)
	add("__wbg_node_905d3e251edff8a2", unusedI32)
	add("__wbg_require_60cc747a6bc5215a", unused0)
	add("__wbg_msCrypto_a61aeb35a24c1329", unusedI32)

	// crypto.getRandomValues(crypto, view) / randomFillSync(?, view)：填 GCM IV。
	add("__wbg_getRandomValues_b8f5dbd5f3995a9e",
		func(_ context.Context, _ api.Module, _ uint32, view uint32) { eng.fillRandom(view) })
	add("__wbg_randomFillSync_ac0988aba3254290",
		func(_ context.Context, _ api.Module, _ uint32, view uint32) { eng.fillRandom(view) })

	add("__wbindgen_object_drop_ref", func(_ context.Context, _ api.Module, id uint32) {
		if int(id) < len(eng.heap) {
			eng.heap[id].bytes = nil
		}
	})
	add("__wbindgen_object_clone_ref", func(_ context.Context, _ api.Module, id uint32) uint32 {
		return eng.push(eng.at(id))
	})
	// new Uint8Array(n)
	add("__wbg_new_with_length_01aa0dc35aa13543",
		func(_ context.Context, _ api.Module, n uint32) uint32 {
			return eng.push(jsValue{bytes: make([]byte, int(n)), tag: "Uint8Array"})
		})
	add("__wbg_length_69bca3cb64fc8748",
		func(_ context.Context, _ api.Module, id uint32) uint32 {
			return uint32(len(eng.at(id).bytes))
		})
	// Uint8Array.prototype.set.call(new Uint8Array(memory, ptr, len), src)
	add("__wbg_prototypesetcall_2a6620b6922694b2",
		func(_ context.Context, m api.Module, ptr, length, src uint32) {
			if !m.Memory().Write(ptr, eng.at(src).bytes) {
				panic("wasm: prototypesetcall write out of range")
			}
		})
	add("__wbg_subarray_480600f3d6a9f26c",
		func(_ context.Context, _ api.Module, id, start, end uint32) uint32 {
			b := eng.at(id).bytes
			return eng.push(jsValue{bytes: b[start:end], tag: "Uint8Array"})
		})

	add("__wbg_static_accessor_GLOBAL_THIS_8b530f326a9e48ac",
		func(_ context.Context, _ api.Module) uint32 { return globalThis })
	add("__wbg_static_accessor_SELF_6fdf4b64710cc91b",
		func(_ context.Context, _ api.Module) uint32 { return self })
	add("__wbg_static_accessor_GLOBAL_89e1d9ac6a1b250e",
		func(_ context.Context, _ api.Module) uint32 { return globalObj })
	add("__wbg_static_accessor_WINDOW_b45bfc5a37f6cfa2",
		func(_ context.Context, _ api.Module) uint32 { return windowH })

	add("__wbg_new_no_args_ee98eee5275000a4",
		func(_ context.Context, _ api.Module, _, _ uint32) uint32 {
			return eng.push(jsValue{tag: "obj"})
		})
	add("__wbg_call_e762c39fa8ea36bf",
		func(_ context.Context, _ api.Module, _, _ uint32) uint32 { return 0 })
	add("__wbg_call_525440f72fbfc0ea",
		func(_ context.Context, _ api.Module, _, _, _ uint32) uint32 { return 0 })
	add("__wbg___wbindgen_throw_b855445ff6a94295",
		func(_ context.Context, _ api.Module, _, _ uint32) {})
	add("__wbg___wbindgen_is_object_c818261d21f283a4",
		func(_ context.Context, _ api.Module, id uint32) uint32 {
			if int(id) < len(eng.heap) {
				return 1
			}
			return 0
		})
	add("__wbg___wbindgen_is_string_fbb76cb2940daafd",
		func(_ context.Context, _ api.Module, _ uint32) uint32 { return 0 })
	add("__wbg___wbindgen_is_function_ee8a6c5833c90377",
		func(_ context.Context, _ api.Module, _ uint32) uint32 { return 0 })
	add("__wbg___wbindgen_is_undefined_2d472862bd29a478",
		func(_ context.Context, _ api.Module, _ uint32) uint32 { return 0 })
	identity := func(_ context.Context, _ api.Module, _, id uint32) uint32 { return id }
	add("__wbindgen_cast_2241b6af4c4b2941", identity)
	add("__wbindgen_cast_cb9088102bce6b30", identity)

	if _, err := builder.Instantiate(ctx); err != nil {
		return nil, fmt.Errorf("wasm: instantiate auth host module: %w", err)
	}
	mod, err := rt.Instantiate(ctx, wasmBinary)
	if err != nil {
		return nil, fmt.Errorf("wasm: instantiate auth crypto module: %w", err)
	}

	eng.mem = mod.ExportedMemory("memory")
	eng.zk03 = mod.ExportedFunction("zk03")
	eng.zk04 = mod.ExportedFunction("zk04")
	eng.addSP = mod.ExportedFunction("__wbindgen_add_to_stack_pointer")
	eng.malloc = mod.ExportedFunction("__wbindgen_export2")
	eng.free = mod.ExportedFunction("__wbindgen_export4")
	if eng.mem == nil || eng.zk03 == nil || eng.zk04 == nil ||
		eng.addSP == nil || eng.malloc == nil || eng.free == nil {
		return nil, errors.New("wasm: auth engine missing required export")
	}
	return eng, nil
}

// push 追加一个 JS 值并返回句柄。
func (e *authEngine) push(v jsValue) uint32 {
	e.heap = append(e.heap, v)
	return uint32(len(e.heap) - 1)
}

// at 读取句柄对应的 JS 值（越界返回零值，避免宿主 panic）。
func (e *authEngine) at(id uint32) jsValue {
	if int(id) >= len(e.heap) {
		return jsValue{}
	}
	return e.heap[id]
}

// fillRandom 用 crypto/rand 填充 Uint8Array。
func (e *authEngine) fillRandom(id uint32) {
	if int(id) >= len(e.heap) {
		return
	}
	if _, err := rand.Read(e.heap[id].bytes); err != nil {
		panic("wasm: crypto/rand failed: " + err.Error())
	}
}

// allocString 用 wasm 自身 malloc 写入一个 UTF-8 字符串，返回 (ptr, len)。
func (e *authEngine) allocString(s string) (uint32, uint32) {
	b := []byte(s)
	res, err := e.malloc.Call(e.ctx, uint64(len(b)), 1)
	if err != nil {
		panic("wasm: auth malloc: " + err.Error())
	}
	ptr := uint32(res[0])
	if !e.mem.Write(ptr, b) {
		panic("wasm: auth alloc write out of range")
	}
	return ptr, uint32(len(b))
}

// call 复刻 wasm.go 中 zk01 同款 ABI 调用：预留 16 字节栈，末位补 key_ptr/key_len=0。
func (e *authEngine) call(fn api.Function, arg string) (string, error) {
	spRes, err := e.addSP.Call(e.ctx, i32(-16))
	if err != nil {
		return "", fmt.Errorf("wasm: auth add_to_stack_pointer: %w", err)
	}
	sp := uint32(spRes[0])

	ptr, l := e.allocString(arg)
	if _, err := fn.Call(e.ctx, uint64(sp), uint64(ptr), uint64(l), 0, 0); err != nil {
		return "", fmt.Errorf("wasm: auth call: %w", err)
	}

	buf, ok := e.mem.Read(sp, 16)
	if !ok {
		return "", errors.New("wasm: auth read stack result failed")
	}
	outPtr := binary.LittleEndian.Uint32(buf[0:4])
	outLen := binary.LittleEndian.Uint32(buf[4:8])
	errPtr := binary.LittleEndian.Uint32(buf[8:12])
	errLen := binary.LittleEndian.Uint32(buf[12:16])

	if errLen != 0 {
		eb, _ := e.mem.Read(errPtr, errLen)
		msg := string(append([]byte(nil), eb...))
		e.free.Call(e.ctx, uint64(errPtr), uint64(errLen), 1)
		return "", fmt.Errorf("wasm: auth error: %s", msg)
	}

	out, _ := e.mem.Read(outPtr, outLen)
	result := string(append([]byte(nil), out...))
	e.free.Call(e.ctx, uint64(outPtr), uint64(outLen), 1)
	e.addSP.Call(e.ctx, i32(16))
	return result, nil
}
