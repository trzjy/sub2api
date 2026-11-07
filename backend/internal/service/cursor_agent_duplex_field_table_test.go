//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCursorDuplexJudgmentsFollowFieldTable 证明 cursor_agent_duplex.go 里的 6 处判据
// 全部由统一字段表（cursor_field_table.go）驱动，而不是各自维护一份硬编码清单：
// 遍历字段表每个登记项，逐一核对对应判据函数的返回值与表的分类一致。
//
// 若任一处判据被改回硬编码清单、与表产生分歧，本用例会红——这正是「判据收口到一张表」的护栏。
func TestCursorDuplexJudgmentsFollowFieldTable(t *testing.T) {
	for key, class := range cursorFieldClassTable {
		switch key.domain {
		case cursorDomainExec:
			parts := []cursorPBField{{Number: key.number}}
			switch class {
			case cursorFieldMetadata:
				require.True(t, cursorExecMetadataFieldOK(key.number),
					"exec 域字段 %d 在表中为 Metadata，元数据判据应返回 true", key.number)
				require.True(t, cursorExecMetadataOnly(parts),
					"exec 域字段 %d 在表中为 Metadata，只含元数据判据应返回 true", key.number)
				require.False(t, cursorExecImplementedSubfieldOK(parts),
					"exec 域字段 %d 在表中为 Metadata，不应被判成已实现子字段", key.number)
				require.False(t, cursorNativeExecRejectFieldOK(parts),
					"exec 域字段 %d 在表中为 Metadata，不应被判成原生拒接字段", key.number)
			case cursorFieldNative:
				require.True(t, cursorNativeExecRejectFieldOK(parts),
					"exec 域字段 %d 在表中为 Native，原生拒接判据应返回 true", key.number)
				require.True(t, cursorExecImplementedSubfieldOK(parts),
					"exec 域字段 %d 在表中为 Native，已实现子字段判据应返回 true", key.number)
				require.False(t, cursorExecMetadataFieldOK(key.number),
					"exec 域字段 %d 在表中为 Native，不应被判成元数据", key.number)
			case cursorFieldHandler:
				require.True(t, cursorExecImplementedSubfieldOK(parts),
					"exec 域字段 %d 在表中为 Handler，已实现子字段判据应返回 true", key.number)
				require.False(t, cursorExecMetadataFieldOK(key.number),
					"exec 域字段 %d 在表中为 Handler，不应被判成元数据", key.number)
				require.False(t, cursorNativeExecRejectFieldOK(parts),
					"exec 域字段 %d 在表中为 Handler，不应被判成原生拒接字段", key.number)
			default:
				t.Fatalf("exec 域字段 %d 出现未预期分类 %v", key.number, class)
			}
		case cursorDomainInteraction:
			// interaction 判定段（respondInteractionQuery 的 :673 闸门）直接查表判
			// cursorFieldInteraction；此处核对表的 classification 与闸门谓词一致。
			require.Equal(t, cursorFieldInteraction, class,
				"interaction 域登记项 %d 必须为 Interaction 类", key.number)
			require.Equal(t, cursorFieldInteraction,
				cursorFieldClassOf(cursorDomainInteraction, key.number),
				"interaction 域字段 %d 经统一入口必须判为 Interaction", key.number)
		default:
			t.Fatalf("未预期消息域 %v", key.domain)
		}
	}
}

// TestCursorDuplexInteractionGateFollowsTable 证明 interaction 未知类型仍走 unsupported 分支：
// 凡是未登记进字段表的 interaction 字段号，闸门谓词都返回「非 Interaction」从而触发 unsupported
// 分支；已登记的 10 类则一律判为 Interaction（闸门放行）。派发单 D 阶段 2 起该分支不再 502
// 失败关闭，而是回通用 rejected 让模型换条路继续（闸门分类本身不变）。
func TestCursorDuplexInteractionGateFollowsTable(t *testing.T) {
	unknown := []int{0, 12, 30, 99, 1000}
	for _, n := range unknown {
		require.NotEqual(t, cursorFieldInteraction,
			cursorFieldClassOf(cursorDomainInteraction, n),
			"未登记的 interaction 字段 %d 必须判为未知（触发 unsupported）", n)
	}
	for _, n := range cursorInteractionQueryOrder {
		require.Equal(t, cursorFieldInteraction,
			cursorFieldClassOf(cursorDomainInteraction, n),
			"已知 interaction 字段 %d 必须判为 Interaction（闸门放行）", n)
	}
}
