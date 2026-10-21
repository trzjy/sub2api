import { describe, expect, it } from "vitest";

import {
  addCustomModelAllowlistItem,
  type ModelAllowlistAddError,
  type ModelAllowlistItem,
  buildModelAllowlistConfig,
  createModelAllowlistState,
  hydrateModelAllowlistState,
  invalidateModelAllowlistCandidates,
  invertModelAllowlistSelection,
  moveModelAllowlistItem,
  selectAllModelAllowlistItems,
  setModelAllowlistCandidates,
  toggleModelAllowlistItem,
} from "../groupModelAllowlist";

const item = (id: string, selected: boolean, source: ModelAllowlistItem["source"] = "candidate"): ModelAllowlistItem => ({
  id,
  selected,
  source,
});

describe("groupModelAllowlist", () => {
  it("selects all default candidates for a new disabled config", () => {
    const state = createModelAllowlistState();

    setModelAllowlistCandidates(state, ["gpt-5.5", "gpt-5.4"]);

    expect(state.enabled).toBe(false);
    expect(state.items).toEqual([
      item("gpt-5.5", true, "candidate"),
      item("gpt-5.4", true, "candidate"),
    ]);
    expect(state.candidates).toEqual(["gpt-5.5", "gpt-5.4"]);
  });

  it("keeps saved selections and marks new candidates as unselected when editing", () => {
    const state = createModelAllowlistState({
      enabled: true,
      models: ["gpt-5.5", "gpt-5.4"],
    });

    setModelAllowlistCandidates(state, ["gpt-5.4", "legacy-gpt", "gpt-5.5"]);

    expect(state.enabled).toBe(true);
    expect(state.items).toEqual([
      item("gpt-5.5", true, "saved"),
      item("gpt-5.4", true, "saved"),
      item("legacy-gpt", false, "candidate"),
    ]);
  });

  it("keeps explicitly unselected candidates across a refresh (no re-selection)", () => {
    const state = createModelAllowlistState({
      enabled: true,
      models: ["gpt-5.5"],
    });

    setModelAllowlistCandidates(state, ["gpt-5.5", "gpt-5.4"]);
    expect(state.items).toEqual([
      item("gpt-5.5", true, "saved"),
      item("gpt-5.4", false, "candidate"),
    ]);

    toggleModelAllowlistItem(state, "gpt-5.4"); // user selects it
    expect(state.items.find((i) => i.id === "gpt-5.4")?.selected).toBe(true);

    setModelAllowlistCandidates(state, ["gpt-5.5", "gpt-5.4"]); // refresh
    expect(state.items).toEqual([
      item("gpt-5.5", true, "saved"),
      item("gpt-5.4", true, "candidate"),
    ]);
  });

  it("drops stale candidate identities on refresh, keeping saved rules", () => {
    const state = createModelAllowlistState({
      enabled: true,
      models: ["gpt-5.5"],
    });
    setModelAllowlistCandidates(state, ["gpt-5.5", "gpt-5.4"]);

    // 第二次刷新：gpt-5.4 不再出现在实供候选中
    setModelAllowlistCandidates(state, ["gpt-5.5", "gpt-5.6"]);
    expect(state.items).toEqual([
      item("gpt-5.5", true, "saved"),
      item("gpt-5.6", false, "candidate"),
    ]);
    expect(state.items.some((i) => i.id === "gpt-5.4")).toBe(false);
  });

  it("builds config with selected models in current display order", () => {
    const state = hydrateModelAllowlistState({
      enabled: true,
      models: ["gpt-5.5", "gpt-5.4", "legacy-gpt"],
    }, ["gpt-5.5", "gpt-5.4", "legacy-gpt"]);

    toggleModelAllowlistItem(state, "legacy-gpt");
    moveModelAllowlistItem(state, 1, 0);

    expect(buildModelAllowlistConfig(state)).toEqual({
      enabled: true,
      models: ["gpt-5.4", "gpt-5.5"],
    });
  });

  it("keeps selected models in payload even when disabled so reopening can restore choices", () => {
    const state = hydrateModelAllowlistState({
      enabled: false,
      models: ["gpt-5.5"],
    }, ["gpt-5.5", "gpt-5.4"]);

    expect(buildModelAllowlistConfig(state)).toEqual({
      enabled: false,
      models: ["gpt-5.5"],
    });
  });

  it("preserves saved models when candidates have not loaded yet", () => {
    const state = createModelAllowlistState({
      enabled: true,
      models: ["gpt-5.5", "gpt-5.4"],
    });

    expect(buildModelAllowlistConfig(state)).toEqual({
      enabled: true,
      models: ["gpt-5.5", "gpt-5.4"],
    });
  });

  it("selects all candidate models from the toolbar action", () => {
    const state = hydrateModelAllowlistState({
      enabled: true,
      models: ["gpt-5.5"],
    }, ["gpt-5.5", "gpt-5.4", "gpt-5.4-mini"]);

    selectAllModelAllowlistItems(state);

    expect(state.items).toEqual([
      item("gpt-5.5", true, "saved"),
      item("gpt-5.4", true, "candidate"),
      item("gpt-5.4-mini", true, "candidate"),
    ]);
  });

  it("inverts selected models from the toolbar action", () => {
    const state = hydrateModelAllowlistState({
      enabled: true,
      models: ["gpt-5.5"],
    }, ["gpt-5.5", "gpt-5.4", "gpt-5.4-mini"]);

    invertModelAllowlistSelection(state);

    expect(state.items).toEqual([
      item("gpt-5.5", false, "saved"),
      item("gpt-5.4", true, "candidate"),
      item("gpt-5.4-mini", true, "candidate"),
    ]);
  });

  it("keeps a custom entry identity when a refresh candidate shares the same id", () => {
    // 发现 2：手工添加 X 后刷新候选含同名 X → X 仍为 custom 身份；读取失败
    // invalidate 后 X 留存且身份不变（不被改标/不被 seen 吞并）。
    const state = createModelAllowlistState({ enabled: true, models: ["gpt-5.5"] });
    setModelAllowlistCandidates(state, ["gpt-5.5", "gpt-5.4"]);
    addCustomModelAllowlistItem(state, "claude-custom");
    expect(state.items.find((i) => i.id === "claude-custom")?.source).toBe("custom");

    // 刷新：实供候选中也包含 claude-custom（同名）。
    setModelAllowlistCandidates(state, ["gpt-5.5", "gpt-5.4", "claude-custom"]);
    expect(state.items).toEqual([
      item("gpt-5.5", true, "saved"),
      item("gpt-5.4", false, "candidate"),
      item("claude-custom", true, "custom"),
    ]);
    expect(state.items.find((i) => i.id === "claude-custom")?.source).toBe("custom");

    // 读取失败 invalidate：旧自动候选失效，custom 条目留存且身份不变。
    invalidateModelAllowlistCandidates(state);
    expect(state.items.find((i) => i.id === "claude-custom")?.source).toBe("custom");
    expect(state.items.some((i) => i.source === "candidate")).toBe(false);
    expect(state.items.some((i) => i.id === "claude-custom")).toBe(true);
  });

  it("keeps custom entries across a candidate refresh", () => {
    const state = createModelAllowlistState({ enabled: true, models: ["gpt-5.5"] });
    setModelAllowlistCandidates(state, ["gpt-5.5", "gpt-5.4"]);
    addCustomModelAllowlistItem(state, "claude-custom");

    expect(state.items.some((i) => i.id === "claude-custom" && i.source === "custom")).toBe(true);

    setModelAllowlistCandidates(state, ["gpt-5.5", "gpt-5.6"]); // refresh
    expect(state.items).toEqual([
      item("gpt-5.5", true, "saved"),
      item("gpt-5.6", false, "candidate"),
      item("claude-custom", true, "custom"),
    ]);
  });

  it("invalidates auto candidates on failure but keeps saved and custom rules", () => {
    const state = createModelAllowlistState({
      enabled: true,
      models: ["gpt-5.5"],
    });
    setModelAllowlistCandidates(state, ["gpt-5.5", "gpt-5.4"]);
    addCustomModelAllowlistItem(state, "claude-custom");

    invalidateModelAllowlistCandidates(state);

    expect(state.candidates).toEqual([]);
    expect(state.items).toEqual([
      item("gpt-5.5", true, "saved"),
      item("claude-custom", true, "custom"),
    ]);
    expect(state.items.some((i) => i.source === "candidate")).toBe(false);
  });
});

