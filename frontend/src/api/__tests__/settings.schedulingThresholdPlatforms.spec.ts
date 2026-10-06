import { describe, expect, it } from "vitest";

import {
  SCHEDULING_THRESHOLD_PLATFORMS,
  normalizeAccountSchedulingThresholdsMap,
  type AccountSchedulingThresholdsMap,
} from "@/api/admin/settings";

describe("admin settings scheduling threshold platforms (muse)", () => {
  it("registers muse as a scheduling-threshold platform (mirrors minimax)", () => {
    expect(SCHEDULING_THRESHOLD_PLATFORMS).toContain("muse");
    // 与现有阈值平台并列，不应破坏既有清单的完整性
    expect(SCHEDULING_THRESHOLD_PLATFORMS).toEqual(
      expect.arrayContaining(["openai", "anthropic", "grok", "kimi", "zhipu", "minimax", "muse"]),
    );
  });

  it("normalizes a default 100 threshold for muse when input is empty", () => {
    const normalized = normalizeAccountSchedulingThresholdsMap(null);
    expect(normalized.muse).toBe(100);
    // 枚举穷尽：返回对象的 key 必须覆盖全部阈值平台
    const keys = Object.keys(normalized).sort();
    expect(keys).toEqual([...SCHEDULING_THRESHOLD_PLATFORMS].sort());
  });

  it("returns a fully-typed AccountSchedulingThresholdsMap including muse", () => {
    const map: AccountSchedulingThresholdsMap = normalizeAccountSchedulingThresholdsMap({
      muse: 80,
    });
    expect(map.muse).toBe(80);
  });
});
