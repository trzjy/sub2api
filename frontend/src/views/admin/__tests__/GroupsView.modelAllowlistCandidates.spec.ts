import { defineComponent, h, type PropType } from "vue";
import { flushPromises, mount } from "@vue/test-utils";
import { createPinia } from "pinia";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { AdminGroup, CodexModelsManifestConfig } from "@/types";
import GroupsView from "@/views/admin/GroupsView.vue";

const {
  listGroups,
  getModelAllowlistCandidates,
  getUsageSummary,
  getCapacitySummary,
  getLiveCapability,
  updateGroup,
  showError,
} = vi.hoisted(() => ({
  listGroups: vi.fn(),
  getModelAllowlistCandidates: vi.fn(),
  getUsageSummary: vi.fn(),
  getCapacitySummary: vi.fn(),
  getLiveCapability: vi.fn(),
  updateGroup: vi.fn(),
  showError: vi.fn(),
}));

vi.mock("@/api/admin", () => ({
  adminAPI: {
    groups: {
      list: listGroups,
      getAll: vi.fn(),
      getModelAllowlistCandidates,
      getUsageSummary,
      getCapacitySummary,
      getLiveCapability,
      create: vi.fn(),
      update: updateGroup,
      delete: vi.fn(),
      duplicate: vi.fn(),
      updateSortOrder: vi.fn(),
    },
    accounts: {
      list: vi.fn(),
      getById: vi.fn(),
    },
  },
}));

vi.mock("@/stores/app", () => ({
  useAppStore: () => ({
    showError,
    showSuccess: vi.fn(),
  }),
}));

vi.mock("@/stores/onboarding", () => ({
  useOnboardingStore: () => ({
    isCurrentStep: vi.fn(() => false),
    nextStep: vi.fn(),
  }),
}));

vi.mock("vue-i18n", async () => {
  const actual = await vi.importActual<typeof import("vue-i18n")>("vue-i18n");
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  };
});

const sourceGroup = {
  id: 42,
  name: "OpenAI",
  description: null,
  platform: "openai",
  rate_multiplier: 1,
  rpm_limit: 0,
  is_exclusive: false,
  status: "active",
  subscription_type: "standard",
  daily_limit_usd: null,
  weekly_limit_usd: null,
  monthly_limit_usd: null,
  long_context_pricing_enabled: true,
  force_openai_fast: false,
  free_openai_fast: false,
  model_pricing: [],
  profit_control_enabled: false,
  profit_min_margin: 0,
  profit_safety_buffer: 0,
  allow_image_generation: false,
  allow_batch_image_generation: false,
  image_rate_independent: false,
  image_rate_multiplier: 1,
  batch_image_discount_multiplier: 0.5,
  batch_image_hold_multiplier: 0.6,
  image_price_1k: null,
  image_price_2k: null,
  image_price_4k: null,
  video_rate_independent: false,
  video_rate_multiplier: 1,
  video_price_480p: null,
  video_price_720p: null,
  video_price_1080p: null,
  web_search_price_per_call: null,
  search_price_per_1k: null,
  audio_realtime_price_per_min: null,
  audio_tts_price_per_million_chars: null,
  audio_stt_price_per_hour: null,
  peak_rate_enabled: false,
  peak_start: "",
  peak_end: "",
  peak_rate_multiplier: 1,
  claude_code_only: false,
  fallback_group_id: null,
  fallback_group_id_on_invalid_request: null,
  allow_messages_dispatch: false,
  allow_live: false,
  require_oauth_only: false,
  require_privacy_set: false,
  created_at: "2026-09-05T00:00:00Z",
  updated_at: "2026-09-05T00:00:00Z",
  model_routing: null,
  model_routing_enabled: false,
  mcp_xml_inject: true,
  supported_model_scopes: [],
  account_count: 1,
  active_account_count: 1,
  rate_limited_account_count: 0,
  models_list_config: undefined,
  codex_models_manifest_config: {
    enabled: false,
    account_ids: [],
    fallback_to_scheduler: false,
  },
  sort_order: 10,
} satisfies AdminGroup;

const AppLayoutStub = defineComponent({
  template: "<main><slot /></main>",
});

const TablePageLayoutStub = defineComponent({
  template: '<section><slot name="filters" /><slot name="table" /><slot name="pagination" /></section>',
});

const DataTableStub = defineComponent({
  props: {
    data: { type: Array, default: () => [] },
  },
  template: '<div><div v-if="data.length"><slot name="cell-actions" :row="data[0]" /></div></div>',
});

