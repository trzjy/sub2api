/**
 * Quota keys management page (Cursor activation codes) messages.
 *
 * Namespace shape matches accounts.ts / promoIntel.ts: the top-level
 * `quotaKeys` key is spread by admin/index.ts, so keys are `admin.quotaKeys.*`.
 */
export default {
  quotaKeys: {
    title: 'Quota Keys',
    description: 'Manage Cursor site-channel quota keys: create, edit, delete, manually refresh, clear exhausted and sweep expired.',
    create: 'Create Quota Key',
    createTitle: 'Create Quota Key',
    editTitle: 'Edit Quota Key',
    detailTitle: 'Quota Key Detail',
    columns: {
      maskedCode: 'Masked Code',
      platform: 'Platform',
      status: 'Status',
      exhausted: 'Exhausted',
      accountCount: 'Accounts',
      workingCount: 'Working',
      expiresAt: 'Expires At',
      lastRefreshError: 'Last Error',
      actions: 'Actions'
    },
    code: 'Activation Code',
    codePlaceholder: 'Enter the raw activation code (submitted once; the plaintext is never retained)',
    codeHint: 'The plaintext code is used for this submission only and never reaches local storage, logs or the address bar.',
    expiresAt: 'Expires At',
    expiresAtPlaceholder: 'Leave empty for no expiry',
    notes: 'Notes',
    notesPlaceholder: 'Optional notes',
    never: 'Never',
    na: '—',
    yes: 'Yes',
    no: 'No',
    status: {
      active: 'Active',
      exhausted: 'Exhausted',
      expired: 'Expired',
      inactive: 'Inactive',
      unknown: 'Unknown'
    },
    actions: {
      detail: 'Detail',
      edit: 'Edit',
      refresh: 'Refresh',
      clearExhausted: 'Clear Exhausted',
      delete: 'Delete'
    },
    sweepExpired: 'Sweep Expired',
    confirmDeleteTitle: 'Delete Quota Key',
    confirmSweepTitle: 'Sweep Expired Quota Keys',
    deleteConfirm: 'Its accounts will be soft-deleted together and cannot be restored. Continue?',
    sweepConfirm: 'All expired quota keys will be swept. This cannot be undone. Continue?',
    detail: {
      maskedCode: 'Masked Code',
      status: 'Status',
      exhausted: 'Exhausted',
      accountCount: 'Accounts',
      workingCount: 'Working',
      expiresAt: 'Expires At',
      lastRefreshAt: 'Last Refreshed At',
      lastRefreshError: 'Last Error',
      notes: 'Notes'
    },
    accountsTitle: 'Attached Accounts',
    noAccounts: 'No accounts attached to this quota key',
    expandAccounts: 'Expand account list',
    collapseAccounts: 'Collapse account list',
    emptyTitle: 'No quota keys yet',
    emptyDescription: 'Create the first Cursor quota key to manage refreshes and attached accounts here.',
    created: 'Quota key created',
    updated: 'Quota key updated',
    deleted: 'Quota key deleted',
    refreshed: 'Manual refresh triggered',
    cleared: 'Exhausted flag cleared',
    swept: 'Swept {count} expired quota keys',
    loadError: 'Failed to load quota keys',
    createError: 'Failed to create quota key',
    updateError: 'Failed to update quota key',
    deleteError: 'Failed to delete quota key',
    refreshError: 'Manual refresh failed',
    clearError: 'Failed to clear exhausted',
    sweepError: 'Failed to sweep expired quota keys',
    detailLoadError: 'Failed to load quota key detail'
  }
}