describe("addCustomModelAllowlistItem", () => {
  const state = () => createModelAllowlistState({ enabled: true, models: ["gpt-5.4"] });

  it("appends a trimmed entry as selected with custom source", () => {
    const s = state();
    expect(addCustomModelAllowlistItem(s, "  claude-sonnet-4.5  ")).toBeNull();
    expect(s.items.at(-1)).toEqual({ id: "claude-sonnet-4.5", selected: true, source: "custom" });
  });

  it("accepts trailing wildcard entries", () => {
    const s = state();
    expect(addCustomModelAllowlistItem(s, "gpt-5.5-*")).toBeNull();
    expect(s.items.at(-1)?.id).toBe("gpt-5.5-*");
  });

  it("rejects blank input", () => {
    expect(addCustomModelAllowlistItem(state(), "   ")).toBe<ModelAllowlistAddError>("empty");
  });

  it("accepts wildcards at any position", () => {
    const s = state();
    expect(addCustomModelAllowlistItem(s, "gpt-*-5.4")).toBeNull();
    expect(addCustomModelAllowlistItem(s, "*codex")).toBeNull();
    expect(addCustomModelAllowlistItem(s, "gpt-*-codex-*")).toBeNull();
    expect(buildModelAllowlistConfig(s).models).toEqual(["gpt-*-5.4", "*codex", "gpt-*-codex-*"]);
  });

  it("rejects duplicates case-insensitively", () => {
    const s = state();
    expect(addCustomModelAllowlistItem(s, "GPT-5.4")).toBe<ModelAllowlistAddError>("duplicate");
  });
});
