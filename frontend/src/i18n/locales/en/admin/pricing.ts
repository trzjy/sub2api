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
      custom: 'Custom Pricing'
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
      lastSeen: 'Last Seen'
    },
    catalog: {
      search: 'Search models',
      searchPlaceholder: 'Model name keyword',
      source: 'Source filter',
      allSources: 'All sources',
      sourceHint: 'Layering: Custom > Remote Sync > Built-in. The catalog lists the global layers only; group/channel overrides live on their own pages.',
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
      cacheRead: 'Cache Read'
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
