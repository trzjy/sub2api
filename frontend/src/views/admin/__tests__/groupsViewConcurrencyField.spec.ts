import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

import { describe, expect, it } from "vitest";

const currentDir = dirname(fileURLToPath(import.meta.url));

const groupsViewSource = readFileSync(
  resolve(currentDir, "../GroupsView.vue"),
  "utf8",
);

const zhOverview = readFileSync(
  resolve(currentDir, "../../../i18n/locales/zh/admin/overview.ts"),
  "utf8",
);
const enOverview = readFileSync(
  resolve(currentDir, "../../../i18n/locales/en/admin/overview.ts"),
  "utf8",
);

// E.5 / E.1 / E.2：分组表单的"订阅分组并发上限"输入项必须正确接线，且 i18n key 在 zh/en 均已定义。
describe("GroupsView subscription-group concurrency field", () => {
  it("binds the create-form concurrency input to createForm.concurrency as a number input", () => {
    expect(groupsViewSource).toContain('v-model.number="createForm.concurrency"');
    expect(groupsViewSource).toContain('type="number"');
    expect(groupsViewSource).toContain('min="0"');
  });

  it("binds the edit-form concurrency input to editForm.concurrency as a number input", () => {
    expect(groupsViewSource).toContain('v-model.number="editForm.concurrency"');
  });

  it("labels the field with the concurrency-limit i18n keys (label / placeholder / hint)", () => {
    expect(groupsViewSource).toContain('t("admin.groups.form.concurrencyLimit")');
    expect(groupsViewSource).toContain("t('admin.groups.form.concurrencyLimitPlaceholder')");
    expect(groupsViewSource).toContain('t("admin.groups.form.concurrencyLimitHint")');
  });

  it("defaults both create and edit forms to concurrency 0 (unlimited)", () => {
    const occurrences = groupsViewSource.match(/concurrency: 0 as number/g) ?? [];
    expect(occurrences.length).toBeGreaterThanOrEqual(2);
  });

  it("loads the edited group concurrency into the edit form", () => {
    expect(groupsViewSource).toContain("editForm.concurrency = group.concurrency ?? 0;");
  });

  it("defines the concurrency-limit i18n keys in both zh and en", () => {
    for (const key of [
      "concurrencyLimit",
      "concurrencyLimitPlaceholder",
      "concurrencyLimitHint",
    ]) {
      expect(zhOverview).toContain(`${key}:`);
      expect(enOverview).toContain(`${key}:`);
    }
  });
});
