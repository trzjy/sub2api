import { describe, it, expect, vi, beforeEach } from "vitest";
import { flushPromises, mount } from "@vue/test-utils";

import SettingsView from "../SettingsView.vue";

const { getSettings, updateSettings } = vi.hoisted(() => ({
  getSettings: vi.fn(),
  updateSettings: vi.fn(),
}));

vi.mock("@/api", () => ({
  adminAPI: {
    settings: { getSettings, updateSettings },
    accounts: { getAll: vi.fn(), getUpstreamBillingProbeSettings: vi.fn(), updateUpstreamBillingProbeSettings: vi.fn(), getOllamaCloudUsageSettings: vi.fn(), updateOllamaCloudUsageSettings: vi.fn() },
    groups: { getAll: vi.fn() },
    proxies: { list: vi.fn() },
    payment: { getProviders: vi.fn(), updateProvider: vi.fn(), createProvider: vi.fn(), deleteProvider: vi.fn() },
  },
}));
vi.mock("@/stores", () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn(), showWarning: vi.fn(), showInfo: vi.fn(), fetchPublicSettings: vi.fn() }),
}));
vi.mock("@/stores/adminSettings", () => ({ useAdminSettingsStore: () => ({ fetch: vi.fn() }) }));
vi.mock("@/composables/useClipboard", () => ({ useClipboard: () => ({ copyToClipboard: vi.fn() }) }));
vi.mock("@/utils/apiError", () => ({
  extractApiErrorMessage: () => "error",
  extractI18nErrorMessage: () => "error",
}));
vi.mock("vue-i18n", async () => {
  const actual = await vi.importActual<typeof import("vue-i18n")>("vue-i18n");
  return { ...actual, useI18n: () => ({ t: (k: string) => k, locale: { value: "zh-CN" } }) };
});

const AppLayoutStub = { template: "<div><slot /></div>" };
const ToggleStub = { template: "<input type='checkbox' class='toggle-stub' />" };

const clone = <T>(value: T): T => JSON.parse(JSON.stringify(value))

function mountView() {
  return mount(SettingsView, {
    global: {
      stubs: {
        AppLayout: AppLayoutStub,
        Select: true,
        Toggle: ToggleStub,
        Icon: true,
        ConfirmDialog: true,
        PaymentProviderList: true,
        PaymentProviderDialog: true,
        GroupBadge: true,
        GroupOptionItem: true,
        ProxySelector: true,
        ImageUpload: true,
        BackupSettings: true,
      },
    },
  });
}

const baseSettings = {
  site_name: "Test Site",
  footer_links: [] as Array<{ id: string; name: string; url: string; sort_order: number }>,
}

