export default {
  pricing: {
    title: 'Pricing Console',
    description: 'Price table sync, global catalog, uncovered model detection and the custom pricing layer',
    mtok: '1M tokens',
    actions: 'Actions',
    tabs: {
      status: 'Sync Status',
      catalog: 'Catalog',
      uncovered: 'Uncovered Models',
      custom: 'Custom Pricing',
      official: 'Official Prices'
    },
    columns: {
      model: 'Model',
      source: 'Source',
      requests: 'Requests'
    },
    source: {
      custom: 'Custom',
      remote: 'Remote Sync',
      builtin: 'Built-in Fallback',
      fuzzy: 'Series Fallback',
      none: 'No Price',
      channel: 'Channel',
      group: 'Group'
    },
    status: {
      syncTitle: 'Remote Price Table Sync',
      syncNow: 'Sync Now',
      syncDone: 'Sync completed',
      modelCount: 'Models',
      lastUpdated: 'Last Loaded',
      lastAttempt: 'Last Attempt',
      noError: 'No error',
      scheduler: 'Scheduler',
      customLayer: 'Custom Pricing Layer',
      customLayerHint: 'enabled / total',
      localHash: 'Local Hash',
      gapsTitle: 'Live Billing Gaps',
      gapsHint: 'Models billed with no price found ($0, revenue leak). In-process data, reset on restart.',
      noGaps: 'No gaps — every billed model has a price',
      addPrice: 'Add Price'
    },
    gaps: {
      count: 'Count',
      firstSeen: 'First Seen',
      lastSeen: 'Last Seen',
      addPrice: 'Add Price'
    },
    catalog: {
      search: 'Search models',
      searchPlaceholder: 'Model name keyword',
      source: 'Source filter',
      allSources: 'All sources',
      sourceHint: 'The catalog covers ALL models: global-layer entries (Custom > Remote > Built-in) plus effective price tiers for in-use/declared models (Series Fallback / No Price). Group/channel overrides live on their own pages.',
      imageOnly: 'image-only',
      override: 'Override',
      empty: 'No matching models'
    },
    preview: {
      title: 'Effective Price Preview',
      modelPlaceholder: 'Model name',
      run: 'Preview',
      input: 'Input',
      output: 'Output',
      cacheWrite: 'Cache Write',
      cacheRead: 'Cache Read',
      noPricingTitle: '{model} has no price — billed as $0',
      noPricingHint: 'No price layer matched (series fallback included). If this model is in use, review it on the Uncovered Models tab and add a price.'
    },
    uncovered: {
      windowDays: 'Usage window',
      days7: 'Last 7 days',
      days30: 'Last 30 days',
      days90: 'Last 90 days',
      hint: 'Multi-channel scan: group declarations + channel models + actual billed usage. Zero-traffic models must be declared in group model lists or channels to enter the scan.',
      verdict: 'Verdict',
      verdictUncovered: 'No price (billed $0)',
      verdictFuzzy: 'Fuzzy (nearby price)',
      references: 'References',
      usage: 'Window usage',
      requests: 'req',
      tokens: 'tokens',
      currentPrice: 'Current $in/$out (per 1M)',
      zeroCost: 'tokens with $0',
      allCovered: 'All exactly covered ({scanned} candidate models scanned)'
    },
    official: {
      hint: 'Official reference prices only feed the "Official Price" column and discount badges on the model plaza — billing is unaffected. Models without an override fall back to the billing catalog.',
      add: 'Add Official Price',
      addTitle: 'Add Official Reference Price',
      editTitle: 'Edit Official Reference Price',
      model: 'Model',
      unitHint: 'Unit: ¥ / 1M tokens (converted to $ at 7.15 on save). Leave blank to hide the item.',
      empty: 'No official price overrides yet',
      deleteTitle: 'Delete Official Reference Price',
      deleteMessage: 'Delete the official reference price for "{model}"? The model will fall back to the billing catalog price.'
    },
    officialTabPlaceholder: '',
    custom: {
      hint: 'The custom layer is global and sits between channel prices and the remote table; supports * wildcard suffix; remote sync never overwrites it.',
      add: 'Add Pricing',
      addTitle: 'Add Custom Pricing',
      editTitle: 'Edit Custom Pricing',
      models: 'Models',
      modelsPlaceholder: 'gpt-5.6-sol, my-alias-*',
      modelsHint: 'Comma-separated, * wildcard suffix supported',
      mode: 'Billing Mode',
      priceUnit: 'Unit: $/1M tokens (stored per-token). Leave blank to inherit lower layers (merge semantics).',
      perRequest: 'Per-request price',
      remark: 'Remark',
      enabled: 'Enabled',
      empty: 'No custom pricing yet',
      deleteTitle: 'Delete Custom Pricing',
      deleteMessage: 'Delete custom pricing for "{models}"? These models will fall back to the remote/built-in prices.'
    }
  }
}