const BaseDialogStub = defineComponent({
  props: {
    show: { type: Boolean, default: false },
  },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
});

const CodexManifestAccountsFieldStub = defineComponent({
  name: "CodexManifestAccountsField",
  props: {
    modelValue: {
      type: Object as PropType<CodexModelsManifestConfig>,
      required: true,
    },
  },
  emits: ["update:modelValue"],
  setup(props, { emit, expose }) {
    expose({ validate: () => true, resetValidation: () => undefined });
    return () =>
      h("div", { "data-testid": "codex-manifest-field" }, [
        h(
          "output",
          { "data-testid": "codex-manifest-value" },
          JSON.stringify(props.modelValue),
        ),
      ]);
  },
});

const mountView = () =>
  mount(GroupsView, {
    global: {
      plugins: [createPinia()],
      stubs: {
        AppLayout: AppLayoutStub,
        TablePageLayout: TablePageLayoutStub,
        DataTable: DataTableStub,
        Pagination: true,
        BaseDialog: BaseDialogStub,
        ConfirmDialog: true,
        EmptyState: true,
        Select: true,
        PlatformIcon: true,
        Icon: true,
        GroupCapacityBadge: true,
        GroupRateMultipliersModal: true,
        GroupRPMOverridesModal: true,
        ReasoningEffortPolicyFields: defineComponent({
          setup(_, { expose }) {
            expose({ validate: () => true, resetValidation: () => undefined });
            return () => h("div");
          },
        }),
        CodexManifestAccountsField: CodexManifestAccountsFieldStub,
        PricingEntryCard: true,
        VueDraggable: true,
      },
    },
  });

const allowlistTagKey = "admin.groups.modelAllowlist.candidateTag";
const loadErrorKey = "admin.groups.modelAllowlist.loadError";
const emptyCreateHintKey = "admin.groups.modelAllowlist.emptyCreateHint";
const emptyKey = "admin.groups.modelAllowlist.empty";

