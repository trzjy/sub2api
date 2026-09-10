import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

import { describe, expect, it } from "vitest";

const currentDir = dirname(fileURLToPath(import.meta.url));

const settingsViewSource = readFileSync(
  resolve(currentDir, "../SettingsView.vue"),
  "utf8",
);

const zhSettings = readFileSync(
  resolve(currentDir, "../../../i18n/locales/zh/admin/settings.ts"),
  "utf8",
);
const enSettings = readFileSync(
  resolve(currentDir, "../../../i18n/locales/en/admin/settings.ts"),
  "utf8",
);

// E.5 / E.3：系统设置页的"并发超限提示文案"输入框必须正确接线，且 i18n key 在 zh/en 均已定义。
describe("SettingsView concurrency limit message field", () => {
  it("binds the textarea to form.concurrency_limit_message", () => {
    expect(settingsViewSource).toContain('v-model="form.concurrency_limit_message"');
  });

  it("labels the field with the concurrency-limit-message i18n keys (label / placeholder / hint)", () => {
    expect(settingsViewSource).toContain('t("admin.settings.site.concurrencyLimitMessage")');
    expect(settingsViewSource).toContain(
      "t('admin.settings.site.concurrencyLimitMessagePlaceholder')",
    );
    expect(settingsViewSource).toContain('t("admin.settings.site.concurrencyLimitMessageHint")');
  });

  it("defaults the field to an empty string and persists it on save", () => {
    expect(settingsViewSource).toContain("concurrency_limit_message: \"\",");
    expect(settingsViewSource).toContain("concurrency_limit_message: form.concurrency_limit_message,");
  });

  it("defines the concurrency-limit-message i18n keys in both zh and en", () => {
    for (const key of [
      "concurrencyLimitMessage",
      "concurrencyLimitMessagePlaceholder",
      "concurrencyLimitMessageHint",
    ]) {
      expect(zhSettings).toContain(`${key}:`);
      expect(enSettings).toContain(`${key}:`);
    }
  });
});