describe("admin SettingsView footer links", () => {
  beforeEach(() => {
    getSettings.mockReset();
    updateSettings.mockReset();
    getSettings.mockResolvedValue(clone(baseSettings));
    // Save response: server generates a stable id for links with empty id (backfill local rows)
    updateSettings.mockImplementation(async (payload: any) => ({
      ...baseSettings,
      ...payload,
      footer_links: (payload.footer_links ?? []).map((l: any, i: number) => ({
        ...l,
        id: l.id || `gen-${i}`,
      })),
    }));
  });

  it("renders the footer links edit area and adds a link row", async () => {
    const wrapper = mountView();
    await flushPromises();
    expect(wrapper.find('[data-testid="footer-link-add"]').exists()).toBe(true);
    expect(wrapper.findAll('[data-testid="footer-link-row"]')).toHaveLength(0);

    await wrapper.get('[data-testid="footer-link-add"]').trigger("click");
    await flushPromises();
    expect(wrapper.findAll('[data-testid="footer-link-row"]')).toHaveLength(1);
  });

  it("binds loaded footer_links from settings response", async () => {
    getSettings.mockResolvedValueOnce(clone({
      ...baseSettings,
      footer_links: [
        { id: "a", name: "Link A", url: "https://a.example.com", sort_order: 1 },
        { id: "b", name: "Link B", url: "https://b.example.com", sort_order: 2 },
      ],
    }));
    const wrapper = mountView();
    await flushPromises();

    const rows = wrapper.findAll('[data-testid="footer-link-row"]');
    expect(rows).toHaveLength(2);
    expect((rows[0].get('[data-testid="footer-link-name"]').element as HTMLInputElement).value).toBe("Link A");
    expect((rows[0].get('[data-testid="footer-link-url"]').element as HTMLInputElement).value).toBe("https://a.example.com");
  });

  it("removes a footer link row", async () => {
    const wrapper = mountView();
    await flushPromises();
    await wrapper.get('[data-testid="footer-link-add"]').trigger("click");
    await flushPromises();
    expect(wrapper.findAll('[data-testid="footer-link-row"]')).toHaveLength(1);

    await wrapper.get('[data-testid="footer-link-remove"]').trigger("click");
    await flushPromises();
    expect(wrapper.findAll('[data-testid="footer-link-row"]')).toHaveLength(0);
  });

  it("moves a footer link up reorders the rows", async () => {
    const wrapper = mountView();
    await flushPromises();
    await wrapper.get('[data-testid="footer-link-add"]').trigger("click");
    await wrapper.get('[data-testid="footer-link-add"]').trigger("click");
    await flushPromises();

    let rows = wrapper.findAll('[data-testid="footer-link-row"]');
    expect(rows).toHaveLength(2);
    await rows[0].get('[data-testid="footer-link-name"]').setValue("First");
    await rows[1].get('[data-testid="footer-link-name"]').setValue("Second");

    // only the 2nd row (index 1) has an up button
    const upButtons = wrapper.findAll('[data-testid="footer-link-up"]');
    await upButtons[0].trigger("click");
    await flushPromises();

    rows = wrapper.findAll('[data-testid="footer-link-row"]');
    expect((rows[0].get('[data-testid="footer-link-name"]').element as HTMLInputElement).value).toBe("Second");
    expect((rows[1].get('[data-testid="footer-link-name"]').element as HTMLInputElement).value).toBe("First");
  });

  it("saves footer_links in payload (unconditional carry) when adding a link", async () => {
    const wrapper = mountView();
    await flushPromises();
    await wrapper.get('[data-testid="footer-link-add"]').trigger("click");
    await flushPromises();
    const row = wrapper.get('[data-testid="footer-link-row"]');
    await row.get('[data-testid="footer-link-name"]').setValue("My Link");
    await row.get('[data-testid="footer-link-url"]').setValue("https://example.com");

    await wrapper.find("form").trigger("submit.prevent");
    await flushPromises();

    expect(updateSettings).toHaveBeenCalled();
    const payload = updateSettings.mock.calls[0][0];
    expect(payload.footer_links).toBeDefined();
    expect(payload.footer_links).toHaveLength(1);
    expect(payload.footer_links[0].name).toBe("My Link");
    expect(payload.footer_links[0].url).toBe("https://example.com");
  });

  it("saves payload always includes footer_links even when footer links untouched", async () => {
    const wrapper = mountView();
    await flushPromises();

    await wrapper.find("form").trigger("submit.prevent");
    await flushPromises();

    const payload = updateSettings.mock.calls[0][0];
    expect(payload).toHaveProperty("footer_links");
  });

  it("backfills server-generated id after save (local row id non-empty)", async () => {
    const wrapper = mountView();
    await flushPromises();
    await wrapper.get('[data-testid="footer-link-add"]').trigger("click");
    await flushPromises();
    const row = wrapper.get('[data-testid="footer-link-row"]');
    await row.get('[data-testid="footer-link-name"]').setValue("My Link");
    await row.get('[data-testid="footer-link-url"]').setValue("https://example.com");

    // first save: id is empty at submit time (server generates it)
    await wrapper.find("form").trigger("submit.prevent");
    await flushPromises();
    const firstPayload = updateSettings.mock.calls[0][0];
    expect(firstPayload.footer_links[0].id).toBe("");

    // second save: backfilled id should be non-empty
    await wrapper.find("form").trigger("submit.prevent");
    await flushPromises();
    const secondPayload = updateSettings.mock.calls[1][0];
    expect(secondPayload.footer_links[0].id).not.toBe("");
    expect(secondPayload.footer_links[0].id).toBe("gen-0");
  });
});