describe("GroupsView model allowlist candidates", () => {
  beforeEach(() => {
    localStorage.clear();
    listGroups.mockReset();
    getModelAllowlistCandidates.mockReset();
    getUsageSummary.mockReset();
    getCapacitySummary.mockReset();
    getLiveCapability.mockReset();
    updateGroup.mockReset();
    updateGroup.mockResolvedValue(sourceGroup);
    showError.mockClear();

    listGroups.mockResolvedValue({
      items: [sourceGroup],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1,
    });
    getModelAllowlistCandidates.mockResolvedValue([]);
    getUsageSummary.mockResolvedValue([]);
    getCapacitySummary.mockResolvedValue([]);
    getLiveCapability.mockResolvedValue({ supported: false });
  });

  it("shows real-supply candidates with a source tag on a successful edit load", async () => {
    getModelAllowlistCandidates.mockResolvedValue(["gpt-5.5", "gpt-5.4"]);

    const wrapper = mountView();
    await flushPromises();

    const editButton = wrapper
      .findAll("button")
      .find((button) => button.text().includes("common.edit"));
    expect(editButton).toBeTruthy();
    await editButton!.trigger("click");
    await flushPromises();

    const toggle = wrapper.find('[data-testid="edit-model-allowlist-toggle"]');
    expect(toggle.exists()).toBe(true);
    await toggle.trigger("click");
    await flushPromises();

    const text = wrapper.text();
    expect(text).toContain("gpt-5.5");
    expect(text).toContain("gpt-5.4");
    expect(text).toContain(allowlistTagKey);
    expect(text).not.toContain(loadErrorKey);
    wrapper.unmount();
  });

  it("shows a user-visible error and drops stale candidates when the edit load fails", async () => {
    getModelAllowlistCandidates.mockRejectedValue(new Error("boom"));

    const wrapper = mountView();
    await flushPromises();

    const editButton = wrapper
      .findAll("button")
      .find((button) => button.text().includes("common.edit"));
    await editButton!.trigger("click");
    await flushPromises();

    const toggle = wrapper.find('[data-testid="edit-model-allowlist-toggle"]');
    await toggle.trigger("click");
    await flushPromises();

    const text = wrapper.text();
    expect(text).toContain(loadErrorKey);
    // 失败不应把旧候选当作当前结果
    expect(text).not.toContain(allowlistTagKey);
    expect(text).not.toContain("gpt-5.5");
    wrapper.unmount();
  });

  it("saves other fields with the original allowlist when the candidate load fails", async () => {
    const savedAllowlist = { enabled: true, models: ["legacy-saved", "gpt-*"] };
    listGroups.mockResolvedValue({
      items: [{ ...sourceGroup, model_allowlist: savedAllowlist }],
      total: 1, page: 1, page_size: 20, pages: 1,
    });
    getModelAllowlistCandidates.mockRejectedValue(new Error("catalog unavailable"));

    const wrapper = mountView();
    await flushPromises();
    const editButton = wrapper.findAll("button")
      .find((button) => button.text().includes("common.edit"));
    await editButton!.trigger("click");
    await flushPromises();

    await wrapper.get('[data-tour="edit-group-form-name"]').setValue("Renamed group");
    await wrapper.get("#edit-group-form").trigger("submit");
    await flushPromises();

    expect(updateGroup).toHaveBeenCalledWith(sourceGroup.id, expect.objectContaining({
      name: "Renamed group",
      model_allowlist: savedAllowlist,
    }));
    expect(showError).not.toHaveBeenCalled();
    wrapper.unmount();
  });

  it("still rejects an enabled allowlist when the user deselects every saved rule after a catalog failure", async () => {
    listGroups.mockResolvedValue({
      items: [{ ...sourceGroup, model_allowlist: { enabled: true, models: ["legacy-saved"] } }],
      total: 1, page: 1, page_size: 20, pages: 1,
    });
    getModelAllowlistCandidates.mockRejectedValue(new Error("catalog unavailable"));
    const wrapper = mountView();
    await flushPromises();
    const editButton = wrapper.findAll("button")
      .find((button) => button.text().includes("common.edit"));
    await editButton!.trigger("click");
    await flushPromises();

    expect(wrapper.text()).toContain(loadErrorKey);
    expect(wrapper.text()).toContain("legacy-saved");
    expect(wrapper.text()).not.toContain(allowlistTagKey);
    const savedRuleCheckbox = wrapper.findAll('#edit-group-form input[type="checkbox"]')
      .find((input) => input.element.parentElement?.textContent?.includes("legacy-saved"));
    expect(savedRuleCheckbox).toBeTruthy();
    await savedRuleCheckbox!.setValue(false);
    await wrapper.get("#edit-group-form").trigger("submit");
    await flushPromises();

    expect(updateGroup).not.toHaveBeenCalled();
    expect(showError).toHaveBeenCalledWith("admin.groups.modelAllowlist.emptySelectionError");
    wrapper.unmount();
  });

  it("shows the create-page hint when no real-supply candidates exist (id=0)", async () => {
    getModelAllowlistCandidates.mockResolvedValue([]);

    const wrapper = mountView();
    await flushPromises();

    const createButton = wrapper
      .findAll("button")
      .find((button) => button.text().includes("admin.groups.createGroup"));
    expect(createButton).toBeTruthy();
    await createButton!.trigger("click");
    await flushPromises();

    const toggle = wrapper.find('[data-testid="create-model-allowlist-toggle"]');
    expect(toggle.exists()).toBe(true);
    await toggle.trigger("click");
    await flushPromises();

    const text = wrapper.text();
    expect(text).toContain(emptyCreateHintKey);
    expect(text).not.toContain(loadErrorKey);
    wrapper.unmount();
  });

  it("distinguishes saved rules from real-supply candidates in the edit dialog", async () => {
    // 保存规则里含一个不在实供候选中的条目，用于验证规则身份保留
    const groupWithSaved = {
      ...sourceGroup,
      model_allowlist: { enabled: false, models: ["gpt-5.5", "legacy-saved"] },
    } as unknown as AdminGroup;
    listGroups.mockResolvedValue({
      items: [groupWithSaved],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1,
    });
    getModelAllowlistCandidates.mockResolvedValue(["gpt-5.5", "gpt-5.4"]);

    const wrapper = mountView();
    await flushPromises();

    const editButton = wrapper
      .findAll("button")
      .find((button) => button.text().includes("common.edit"));
    await editButton!.trigger("click");
    await flushPromises();

    const toggle = wrapper.find('[data-testid="edit-model-allowlist-toggle"]');
    await toggle.trigger("click");
    await flushPromises();

    const text = wrapper.text();
    expect(text).toContain("gpt-5.5");
    expect(text).toContain("gpt-5.4");
    expect(text).toContain("legacy-saved");
    expect(text).toContain("admin.groups.modelAllowlist.savedTag");
    expect(text).toContain(allowlistTagKey);
    wrapper.unmount();
  });
});
